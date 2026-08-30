// Package admin implements the localhost-only admin API used to approve or
// reject agent pairing codes. This surface is deliberately NOT part of the
// MCP protocol -- no MCP tool or resource may ever call into it, since an
// LLM client must never be able to approve a new device. See
// docs/specs/backend.md Section 12.2 and Section 12.4.
package admin

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"

	"github.com/CloudKeter/rc-mcp/internal/audit"
	"github.com/CloudKeter/rc-mcp/internal/devices"
)

// Notifier is the subset of *agent.Hub the admin API needs: pushing an
// approval/rejection result to whichever connection is waiting on a
// pairing code.
type Notifier interface {
	NotifyApproved(code string, device *devices.Device, token string) bool
	NotifyRejected(code string) bool
	// RevokeDevice cuts off a revoked device's live connection and expires
	// any dispatch state held for it (Section 12.2).
	RevokeDevice(deviceID string)
}

// API implements the admin HTTP surface: POST /admin/approve, POST
// /admin/reject, GET /admin/pending, GET /admin/devices, DELETE
// /admin/devices/{id}, GET /admin/audit, plus the admin web UI at "/".
type API struct {
	Registry devices.DeviceRegistry
	Hub      Notifier
	// AuditPath, if set, is the audit log GET /admin/audit reads from for
	// the web UI's audit log viewer. Empty disables that endpoint (it
	// returns an empty list rather than an error, matching the MCP
	// audit://log resource's behavior when unset).
	AuditPath string
}

// NewAPI constructs an admin API handler.
func NewAPI(registry devices.DeviceRegistry, hub Notifier) *API {
	return &API{Registry: registry, Hub: hub}
}

// Handler returns an http.Handler for the admin API routes, wrapped in a
// loopback-only guard as defense in depth (the primary control is binding
// ADMIN_BIND_ADDR to 127.0.0.1 in cmd/server/main.go).
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/approve", a.handleApprove)
	mux.HandleFunc("POST /admin/reject", a.handleReject)
	mux.HandleFunc("GET /admin/pending", a.handlePending)
	mux.HandleFunc("GET /admin/devices", a.handleListDevices)
	mux.HandleFunc("DELETE /admin/devices/{id}", a.handleRevoke)
	mux.HandleFunc("GET /admin/audit", a.handleAuditLog)
	mux.Handle("/", uiHandler())
	return loopbackOnly(mux)
}

// loopbackOnly rejects any request whose remote address is not loopback.
// This is a defense-in-depth check: the authoritative control is that
// ADMIN_BIND_ADDR only listens on 127.0.0.1, so no non-loopback connection
// can reach this handler in the first place under normal deployment.
func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r.RemoteAddr) {
			http.Error(w, "forbidden: admin API is loopback-only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr // remoteAddr may lack a port (e.g. in tests)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

type approveRequest struct {
	Code string `json:"code"`
}

type approveResponse struct {
	DeviceID string `json:"deviceId"`
	Label    string `json:"label"`
}

func (a *API) handleApprove(w http.ResponseWriter, r *http.Request) {
	var req approveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Code == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid request: expected {\"code\": \"XXXX-XXXX\"}")
		return
	}

	device, token, err := a.Registry.ApprovePairing(r.Context(), req.Code)
	if err != nil {
		writeJSONError(w, statusForPairingError(err), err.Error())
		return
	}

	if !a.Hub.NotifyApproved(req.Code, device, token) {
		log.Printf("admin: approved %s but no agent connection is waiting on it (it may have disconnected)", req.Code)
	}

	writeJSON(w, http.StatusOK, approveResponse{DeviceID: device.ID, Label: device.Label})
}

type rejectRequest struct {
	Code string `json:"code"`
}

func (a *API) handleReject(w http.ResponseWriter, r *http.Request) {
	var req rejectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Code == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid request: expected {\"code\": \"XXXX-XXXX\"}")
		return
	}

	if err := a.Registry.RejectPairing(r.Context(), req.Code); err != nil {
		writeJSONError(w, statusForPairingError(err), err.Error())
		return
	}

	a.Hub.NotifyRejected(req.Code)

	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handlePending(w http.ResponseWriter, r *http.Request) {
	codes, err := a.Registry.PendingPairingCodes(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if codes == nil {
		codes = []*devices.PairingCode{}
	}
	writeJSON(w, http.StatusOK, codes)
}

func statusForPairingError(err error) int {
	switch {
	case errors.Is(err, devices.ErrPairingCodeNotFound):
		return http.StatusNotFound
	case errors.Is(err, devices.ErrPairingCodeUsed), errors.Is(err, devices.ErrPairingCodeExpired):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

type revokeResponse struct {
	DeviceID string `json:"deviceId"`
	Revoked  bool   `json:"revoked"`
}

// handleRevoke implements DELETE /admin/devices/{id} (Section 12.2):
// removes the device from the registry so its token can never
// authenticate again, then tears down its live connection and any held
// dispatch state.
func (a *API) handleRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "device id is required")
		return
	}
	if err := a.Registry.Revoke(r.Context(), id); err != nil {
		if errors.Is(err, devices.ErrDeviceNotFound) {
			writeJSONError(w, http.StatusNotFound, "unknown device "+id)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.Hub.RevokeDevice(id)
	writeJSON(w, http.StatusOK, revokeResponse{DeviceID: id, Revoked: true})
}

// handleListDevices implements GET /admin/devices: the full paired-device
// list for the web UI's status dashboard (unlike the MCP clients://list
// resource, this is the admin-only surface and carries no restriction on
// what an operator may see).
func (a *API) handleListDevices(w http.ResponseWriter, r *http.Request) {
	all, err := a.Registry.List(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if all == nil {
		all = []*devices.Device{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": all})
}

// handleAuditLog implements GET /admin/audit?limit=&offset=: newest-first,
// offset-paginated audit log entries for the web UI's audit viewer. Mirrors
// the MCP audit://log resource's contract (internal/mcp/resources).
func (a *API) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeJSONError(w, http.StatusBadRequest, "invalid \"limit\"")
			return
		}
		limit = n
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSONError(w, http.StatusBadRequest, "invalid \"offset\"")
			return
		}
		offset = n
	}

	entries := []audit.Entry{}
	if a.AuditPath != "" {
		var err error
		entries, err = audit.ReadEntries(a.AuditPath, limit, offset)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "offset": offset, "limit": limit})
}
