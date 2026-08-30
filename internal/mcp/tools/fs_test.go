package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CloudKeter/rc-mcp/internal/agent"
	"github.com/CloudKeter/rc-mcp/internal/audit"
	"github.com/CloudKeter/rc-mcp/internal/fsroot"
	"github.com/CloudKeter/rc-mcp/internal/protocol"
	"github.com/CloudKeter/rc-mcp/internal/session"
	"github.com/CloudKeter/rc-mcp/internal/transport"
)

func newFSTestDeps(t *testing.T, skipConfirm bool, dispatch *fakeDispatcher) FSDeps {
	t.Helper()
	auditLogger, err := audit.NewLogger(filepath.Join(t.TempDir(), "audit.log"))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { auditLogger.Close() })
	return FSDeps{Bridge: dispatch, Audit: auditLogger, SkipConfirm: skipConfirm, ElicitationTimeout: 2 * time.Second}
}

func TestFSRead_TruncatedAndListTruncatedTotalCount(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{
			Tool:   "fs_read",
			Output: map[string]any{"content": "0123", "encoding": "utf8", "size": 10, "truncated": true},
		}, nil
	}}
	deps := newFSTestDeps(t, true, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	args, _ := json.Marshal(map[string]any{"clientId": "dev-1", "path": "/tmp/x", "limit": 4})
	result, rpcErr := deps.handleRead(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	var out map[string]any
	_ = json.Unmarshal(result.StructuredContent, &out)
	if out["truncated"] != true {
		t.Fatalf("truncated = %v, want true", out["truncated"])
	}

	listDispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{
			Tool:   "fs_list",
			Output: map[string]any{"entries": []any{}, "truncated": true, "totalCount": 5},
		}, nil
	}}
	listDeps := newFSTestDeps(t, true, listDispatch)
	largs, _ := json.Marshal(map[string]any{"clientId": "dev-1", "path": "/tmp"})
	listResult, rpcErr := listDeps.handleList(context.Background(), sess, transport.ToolCallMeta{}, largs)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	var listOut map[string]any
	_ = json.Unmarshal(listResult.StructuredContent, &listOut)
	if listOut["totalCount"] != float64(5) {
		t.Fatalf("totalCount = %v, want 5", listOut["totalCount"])
	}
}

func TestFSRead_LargeFile_AssembledFromBinaryFrames(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		if onProgress != nil {
			onProgress(nil, &agent.BinaryFrame{
				Header: protocol.BinaryHeader{FrameType: protocol.FrameFileContent},
				Data:   []byte("hello "),
			})
			onProgress(nil, &agent.BinaryFrame{
				Header: protocol.BinaryHeader{FrameType: protocol.FrameFileContent},
				Data:   []byte("world"),
			})
		}
		return protocol.ResultPayload{
			Tool:   "fs_read",
			Output: map[string]any{"content": "", "encoding": "utf8", "size": 11, "truncated": false},
		}, nil
	}}
	deps := newFSTestDeps(t, true, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	args, _ := json.Marshal(map[string]any{"clientId": "dev-1", "path": "/tmp/big"})
	result, rpcErr := deps.handleRead(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	var out map[string]any
	_ = json.Unmarshal(result.StructuredContent, &out)
	if out["content"] != "hello world" {
		t.Fatalf("content = %v, want assembled from binary frames", out["content"])
	}
}

func TestFSWrite_OverwriteExisting_RequiresConfirmation(t *testing.T) {
	var writeCalled bool
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		if tool == "fs_stat" {
			// File exists.
			return protocol.ResultPayload{Tool: "fs_stat", Output: map[string]any{"name": "x", "path": "/tmp/x", "type": "file", "size": 1, "mode": "-rw-r--r--", "modTime": "now"}}, nil
		}
		writeCalled = true
		return protocol.ResultPayload{Tool: "fs_write", Output: map[string]any{"bytesWritten": 5, "path": "/tmp/x"}}, nil
	}}
	deps := newFSTestDeps(t, false, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	resultCh := make(chan *transport.ToolCallResult, 1)
	go func() {
		args, _ := json.Marshal(map[string]any{"clientId": "dev-1", "path": "/tmp/x", "content": "hello"})
		result, _ := deps.handleWrite(context.Background(), sess, transport.ToolCallMeta{}, args)
		resultCh <- result
	}()

	deliverElicitationResponse(t, sess, map[string]any{"action": "accept", "content": map[string]any{"confirm": true}})

	select {
	case result := <-resultCh:
		if result.IsError {
			t.Fatalf("result.IsError = true: %+v", result.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
	if !writeCalled {
		t.Fatal("expected fs_write dispatch after confirmation")
	}
}

func TestFSWrite_NewFile_NoConfirmationNeeded(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		if tool == "fs_stat" {
			return protocol.ResultPayload{Tool: "fs_stat", IsError: true, Error: "not found"}, nil
		}
		return protocol.ResultPayload{Tool: "fs_write", Output: map[string]any{"bytesWritten": 5, "path": "/tmp/new"}}, nil
	}}
	deps := newFSTestDeps(t, false, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	args, _ := json.Marshal(map[string]any{"clientId": "dev-1", "path": "/tmp/new", "content": "hello"})
	result, rpcErr := deps.handleWrite(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true: %+v", result.Content)
	}
}

func TestFSDelete_RequiresConfirmPathMatch(t *testing.T) {
	dispatchCalled := false
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		dispatchCalled = true
		return protocol.ResultPayload{Tool: "fs_delete", Output: map[string]any{"deleted": true, "path": "/tmp/x"}}, nil
	}}
	deps := newFSTestDeps(t, true, dispatch) // SkipConfirm is irrelevant for fs_delete: always confirms
	sess := session.New(context.Background(), "sess-1", 10)

	resultCh := make(chan *transport.ToolCallResult, 1)
	go func() {
		args, _ := json.Marshal(map[string]any{"clientId": "dev-1", "path": "/tmp/x"})
		result, _ := deps.handleDelete(context.Background(), sess, transport.ToolCallMeta{}, args)
		resultCh <- result
	}()

	// Wrong confirmPath: dispatch must not proceed.
	deliverElicitationResponse(t, sess, map[string]any{"action": "accept", "content": map[string]any{"confirmPath": "/tmp/wrong"}})

	select {
	case result := <-resultCh:
		if result.IsError {
			t.Fatalf("mismatch should be a declined result, not isError: %+v", result.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
	if dispatchCalled {
		t.Fatal("fs_delete should not dispatch when confirmPath does not match")
	}
}

func TestFSDelete_CorrectConfirmPath_Proceeds(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{Tool: "fs_delete", Output: map[string]any{"deleted": true, "path": "/tmp/x", "itemsRemoved": 1}}, nil
	}}
	deps := newFSTestDeps(t, true, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	resultCh := make(chan *transport.ToolCallResult, 1)
	go func() {
		args, _ := json.Marshal(map[string]any{"clientId": "dev-1", "path": "/tmp/x"})
		result, _ := deps.handleDelete(context.Background(), sess, transport.ToolCallMeta{}, args)
		resultCh <- result
	}()

	deliverElicitationResponse(t, sess, map[string]any{"action": "accept", "content": map[string]any{"confirmPath": "/tmp/x"}})

	select {
	case result := <-resultCh:
		if result.IsError {
			t.Fatalf("result.IsError = true: %+v", result.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}

func TestFSStat_OfflineDevice_ToolError(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{}, agent.ErrDeviceOffline
	}}
	deps := newFSTestDeps(t, true, dispatch)
	sess := session.New(context.Background(), "sess-1", 10)

	args, _ := json.Marshal(map[string]any{"clientId": "dev-1", "path": "/tmp/x"})
	result, rpcErr := deps.handleStat(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if !result.IsError {
		t.Fatal("expected isError=true for offline device")
	}
	if !strings.Contains(result.Content[0].Text, "offline") {
		t.Fatalf("error text = %q, want to mention offline", result.Content[0].Text)
	}
}

func TestFSRead_GlobalRootBlocksBeforeDispatch(t *testing.T) {
	dispatched := false
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		dispatched = true
		return protocol.ResultPayload{}, nil
	}}
	deps := newFSTestDeps(t, true, dispatch)
	pol, err := fsroot.New([]string{"/srv/data"})
	if err != nil {
		t.Fatalf("fsroot.New: %v", err)
	}
	deps.GlobalRoots = pol
	sess := session.New(context.Background(), "sess-1", 10)

	args, _ := json.Marshal(map[string]any{"clientId": "dev-1", "path": "/etc/shadow"})
	result, rpcErr := deps.handleRead(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if !result.IsError {
		t.Fatal("path outside the global root should be a tool error")
	}
	if dispatched {
		t.Fatal("a globally-blocked path must never reach the dispatcher")
	}
}

func TestFSRead_GlobalRootAllowsPathInsideRoot(t *testing.T) {
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{Tool: tool, Output: map[string]any{"content": "hi", "encoding": "utf8", "size": 2, "truncated": false}}, nil
	}}
	deps := newFSTestDeps(t, true, dispatch)
	pol, err := fsroot.New([]string{"/srv/data"})
	if err != nil {
		t.Fatalf("fsroot.New: %v", err)
	}
	deps.GlobalRoots = pol
	sess := session.New(context.Background(), "sess-1", 10)

	args, _ := json.Marshal(map[string]any{"clientId": "dev-1", "path": "/srv/data/file.txt"})
	result, rpcErr := deps.handleRead(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if result.IsError {
		t.Fatalf("path inside the global root should succeed: %+v", result.Content)
	}
}

func TestFSDelete_GlobalRootBlocksBeforeElicitation(t *testing.T) {
	dispatched := false
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		dispatched = true
		return protocol.ResultPayload{}, nil
	}}
	deps := newFSTestDeps(t, false, dispatch) // confirmation required, if we got that far
	pol, err := fsroot.New([]string{"/srv/data"})
	if err != nil {
		t.Fatalf("fsroot.New: %v", err)
	}
	deps.GlobalRoots = pol
	sess := session.New(context.Background(), "sess-1", 10)

	args, _ := json.Marshal(map[string]any{"clientId": "dev-1", "path": "/etc/passwd"})
	result, rpcErr := deps.handleDelete(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if !result.IsError {
		t.Fatal("path outside the global root should be a tool error")
	}
	if dispatched {
		t.Fatal("a globally-blocked delete must never reach the dispatcher (and never prompt elicitation)")
	}
}

func TestFSList_GlobalRootBlocksBeforeDispatch(t *testing.T) {
	dispatched := false
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		dispatched = true
		return protocol.ResultPayload{}, nil
	}}
	deps := newFSTestDeps(t, true, dispatch)
	pol, err := fsroot.New([]string{"/srv/data"})
	if err != nil {
		t.Fatalf("fsroot.New: %v", err)
	}
	deps.GlobalRoots = pol
	sess := session.New(context.Background(), "sess-1", 10)

	args, _ := json.Marshal(map[string]any{"clientId": "dev-1", "path": "/root"})
	result, rpcErr := deps.handleList(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if !result.IsError || dispatched {
		t.Fatalf("expected a blocked tool error with no dispatch, got isError=%v dispatched=%v", result.IsError, dispatched)
	}
}

func TestFSRead_RelativePathBypassesGlobalRootCheck(t *testing.T) {
	// Documented behavior: the global root policy only evaluates absolute
	// paths (the server has no notion of the target agent's cwd), so a
	// relative path is left entirely to the agent's own
	// AGENT_FS_ALLOWED_ROOTS.
	dispatch := &fakeDispatcher{fn: func(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error) {
		return protocol.ResultPayload{Tool: tool, Output: map[string]any{"content": "hi", "encoding": "utf8", "size": 2, "truncated": false}}, nil
	}}
	deps := newFSTestDeps(t, true, dispatch)
	pol, err := fsroot.New([]string{"/srv/data"})
	if err != nil {
		t.Fatalf("fsroot.New: %v", err)
	}
	deps.GlobalRoots = pol
	sess := session.New(context.Background(), "sess-1", 10)

	args, _ := json.Marshal(map[string]any{"clientId": "dev-1", "path": "relative/file.txt"})
	result, rpcErr := deps.handleRead(context.Background(), sess, transport.ToolCallMeta{}, args)
	if rpcErr != nil {
		t.Fatalf("unexpected rpcErr: %+v", rpcErr)
	}
	if result.IsError {
		t.Fatalf("relative path should pass the global check: %+v", result.Content)
	}
}
