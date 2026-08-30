// Package resources implements the five MCP resources from
// docs/specs/backend.md Section 4: clients://list, job://{id},
// sysinfo://{clientId}/overview, audit://log, and shell://sessions —
// read-only status surfaces with per-session subscriptions and pushed
// notifications/resources/updated events.
package resources

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CloudKeter/rc-mcp/internal/agent"
	"github.com/CloudKeter/rc-mcp/internal/audit"
	"github.com/CloudKeter/rc-mcp/internal/devices"
	"github.com/CloudKeter/rc-mcp/internal/jobs"
	"github.com/CloudKeter/rc-mcp/internal/protocol"
	"github.com/CloudKeter/rc-mcp/internal/session"
	"github.com/CloudKeter/rc-mcp/internal/transport"
)

// SysinfoRefreshInterval is how often a subscribed sysinfo overview pushes
// an update notification while its device is online (Section 4.3).
const SysinfoRefreshInterval = 30 * time.Second

// emitBackpressure matches the Section 8 policy used elsewhere.
const emitBackpressure = 5 * time.Second

// Dispatcher is the subset of *agent.Bridge the sysinfo resource needs,
// injectable for tests (same shape as tools.ShellDispatcher).
type Dispatcher interface {
	Dispatch(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error)
}

// Registry implements transport.ResourceRegistry.
type Registry struct {
	Devices  devices.DeviceRegistry
	Jobs     jobs.JobStore
	Bridge   Dispatcher
	Sessions session.SessionStore
	// AuditPath is the audit log file read by audit://log. Empty disables
	// the resource's content (it still lists, reads return empty).
	AuditPath string
	// RefreshInterval overrides SysinfoRefreshInterval (tests). Zero uses
	// the default.
	RefreshInterval time.Duration

	mu         sync.Mutex
	refreshers map[string]context.CancelFunc // sessionID + "\x00" + uri -> stop refresher
}

// NewRegistry constructs a resource Registry.
func NewRegistry(deviceRegistry devices.DeviceRegistry, jobStore jobs.JobStore, bridge Dispatcher, sessions session.SessionStore, auditPath string) *Registry {
	return &Registry{
		Devices:    deviceRegistry,
		Jobs:       jobStore,
		Bridge:     bridge,
		Sessions:   sessions,
		AuditPath:  auditPath,
		refreshers: map[string]context.CancelFunc{},
	}
}

// ListResources implements transport.ResourceRegistry.
func (r *Registry) ListResources(ctx context.Context) []transport.ResourceDescriptor {
	return []transport.ResourceDescriptor{
		{URI: "clients://list", Name: "Paired Devices", Description: "Read-only list of all paired devices: online status, capabilities, last seen. Not a control surface.", MimeType: "application/json"},
		{URI: "job://{id}", Name: "Job Status", Description: "Status and result of a long-running job (e.g. screenshot_watch).", MimeType: "application/json"},
		{URI: "sysinfo://{clientId}/overview", Name: "System Overview", Description: "Live system overview (CPU/mem/disk/load) from a specific device.", MimeType: "application/json"},
		{URI: "audit://log", Name: "Audit Log", Description: "Server-side append-only audit log, newest-first. Paginated via ?limit=&offset=.", MimeType: "application/json"},
		{URI: "shell://sessions", Name: "Shell Sessions", Description: "Active interactive shell sessions in the current MCP session.", MimeType: "application/json"},
	}
}

// ReadResource implements transport.ResourceRegistry.
func (r *Registry) ReadResource(ctx context.Context, sess *session.Session, uri string) ([]transport.ResourceContent, *transport.RPCError) {
	switch {
	case uri == "clients://list":
		return r.readClients(ctx, uri)
	case strings.HasPrefix(uri, "job://"):
		return r.readJob(sess, uri)
	case strings.HasPrefix(uri, "sysinfo://"):
		return r.readSysinfo(ctx, sess, uri)
	case uri == "audit://log" || strings.HasPrefix(uri, "audit://log?"):
		return r.readAudit(uri)
	case uri == "shell://sessions":
		return r.readShellSessions(sess, uri)
	default:
		return nil, &transport.RPCError{Code: -32602, Message: fmt.Sprintf("unknown resource %q", uri)}
	}
}

// SubscribeResource implements transport.ResourceRegistry.
func (r *Registry) SubscribeResource(ctx context.Context, sess *session.Session, uri string) *transport.RPCError {
	if rpcErr := r.validateURI(uri); rpcErr != nil {
		return rpcErr
	}
	sess.Subscribe(uri)
	if clientID, ok := sysinfoClientID(uri); ok {
		r.startSysinfoRefresher(sess, uri, clientID)
	}
	return nil
}

// UnsubscribeResource implements transport.ResourceRegistry.
func (r *Registry) UnsubscribeResource(ctx context.Context, sess *session.Session, uri string) *transport.RPCError {
	sess.Unsubscribe(uri)
	r.stopSysinfoRefresher(sess.ID, uri)
	return nil
}

// validateURI accepts any URI ReadResource can serve.
func (r *Registry) validateURI(uri string) *transport.RPCError {
	switch {
	case uri == "clients://list", uri == "shell://sessions",
		uri == "audit://log", strings.HasPrefix(uri, "audit://log?"),
		strings.HasPrefix(uri, "job://"):
		return nil
	}
	if _, ok := sysinfoClientID(uri); ok {
		return nil
	}
	return &transport.RPCError{Code: -32602, Message: fmt.Sprintf("unknown resource %q", uri)}
}

// --- clients://list (4.1) ---

type clientEntry struct {
	ClientID     string    `json:"clientId"`
	Label        string    `json:"label"`
	Online       bool      `json:"online"`
	Capabilities []string  `json:"capabilities"`
	LastSeen     time.Time `json:"lastSeen"`
	PairedAt     time.Time `json:"pairedAt"`
}

func (r *Registry) readClients(ctx context.Context, uri string) ([]transport.ResourceContent, *transport.RPCError) {
	all, err := r.Devices.List(ctx)
	if err != nil {
		return nil, &transport.RPCError{Code: -32603, Message: "failed to list devices"}
	}
	clients := make([]clientEntry, 0, len(all))
	for _, d := range all {
		caps := d.Capabilities
		if caps == nil {
			caps = []string{}
		}
		clients = append(clients, clientEntry{
			ClientID:     d.ID,
			Label:        d.Label,
			Online:       d.Online,
			Capabilities: caps,
			LastSeen:     d.LastSeen,
			PairedAt:     d.PairedAt,
		})
	}
	return jsonContent(uri, map[string]any{"clients": clients})
}

// --- job://{id} (4.2) ---

func (r *Registry) readJob(sess *session.Session, uri string) ([]transport.ResourceContent, *transport.RPCError) {
	id := strings.TrimPrefix(uri, "job://")
	if id == "" || strings.Contains(id, "/") {
		return nil, &transport.RPCError{Code: -32602, Message: fmt.Sprintf("unknown resource %q", uri)}
	}
	job, err := r.Jobs.Get(id)
	if err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			return nil, &transport.RPCError{Code: -32602, Message: fmt.Sprintf("unknown job %q", id)}
		}
		return nil, &transport.RPCError{Code: -32603, Message: "failed to read job"}
	}
	// Jobs are session-scoped status: only the owning session may read
	// them (they can carry tool output).
	if job.SessionID != sess.ID {
		return nil, &transport.RPCError{Code: -32602, Message: fmt.Sprintf("unknown job %q", id)}
	}
	return jsonContent(uri, job)
}

// --- sysinfo://{clientId}/overview (4.3) ---

func sysinfoClientID(uri string) (string, bool) {
	rest, ok := strings.CutPrefix(uri, "sysinfo://")
	if !ok {
		return "", false
	}
	clientID, ok := strings.CutSuffix(rest, "/overview")
	if !ok || clientID == "" || strings.Contains(clientID, "/") {
		return "", false
	}
	return clientID, true
}

func (r *Registry) readSysinfo(ctx context.Context, sess *session.Session, uri string) ([]transport.ResourceContent, *transport.RPCError) {
	clientID, ok := sysinfoClientID(uri)
	if !ok {
		return nil, &transport.RPCError{Code: -32602, Message: fmt.Sprintf("unknown resource %q", uri)}
	}
	device, err := r.Devices.Get(ctx, clientID)
	if err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: fmt.Sprintf("Unknown device %s", clientID)}
	}
	if !device.Online {
		return nil, &transport.RPCError{Code: -32603, Message: fmt.Sprintf("Device %s is offline", clientID)}
	}

	correlationID, err := newCorrelationID()
	if err != nil {
		return nil, &transport.RPCError{Code: -32603, Message: "internal error"}
	}
	input := map[string]any{"clientId": clientID, "sections": []string{"all"}}
	result, err := r.Bridge.Dispatch(ctx, clientID, correlationID, "sysinfo_get", sess.ID, input, nil)
	if err != nil {
		return nil, &transport.RPCError{Code: -32603, Message: fmt.Sprintf("sysinfo read failed: %v", err)}
	}
	if result.IsError {
		return nil, &transport.RPCError{Code: -32603, Message: result.Error}
	}
	return jsonContent(uri, result.Output)
}

// --- audit://log (4.4) ---

func (r *Registry) readAudit(uri string) ([]transport.ResourceContent, *transport.RPCError) {
	limit, offset := 100, 0
	if q := strings.SplitN(uri, "?", 2); len(q) == 2 {
		values, err := url.ParseQuery(q[1])
		if err != nil {
			return nil, &transport.RPCError{Code: -32602, Message: "invalid audit://log query"}
		}
		if v := values.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return nil, &transport.RPCError{Code: -32602, Message: "invalid \"limit\""}
			}
			limit = n
		}
		if v := values.Get("offset"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return nil, &transport.RPCError{Code: -32602, Message: "invalid \"offset\""}
			}
			offset = n
		}
	}

	entries := []audit.Entry{}
	if r.AuditPath != "" {
		var err error
		entries, err = audit.ReadEntries(r.AuditPath, limit, offset)
		if err != nil {
			return nil, &transport.RPCError{Code: -32603, Message: "failed to read audit log"}
		}
	}
	return jsonContent(uri, map[string]any{"entries": entries, "offset": offset, "limit": limit})
}

// --- shell://sessions (4.5) ---

type shellSessionEntry struct {
	ShellSessionID string    `json:"shellSessionId"`
	ClientID       string    `json:"clientId"`
	PID            int       `json:"pid"`
	Shell          string    `json:"shell"`
	CreatedAt      time.Time `json:"createdAt"`
}

func (r *Registry) readShellSessions(sess *session.Session, uri string) ([]transport.ResourceContent, *transport.RPCError) {
	entries := sess.ListShellSessions()
	out := make([]shellSessionEntry, 0, len(entries))
	for id, e := range entries {
		out = append(out, shellSessionEntry{
			ShellSessionID: id,
			ClientID:       e.ClientID,
			PID:            e.PID,
			Shell:          e.Shell,
			CreatedAt:      e.CreatedAt,
		})
	}
	return jsonContent(uri, map[string]any{"sessions": out})
}

// --- notifications ---

// NotifySessionUpdated pushes notifications/resources/updated for uri to
// one session, if it is subscribed.
func (r *Registry) NotifySessionUpdated(sess *session.Session, uri string) {
	if sess == nil || !sess.IsSubscribed(uri) {
		return
	}
	emitNotification(sess, "notifications/resources/updated", map[string]any{"uri": uri})
}

// BroadcastUpdated pushes notifications/resources/updated for uri to every
// subscribed session.
func (r *Registry) BroadcastUpdated(uri string) {
	if r.Sessions == nil {
		return
	}
	r.Sessions.Range(func(sess *session.Session) bool {
		r.NotifySessionUpdated(sess, uri)
		return true
	})
}

// BroadcastListChanged pushes a parameterless list-changed notification
// (e.g. notifications/tools/list_changed) to every session; list-changed
// events are not subscription-gated.
func (r *Registry) BroadcastListChanged(method string) {
	if r.Sessions == nil {
		return
	}
	r.Sessions.Range(func(sess *session.Session) bool {
		emitNotification(sess, method, map[string]any{})
		return true
	})
}

// NotifyJobUpdated routes a job store update to its owning session's
// job://{id} subscription. Wire as jobs.MemoryStore.OnUpdate.
func (r *Registry) NotifyJobUpdated(job *jobs.Job) {
	if r.Sessions == nil || job == nil {
		return
	}
	sess, err := r.Sessions.Get(context.Background(), job.SessionID)
	if err != nil {
		return
	}
	r.NotifySessionUpdated(sess, "job://"+job.ID)
}

// NotifyDeviceChange handles an agent connect/disconnect: subscribers of
// clients://list get an updated notification, and every session learns the
// tools/list may have changed (online capability union changed). Wire as
// agent.Hub.OnDeviceChange.
func (r *Registry) NotifyDeviceChange(deviceID string) {
	r.BroadcastUpdated("clients://list")
	r.BroadcastListChanged("notifications/tools/list_changed")
}

// NotifyAuditEntry handles a new audit log entry. Wire as
// audit.Logger.OnWrite.
func (r *Registry) NotifyAuditEntry(audit.Entry) {
	r.BroadcastUpdated("audit://log")
}

// --- sysinfo refreshers ---

func refresherKey(sessionID, uri string) string { return sessionID + "\x00" + uri }

func (r *Registry) startSysinfoRefresher(sess *session.Session, uri, clientID string) {
	key := refresherKey(sess.ID, uri)
	r.mu.Lock()
	if _, exists := r.refreshers[key]; exists {
		r.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(sess.Ctx)
	r.refreshers[key] = cancel
	r.mu.Unlock()

	interval := r.RefreshInterval
	if interval <= 0 {
		interval = SysinfoRefreshInterval
	}

	go func() {
		defer r.stopSysinfoRefresher(sess.ID, uri)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !sess.IsSubscribed(uri) {
					return
				}
				device, err := r.Devices.Get(ctx, clientID)
				if err != nil || !device.Online {
					continue // paused while offline; resumes if it comes back
				}
				r.NotifySessionUpdated(sess, uri)
			}
		}
	}()
}

func (r *Registry) stopSysinfoRefresher(sessionID, uri string) {
	key := refresherKey(sessionID, uri)
	r.mu.Lock()
	cancel, ok := r.refreshers[key]
	if ok {
		delete(r.refreshers, key)
	}
	r.mu.Unlock()
	if ok {
		cancel()
	}
}

// --- helpers ---

func jsonContent(uri string, v any) ([]transport.ResourceContent, *transport.RPCError) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, &transport.RPCError{Code: -32603, Message: "failed to encode resource"}
	}
	return []transport.ResourceContent{{URI: uri, MimeType: "application/json", Text: string(data)}}, nil
}

func emitNotification(sess *session.Session, method string, params any) {
	data, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return
	}
	if !sess.Emit(session.SSEEvent{Data: string(data)}, emitBackpressure) {
		log.Printf("session %s: dropped %s notification (EventCh full)", sess.ID, method)
	}
}

func newCorrelationID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

var _ transport.ResourceRegistry = (*Registry)(nil)
