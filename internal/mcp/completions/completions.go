// Package completions implements the "completions" MCP capability from
// docs/specs/backend.md Section 2: argument auto-completion for tool
// inputs, most usefully clientId (sourced from the device registry) and
// filesystem paths (dispatched to the target agent's fs_list).
package completions

import (
	"context"
	"crypto/rand"
	"fmt"
	"path"
	"strings"

	"github.com/champi-ai/rc-mcp/internal/agent"
	"github.com/champi-ai/rc-mcp/internal/devices"
	"github.com/champi-ai/rc-mcp/internal/protocol"
	"github.com/champi-ai/rc-mcp/internal/session"
	"github.com/champi-ai/rc-mcp/internal/transport"
)

// maxResults caps how many completion values a single call returns,
// matching typical MCP client-side completion menu sizes.
const maxResults = 100

// Dispatcher is the subset of *agent.Bridge fs path completion needs (same
// shape as tools.ShellDispatcher, injectable for tests).
type Dispatcher interface {
	Dispatch(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error)
}

// fsTools is the set of tool names whose "path" argument completes via
// fs_list against the target device.
var fsTools = map[string]bool{
	"fs_read": true, "fs_write": true, "fs_list": true, "fs_delete": true, "fs_stat": true,
}

// clientIDTools is the set of tool names with a "clientId" argument that
// completes from the device registry. Kept as an explicit allowlist
// (rather than "any tool") so completion never implies a tool exists.
var clientIDTools = map[string]bool{
	"shell_exec": true, "shell_session_start": true,
	"fs_read": true, "fs_write": true, "fs_list": true, "fs_delete": true, "fs_stat": true,
	"process_list": true, "process_info": true, "process_signal": true,
	"sysinfo_get":        true,
	"screenshot_capture": true,
	"screenshot_watch":   true,
}

// Registry implements transport.CompletionRegistry.
type Registry struct {
	Devices devices.DeviceRegistry
	Bridge  Dispatcher
}

// NewRegistry constructs a completions Registry.
func NewRegistry(deviceRegistry devices.DeviceRegistry, bridge Dispatcher) *Registry {
	return &Registry{Devices: deviceRegistry, Bridge: bridge}
}

// Complete implements transport.CompletionRegistry.
func (r *Registry) Complete(ctx context.Context, sess *session.Session, ref transport.CompletionRef, arg transport.CompletionArgument, compCtx transport.CompletionContext) (*transport.CompletionValues, *transport.RPCError) {
	switch ref.Type {
	case "ref/tool":
		return r.completeTool(ctx, sess, ref.Name, arg, compCtx)
	case "ref/resource":
		return r.completeResource(ctx, sess, ref.URI, arg, compCtx)
	default:
		// Prompts and unrecognized ref kinds have no completion source
		// yet; an empty (not error) result matches Section 2's "degrade
		// gracefully" guidance for anything not resolvable.
		return &transport.CompletionValues{Values: []string{}}, nil
	}
}

func (r *Registry) completeTool(ctx context.Context, sess *session.Session, tool string, arg transport.CompletionArgument, compCtx transport.CompletionContext) (*transport.CompletionValues, *transport.RPCError) {
	switch arg.Name {
	case "clientId":
		if !clientIDTools[tool] {
			return &transport.CompletionValues{Values: []string{}}, nil
		}
		return r.completeClientID(ctx, arg.Value)
	case "path":
		if !fsTools[tool] {
			return &transport.CompletionValues{Values: []string{}}, nil
		}
		return r.completePath(ctx, sess, compCtx.Arguments["clientId"], arg.Value)
	default:
		return &transport.CompletionValues{Values: []string{}}, nil
	}
}

// completeResource handles the sysinfo://{clientId}/overview and
// job://{id} URI templates' clientId-shaped segment. Only sysinfo's
// {clientId} is a meaningful completion target; job IDs are opaque.
func (r *Registry) completeResource(ctx context.Context, sess *session.Session, uri string, arg transport.CompletionArgument, compCtx transport.CompletionContext) (*transport.CompletionValues, *transport.RPCError) {
	if uri == "sysinfo://{clientId}/overview" && arg.Name == "clientId" {
		return r.completeClientID(ctx, arg.Value)
	}
	return &transport.CompletionValues{Values: []string{}}, nil
}

// completeClientID lists paired device IDs (online or offline, per Section
// 2's implementation note) whose ID or label starts with prefix.
func (r *Registry) completeClientID(ctx context.Context, prefix string) (*transport.CompletionValues, *transport.RPCError) {
	all, err := r.Devices.List(ctx)
	if err != nil {
		return &transport.CompletionValues{Values: []string{}}, nil
	}
	var values []string
	for _, d := range all {
		if matchesPrefix(d.ID, prefix) || matchesPrefix(d.Label, prefix) {
			values = append(values, d.ID)
		}
	}
	return capValues(values), nil
}

// completePath dispatches fs_list to clientID for the directory portion of
// value and returns entries whose name starts with its basename portion.
// An offline or unspecified device, or a dispatch failure, degrades to an
// empty result rather than an error (Section 2, acceptance criteria).
func (r *Registry) completePath(ctx context.Context, sess *session.Session, clientID, value string) (*transport.CompletionValues, *transport.RPCError) {
	if clientID == "" {
		return &transport.CompletionValues{Values: []string{}}, nil
	}
	device, err := r.Devices.Get(ctx, clientID)
	if err != nil || !device.Online {
		return &transport.CompletionValues{Values: []string{}}, nil
	}

	dir, prefix := splitPathPrefix(value)
	correlationID, err := newCorrelationID()
	if err != nil {
		return &transport.CompletionValues{Values: []string{}}, nil
	}
	input := map[string]any{"clientId": clientID, "path": dir, "limit": 500}
	result, dispatchErr := r.Bridge.Dispatch(ctx, clientID, correlationID, "fs_list", sess.ID, input, nil)
	if dispatchErr != nil || result.IsError {
		return &transport.CompletionValues{Values: []string{}}, nil
	}

	entries, ok := result.Output.(map[string]any)
	if !ok {
		return &transport.CompletionValues{Values: []string{}}, nil
	}
	rawEntries, _ := entries["entries"].([]any)
	var values []string
	for _, e := range rawEntries {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		name, _ := entry["name"].(string)
		if !matchesPrefix(name, prefix) {
			continue
		}
		full := path.Join(dir, name)
		if entry["type"] == "dir" {
			full += "/"
		}
		values = append(values, full)
	}
	return capValues(values), nil
}

// splitPathPrefix splits value into the directory to list and the
// basename prefix to filter by, defaulting to "/" and "" for an empty or
// bare-name value.
func splitPathPrefix(value string) (dir, prefix string) {
	if value == "" {
		return "/", ""
	}
	if strings.HasSuffix(value, "/") {
		return strings.TrimSuffix(value, "/") + "/", ""
	}
	dir = path.Dir(value)
	prefix = path.Base(value)
	if dir == "." {
		dir = "/"
	}
	return dir, prefix
}

func matchesPrefix(s, prefix string) bool {
	if prefix == "" {
		return true
	}
	return strings.HasPrefix(strings.ToLower(s), strings.ToLower(prefix))
}

func capValues(values []string) *transport.CompletionValues {
	total := len(values)
	hasMore := total > maxResults
	if hasMore {
		values = values[:maxResults]
	}
	if values == nil {
		values = []string{}
	}
	return &transport.CompletionValues{Values: values, Total: &total, HasMore: hasMore}
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

var _ transport.CompletionRegistry = (*Registry)(nil)
