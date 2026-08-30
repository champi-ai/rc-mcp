// Package agent implements the server side of the desktop-agent WebSocket
// connection lifecycle: accepting upgrades on GET /agent/ws, running the
// hello/pairing handshake, and tracking online/offline devices. See
// docs/specs/backend.md Section 2.1 and Section 12.2.
package agent

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/champi-ai/rc-mcp/internal/devices"
)

// approvalResult is delivered to a pending connection when the operator
// approves or rejects its pairing code via the admin API.
type approvalResult struct {
	approved bool
	device   *devices.Device
	token    string
}

// Hub accepts agent WebSocket connections, runs their handshake, and
// tracks which devices are currently online.
type Hub struct {
	Registry devices.DeviceRegistry

	// PairingCodeTTL bounds how long a connection will wait for operator
	// approval before it sends "pairing_expired" and closes.
	PairingCodeTTL time.Duration

	// OnDeviceChange, if set, is invoked (in its own goroutine) whenever a
	// device connects or disconnects, so resource/tool list-change
	// notifications can be pushed (Section 4.1). Set before serving.
	OnDeviceChange func(deviceID string)

	// OnLocalPresenceChange, if set, is invoked (in its own goroutine)
	// whenever a device connects or disconnects *on this replica*, with
	// online reflecting which. Used by the cross-replica dispatch bridge
	// (Section 10, Section 19) to publish/clear this replica's ownership
	// record for the device; unrelated to OnDeviceChange, which is about
	// resource/tool list-change notifications. Set before serving.
	OnLocalPresenceChange func(deviceID string, online bool)

	// ReconnectGracePeriod is how long in-flight dispatch state is held
	// after an agent disconnect (AGENT_RECONNECT_GRACE_PERIOD, Section
	// 2.1). Zero uses DefaultReconnectGracePeriod. Set before serving.
	ReconnectGracePeriod time.Duration

	// LatestAgentVersion, if set, is advertised in every hello_ack
	// (AGENT_LATEST_VERSION, Section 19: "Agent auto-update mechanism").
	// Empty means don't advertise a version at all. Set before serving.
	LatestAgentVersion string

	upgrader websocket.Upgrader

	// connWG tracks running connection goroutines so Shutdown can wait
	// for their teardown (registry writes included) to finish.
	connWG sync.WaitGroup

	mu      sync.Mutex
	online  map[string]*Connection         // deviceID -> connection
	pending map[string]chan approvalResult // pairing code -> waiting connection's channel
	states  map[string]*deviceState        // deviceID -> dispatch state (survives reconnects)
}

// NewHub constructs a Hub. pairingCodeTTL should match the registry's
// configured PAIRING_CODE_TTL.
func NewHub(registry devices.DeviceRegistry, pairingCodeTTL time.Duration) *Hub {
	return &Hub{
		Registry:       registry,
		PairingCodeTTL: pairingCodeTTL,
		upgrader: websocket.Upgrader{
			// Agents are not browsers; no Origin/CSRF concerns here (see
			// docs/specs/backend.md Section 12.3).
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		online:  map[string]*Connection{},
		pending: map[string]chan approvalResult{},
		states:  map[string]*deviceState{},
	}
}

// ServeHTTP upgrades GET /agent/ws requests and runs the connection.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("agent hub: upgrade failed: %v", err)
		return
	}

	c := newConnection(h, ws)
	h.connWG.Add(1)
	defer h.connWG.Done()
	c.run() // blocks until the connection closes
}

// AgentsOnline returns the number of currently online devices, for the
// GET /health endpoint.
func (h *Hub) AgentsOnline() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.online)
}

// OnlineDeviceIDs returns the IDs of every device currently connected to
// this replica, for the cross-replica dispatch bridge's location-record
// heartbeat (Section 10, Section 19).
func (h *Hub) OnlineDeviceIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.online))
	for id := range h.online {
		out = append(out, id)
	}
	return out
}

// Connection returns the active connection for deviceID, if it is
// currently online.
func (h *Hub) Connection(deviceID string) (*Connection, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.online[deviceID]
	return c, ok
}

// Shutdown sends a "close" message with the given reason to every
// currently connected agent and closes their WebSocket connections. Used
// by the server entry point during graceful shutdown
// (docs/specs/backend.md Section 8, "Graceful shutdown").
func (h *Hub) Shutdown(reason string) {
	h.mu.Lock()
	conns := make([]*Connection, 0, len(h.online))
	for _, c := range h.online {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	for _, c := range conns {
		c.sendCloseMessage(reason)
	}
	// Give the writer goroutines a brief moment to flush the queued
	// "close" envelopes before the raw WebSocket close frame follows.
	if len(conns) > 0 {
		time.Sleep(50 * time.Millisecond)
	}
	for _, c := range conns {
		c.closeWithReason(websocket.CloseGoingAway, reason)
	}
	// Wait for connection goroutines (including their teardown's registry
	// writes) to finish. Upgraded WebSockets are hijacked, so an HTTP
	// server's own graceful shutdown does not wait for them.
	h.connWG.Wait()
}

// state returns (creating if needed) the device's dispatch state.
func (h *Hub) state(deviceID string) *deviceState {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.states[deviceID]
	if !ok {
		st = newDeviceState()
		h.states[deviceID] = st
	}
	return st
}

func (h *Hub) gracePeriod() time.Duration {
	if h.ReconnectGracePeriod > 0 {
		return h.ReconnectGracePeriod
	}
	return DefaultReconnectGracePeriod
}

// registerOnline marks a device's connection as the active one for its
// device ID. Any previous connection for the same device ID is closed
// (a device can only have one live connection at a time). It returns true
// if the device reconnected while in-flight dispatch state was being held
// within the grace period (Section 2.1: hello_ack resume flag).
func (h *Hub) registerOnline(deviceID string, c *Connection) (resumed bool) {
	h.mu.Lock()
	prev := h.online[deviceID]
	h.online[deviceID] = c
	h.mu.Unlock()

	if prev != nil && prev != c {
		prev.closeWithReason(websocket.CloseNormalClosure, "superseded_by_new_connection")
	}
	resumed = h.state(deviceID).resume()
	h.notifyDeviceChange(deviceID)
	h.notifyLocalPresence(deviceID, true)
	return resumed
}

// unregisterOnline removes a device's connection if it is still the active
// one (a stale unregister from a superseded connection must not clobber a
// newer one).
func (h *Hub) unregisterOnline(deviceID string, c *Connection) {
	h.mu.Lock()
	changed := h.online[deviceID] == c
	if changed {
		delete(h.online, deviceID)
	}
	h.mu.Unlock()
	if changed {
		// Hold in-flight dispatch state for the reconnect grace period;
		// if the agent does not return in time, waiting bridges observe
		// ErrConnectionClosed and mark their work failed (Section 2.1).
		if st := h.state(deviceID); st.hasPending() {
			st.startGrace(h.gracePeriod())
		}
		h.notifyDeviceChange(deviceID)
		h.notifyLocalPresence(deviceID, false)
	}
}

// RevokeDevice cuts a device off immediately: its live connection (if any)
// receives a "revoked" close message, and any dispatch state held for it
// (in flight or within a reconnect grace period) is expired so waiting
// bridges fail now rather than at the grace deadline. The registry entry
// itself is removed by the caller (admin API) via DeviceRegistry.Revoke.
func (h *Hub) RevokeDevice(deviceID string) {
	h.mu.Lock()
	conn := h.online[deviceID]
	st := h.states[deviceID]
	delete(h.states, deviceID)
	h.mu.Unlock()

	if conn != nil {
		conn.sendCloseMessage("revoked")
		// Brief moment for the writer to flush the close envelope.
		time.Sleep(100 * time.Millisecond)
		conn.closeWithReason(websocket.ClosePolicyViolation, "revoked")
	}
	if st != nil {
		st.expire()
	}
	if conn == nil {
		// A live connection notifies via its own teardown; an offline
		// device still needs clients://list observers to hear about it.
		h.notifyDeviceChange(deviceID)
	}
}

func (h *Hub) notifyLocalPresence(deviceID string, online bool) {
	if h.OnLocalPresenceChange != nil {
		go h.OnLocalPresenceChange(deviceID, online)
	}
}

func (h *Hub) notifyDeviceChange(deviceID string) {
	if h.OnDeviceChange != nil {
		go h.OnDeviceChange(deviceID)
	}
}

// waitForApproval registers a channel that the admin API's Approve/Reject
// will deliver a result to for the given pairing code.
func (h *Hub) waitForApproval(code string) chan approvalResult {
	ch := make(chan approvalResult, 1)
	h.mu.Lock()
	h.pending[code] = ch
	h.mu.Unlock()
	return ch
}

func (h *Hub) stopWaitingForApproval(code string) {
	h.mu.Lock()
	delete(h.pending, code)
	h.mu.Unlock()
}

// NotifyApproved is called by the admin API after it approves a pairing
// code, delivering the new device and raw token to the waiting connection.
// Returns false if no connection is currently waiting on that code (e.g.
// it already expired and disconnected).
func (h *Hub) NotifyApproved(code string, device *devices.Device, token string) bool {
	h.mu.Lock()
	ch, ok := h.pending[code]
	if ok {
		delete(h.pending, code)
	}
	h.mu.Unlock()
	if !ok {
		return false
	}
	ch <- approvalResult{approved: true, device: device, token: token}
	return true
}

// NotifyRejected is called by the admin API after it rejects a pairing
// code, telling the waiting connection to send "pairing_rejected" and
// close.
func (h *Hub) NotifyRejected(code string) bool {
	h.mu.Lock()
	ch, ok := h.pending[code]
	if ok {
		delete(h.pending, code)
	}
	h.mu.Unlock()
	if !ok {
		return false
	}
	ch <- approvalResult{approved: false}
	return true
}
