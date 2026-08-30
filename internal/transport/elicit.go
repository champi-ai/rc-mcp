package transport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/champi-ai/rc-mcp/internal/session"
)

// DefaultElicitationTimeout is used when RequestElicitation is called with
// a non-positive timeout (RC_ELICITATION_TIMEOUT default: 120s). See
// docs/specs/backend.md Section 11.
const DefaultElicitationTimeout = 120 * time.Second

// ElicitationResult is the outcome of an elicitation/create round trip.
type ElicitationResult struct {
	// Declined is true whenever the tool should NOT proceed to dispatch:
	// the user declined, the request timed out, or it was cancelled.
	Declined bool
	// Reason explains a Declined result: "elicitation_timeout",
	// "declined", "cancelled", or "invalid_response".
	Reason string
	// Content is the accepted elicitation response payload (e.g.
	// {"confirm": true}) when Declined is false.
	Content json.RawMessage
}

// elicitationClientResponse is the "result" shape of a client's response
// to elicitation/create, per the MCP elicitation capability.
type elicitationClientResponse struct {
	Action  string          `json:"action"` // "accept", "decline", "cancel"
	Content json.RawMessage `json:"content,omitempty"`
}

// RequestElicitation sends an elicitation/create request to sess's MCP
// client over its SSE stream, with message and requestedSchema describing
// what confirmation is being asked for, and blocks until the client
// responds, ctx is cancelled (the originating tools/call received
// notifications/cancelled), the session closes, or timeout elapses.
//
// timeout <= 0 uses DefaultElicitationTimeout.
func RequestElicitation(ctx context.Context, sess *session.Session, message string, requestedSchema json.RawMessage, timeout time.Duration) ElicitationResult {
	if timeout <= 0 {
		timeout = DefaultElicitationTimeout
	}

	id, err := generateElicitationID()
	if err != nil {
		return ElicitationResult{Declined: true, Reason: "internal_error"}
	}

	respCh := sess.AwaitResponse(id)
	defer sess.CancelAwait(id)

	data, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "elicitation/create",
		"params": map[string]any{
			"message":         message,
			"requestedSchema": requestedSchema,
		},
	})
	if err != nil {
		return ElicitationResult{Declined: true, Reason: "internal_error"}
	}

	if !sess.Emit(session.SSEEvent{Data: string(data)}, DefaultEmitBackpressure) {
		return ElicitationResult{Declined: true, Reason: "elicitation_send_failed"}
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case raw := <-respCh:
		return parseElicitationResponse(raw)
	case <-timer.C:
		return ElicitationResult{Declined: true, Reason: "elicitation_timeout"}
	case <-ctx.Done():
		return ElicitationResult{Declined: true, Reason: "cancelled"}
	case <-sess.Ctx.Done():
		return ElicitationResult{Declined: true, Reason: "cancelled"}
	}
}

func parseElicitationResponse(raw json.RawMessage) ElicitationResult {
	var msg rpcRequest
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ElicitationResult{Declined: true, Reason: "invalid_response"}
	}
	if msg.Error != nil {
		return ElicitationResult{Declined: true, Reason: "declined"}
	}

	var payload elicitationClientResponse
	if len(msg.Result) > 0 {
		if err := json.Unmarshal(msg.Result, &payload); err != nil {
			return ElicitationResult{Declined: true, Reason: "invalid_response"}
		}
	}
	if payload.Action != "accept" {
		return ElicitationResult{Declined: true, Reason: "declined"}
	}
	return ElicitationResult{Declined: false, Content: payload.Content}
}

// DefaultEmitBackpressure mirrors internal/agent's session-emit
// backpressure window (Section 8), reused here so elicitation events
// obey the same "block up to 5s, then drop" policy as progress events.
const DefaultEmitBackpressure = 5 * time.Second

func generateElicitationID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("elicit-%s", hex.EncodeToString(b)), nil
}
