package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/CloudKeter/rc-mcp/internal/agent"
	"github.com/CloudKeter/rc-mcp/internal/audit"
	"github.com/CloudKeter/rc-mcp/internal/protocol"
	"github.com/CloudKeter/rc-mcp/internal/session"
	"github.com/CloudKeter/rc-mcp/internal/transport"
)

func newInputTestDeps(t *testing.T, dispatch *fakeDispatcher) InputDeps {
	t.Helper()
	auditLogger, err := audit.NewLogger(filepath.Join(t.TempDir(), "audit.log"))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { auditLogger.Close() })
	return InputDeps{Bridge: dispatch, Audit: auditLogger, ElicitationTimeout: 2 * time.Second}
}

func callInput(deps InputDeps, sess *session.Session, handler HandlerFunc, input map[string]any) (*transport.ToolCallResult, *transport.RPCError) {
	args, _ := json.Marshal(input)
	return handler(context.Background(), sess, transport.ToolCallMeta{}, args)
}

// TestInput_NoSkipConfirmField is a compile-time-adjacent guard: InputDeps
// must never grow a SkipConfirm-style bypass. This test exists so a future
// change adding one gets a clear, deliberate test failure to override
// rather than silently reintroducing a bypass for the highest-risk tool
// group.
func TestInput_MandatoryConfirmation_DeclinedBlocksDispatch(t *testing.T) {
	dispatched := false
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		dispatched = true
		return protocol.ResultPayload{}, nil
	}}
	deps := newInputTestDeps(t, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	resultCh := make(chan *transport.ToolCallResult, 1)
	go func() {
		result, _ := callInput(deps, sess, deps.handleKey, map[string]any{"clientId": "dev-1", "key": "Return"})
		resultCh <- result
	}()

	deliverElicitationResponse(t, sess, map[string]any{"action": "decline"})

	select {
	case result := <-resultCh:
		if result.IsError {
			t.Fatalf("declined result should not be isError: %+v", result.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for declined result")
	}
	if dispatched {
		t.Fatal("a declined input action must never reach the dispatcher")
	}
}

func TestInput_MandatoryConfirmation_ConfirmedProceeds(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		if tool != "input_key" {
			t.Errorf("tool = %q, want input_key", tool)
		}
		return protocol.ResultPayload{Tool: tool, Output: map[string]any{}}, nil
	}}
	deps := newInputTestDeps(t, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	resultCh := make(chan *transport.ToolCallResult, 1)
	go func() {
		result, _ := callInput(deps, sess, deps.handleKey, map[string]any{"clientId": "dev-1", "key": "Return"})
		resultCh <- result
	}()

	deliverElicitationResponse(t, sess, map[string]any{"confirm": true})

	select {
	case result := <-resultCh:
		if result.IsError {
			t.Fatalf("confirmed action should succeed: %+v", result.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result")
	}
}

func TestInput_ElicitationTimeout_TreatedAsDeclined(t *testing.T) {
	dispatched := false
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		dispatched = true
		return protocol.ResultPayload{}, nil
	}}
	deps := newInputTestDeps(t, dispatch)
	deps.ElicitationTimeout = 50 * time.Millisecond
	sess := session.New(context.Background(), "sess-1", 10)

	result, _ := callInput(deps, sess, deps.handleMouseMove, map[string]any{"clientId": "dev-1", "x": 1, "y": 2})
	if result.IsError {
		t.Fatalf("a timed-out elicitation should be a declined (not tool-error) result: %+v", result.Content)
	}
	if dispatched {
		t.Fatal("an unconfirmed action (timeout) must never reach the dispatcher")
	}
}

func TestInputMouseClick_MissingClientID(t *testing.T) {
	deps := newInputTestDeps(t, &fakeDispatcher{})
	sess := session.New(context.Background(), "sess-1", 10)
	_, rpcErr := callInput(deps, sess, deps.handleMouseClick, map[string]any{"x": 1, "y": 2})
	if rpcErr == nil || rpcErr.Code != -32602 {
		t.Fatalf("rpcErr = %+v, want -32602", rpcErr)
	}
}

func TestInputType_MissingClientID(t *testing.T) {
	deps := newInputTestDeps(t, &fakeDispatcher{})
	sess := session.New(context.Background(), "sess-1", 10)
	_, rpcErr := callInput(deps, sess, deps.handleType, map[string]any{"text": "hi"})
	if rpcErr == nil || rpcErr.Code != -32602 {
		t.Fatalf("rpcErr = %+v, want -32602", rpcErr)
	}
}

func TestInputKey_MissingKey(t *testing.T) {
	deps := newInputTestDeps(t, &fakeDispatcher{})
	sess := session.New(context.Background(), "sess-1", 10)
	_, rpcErr := callInput(deps, sess, deps.handleKey, map[string]any{"clientId": "dev-1", "key": ""})
	if rpcErr == nil || rpcErr.Code != -32602 {
		t.Fatalf("rpcErr = %+v, want -32602", rpcErr)
	}
}

// TestInput_RegistryGating_NotListedWithoutCapability exercises the
// acceptance criterion that the input tools only appear once a device has
// the "input" capability enabled -- reusing the registry's existing
// capability-gating machinery (Section 2), same as every other tool group.
func TestInput_RegistryGating_NotListedWithoutCapability(t *testing.T) {
	reg, deviceReg := newTestRegistry(t)
	reg.Register(NewInputKeyDefinition(newInputTestDeps(t, &fakeDispatcher{})))
	_ = pairOnlineDevice(t, deviceReg, []string{"shell"}) // no "input" capability

	list := reg.ListTools(context.Background())
	for _, tool := range list {
		if tool.Name == "input_key" {
			t.Fatal("input_key must not be listed when no device has the input capability enabled")
		}
	}

	id := pairOnlineDevice(t, deviceReg, []string{"input"})
	_ = id
	list = reg.ListTools(context.Background())
	found := false
	for _, tool := range list {
		if tool.Name == "input_key" {
			found = true
		}
	}
	if !found {
		t.Fatal("input_key should be listed once a device has the input capability enabled")
	}
}
