package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CloudKeter/rc-mcp/internal/audit"
	"github.com/CloudKeter/rc-mcp/internal/devices"
)

// fakeNotifier records NotifyApproved/NotifyRejected calls without needing
// a real agent hub/WebSocket connection.
type fakeNotifier struct {
	revoked      []string
	approvedCode string
	rejectedCode string
	approveOK    bool
	rejectOK     bool
}

func (f *fakeNotifier) NotifyApproved(code string, _ *devices.Device, _ string) bool {
	f.approvedCode = code
	return f.approveOK
}

func (f *fakeNotifier) RevokeDevice(deviceID string) {
	f.revoked = append(f.revoked, deviceID)
}

func (f *fakeNotifier) NotifyRejected(code string) bool {
	f.rejectedCode = code
	return f.rejectOK
}

func newTestAPI(t *testing.T) (*API, *devices.FileRegistry, *fakeNotifier) {
	t.Helper()
	reg, err := devices.NewFileRegistry(t.TempDir() + "/devices.json")
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	n := &fakeNotifier{approveOK: true, rejectOK: true}
	return NewAPI(reg, n), reg, n
}

func loopbackRequest(method, target string, body []byte) *http.Request {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	return req
}

func TestApprove_ValidCode(t *testing.T) {
	api, reg, notifier := newTestAPI(t)
	handler := api.Handler()

	pc, err := reg.CreatePairingCode(context.Background(), "test-host")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}

	body, _ := json.Marshal(approveRequest{Code: pc.Code})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, loopbackRequest(http.MethodPost, "/admin/approve", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp approveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.DeviceID == "" {
		t.Fatal("expected non-empty deviceId")
	}
	if notifier.approvedCode != pc.Code {
		t.Errorf("notifier.approvedCode = %q, want %q", notifier.approvedCode, pc.Code)
	}
}

func TestApprove_SecondAttemptFails(t *testing.T) {
	api, reg, _ := newTestAPI(t)
	handler := api.Handler()

	pc, err := reg.CreatePairingCode(context.Background(), "test-host")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	body, _ := json.Marshal(approveRequest{Code: pc.Code})

	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, loopbackRequest(http.MethodPost, "/admin/approve", body))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first approve status = %d, want 200", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, loopbackRequest(http.MethodPost, "/admin/approve", body))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("second approve status = %d, want 409", rec2.Code)
	}
}

func TestReject_InvalidatesCode(t *testing.T) {
	api, reg, notifier := newTestAPI(t)
	handler := api.Handler()

	pc, err := reg.CreatePairingCode(context.Background(), "test-host")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	body, _ := json.Marshal(rejectRequest{Code: pc.Code})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, loopbackRequest(http.MethodPost, "/admin/reject", body))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reject status = %d, want 204", rec.Code)
	}
	if notifier.rejectedCode != pc.Code {
		t.Errorf("notifier.rejectedCode = %q, want %q", notifier.rejectedCode, pc.Code)
	}

	approveBody, _ := json.Marshal(approveRequest{Code: pc.Code})
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, loopbackRequest(http.MethodPost, "/admin/approve", approveBody))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("approve after reject status = %d, want 409", rec2.Code)
	}
}

func TestPending_ListsOnlyNonExpiredUnused(t *testing.T) {
	api, reg, _ := newTestAPI(t)
	handler := api.Handler()

	pc1, err := reg.CreatePairingCode(context.Background(), "host-1")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	pc2, err := reg.CreatePairingCode(context.Background(), "host-2")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	// Approve pc2 so it should no longer be pending.
	if _, _, err := reg.ApprovePairing(context.Background(), pc2.Code); err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, loopbackRequest(http.MethodGet, "/admin/pending", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var pending []devices.PairingCode
	if err := json.Unmarshal(rec.Body.Bytes(), &pending); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending code, got %d", len(pending))
	}
	if pending[0].Code != pc1.Code {
		t.Errorf("pending code = %q, want %q", pending[0].Code, pc1.Code)
	}
}

func TestLoopbackOnly_RejectsNonLoopback(t *testing.T) {
	api, _, _ := newTestAPI(t)
	handler := api.Handler()

	req := httptest.NewRequest(http.MethodGet, "/admin/pending", nil)
	req.RemoteAddr = "203.0.113.5:12345" // TEST-NET-3, non-loopback

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestLoopbackOnly_AllowsIPv6Loopback(t *testing.T) {
	api, _, _ := newTestAPI(t)
	handler := api.Handler()

	req := httptest.NewRequest(http.MethodGet, "/admin/pending", nil)
	req.RemoteAddr = "[::1]:12345"

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestRevoke_RemovesDeviceAndNotifiesHub(t *testing.T) {
	api, reg, notifier := newTestAPI(t)
	handler := api.Handler()

	pc, err := reg.CreatePairingCode(context.Background(), "revoke-host")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	device, token, err := reg.ApprovePairing(context.Background(), pc.Code)
	if err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, loopbackRequest(http.MethodDelete, "/admin/devices/"+device.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(notifier.revoked) != 1 || notifier.revoked[0] != device.ID {
		t.Fatalf("hub not notified of revocation: %+v", notifier.revoked)
	}
	if _, err := reg.Authenticate(context.Background(), token); err == nil {
		t.Fatal("revoked device token must no longer authenticate")
	}
	if _, err := reg.Get(context.Background(), device.ID); err == nil {
		t.Fatal("revoked device must be gone from the registry")
	}
}

func TestRevoke_UnknownDeviceIs404(t *testing.T) {
	api, _, notifier := newTestAPI(t)
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, loopbackRequest(http.MethodDelete, "/admin/devices/ghost", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if len(notifier.revoked) != 0 {
		t.Fatal("hub must not be notified for unknown devices")
	}
}

func TestListDevices_ReturnsPairedDevices(t *testing.T) {
	api, reg, _ := newTestAPI(t)
	handler := api.Handler()

	pc, err := reg.CreatePairingCode(context.Background(), "host-1")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	device, _, err := reg.ApprovePairing(context.Background(), pc.Code)
	if err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, loopbackRequest(http.MethodGet, "/admin/devices", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Devices []devices.Device `json:"devices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Devices) != 1 || out.Devices[0].ID != device.ID {
		t.Fatalf("devices = %+v", out.Devices)
	}
}

func TestListDevices_EmptyIsEmptyArrayNotNull(t *testing.T) {
	api, _, _ := newTestAPI(t)
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, loopbackRequest(http.MethodGet, "/admin/devices", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"devices":[]`)) {
		t.Fatalf("body = %s, want an empty array not null", rec.Body.String())
	}
}

func TestAuditLog_EmptyWhenPathUnset(t *testing.T) {
	api, _, _ := newTestAPI(t)
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, loopbackRequest(http.MethodGet, "/admin/audit", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"entries":[]`)) {
		t.Fatalf("body = %s, want empty entries when AuditPath is unset", rec.Body.String())
	}
}

func TestAuditLog_ReadsConfiguredPath(t *testing.T) {
	api, _, _ := newTestAPI(t)
	auditPath := t.TempDir() + "/audit.log"
	logger, err := audit.NewLogger(auditPath)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	if err := logger.LogCall("sess-1", "dev-1", "shell_exec", map[string]any{"command": "ls"}, audit.StatusOK, 0, ""); err != nil {
		t.Fatalf("LogCall: %v", err)
	}
	_ = logger.Close()
	api.AuditPath = auditPath

	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, loopbackRequest(http.MethodGet, "/admin/audit?limit=10&offset=0", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Entries []audit.Entry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Entries) != 1 || out.Entries[0].Tool != "shell_exec" {
		t.Fatalf("entries = %+v", out.Entries)
	}
}

func TestAuditLog_InvalidLimitOffset(t *testing.T) {
	api, _, _ := newTestAPI(t)
	handler := api.Handler()
	for _, q := range []string{"/admin/audit?limit=bad", "/admin/audit?limit=0", "/admin/audit?offset=-1"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, loopbackRequest(http.MethodGet, q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, rec.Code)
		}
	}
}

func TestUI_ServedAtRootAndLoopbackOnly(t *testing.T) {
	api, _, _ := newTestAPI(t)
	handler := api.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, loopbackRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("rc-mcp admin")) {
		t.Fatalf("body does not look like the admin UI: %s", rec.Body.String())
	}

	// Non-loopback requests are rejected the same way as the JSON API.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5:12345"
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a non-loopback UI request", rec.Code)
	}
}
