// Package tools implements the MCP tool registration framework: a registry
// that every tool (shell_exec now; screenshot/fs/process/sysinfo in a
// later phase) registers into, providing capability-gated tools/list
// aggregation and tools/call dispatch by name. See docs/specs/backend.md
// Section 3 and Section 2.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/champi-ai/rc-mcp/internal/devices"
	"github.com/champi-ai/rc-mcp/internal/mcp/schema"
	"github.com/champi-ai/rc-mcp/internal/session"
	"github.com/champi-ai/rc-mcp/internal/transport"
)

// HandlerFunc executes one tool invocation. It is called only after the
// registry has resolved and validated the target device's capability (if
// the tool declares one); the handler is responsible for its own input
// schema validation and for actually dispatching to the agent.
type HandlerFunc func(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError)

// Definition describes one registered tool.
type Definition struct {
	Name         string
	Title        string
	Description  string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	Annotations  json.RawMessage

	// RequiredCapability is the agent capability area this tool needs
	// (e.g. "shell"). Empty means the tool is always listed/callable
	// regardless of any agent's capabilities (no tool in Phase 1 needs
	// this, but the framework supports it for future non-agent tools).
	RequiredCapability string

	Handler HandlerFunc
}

// Registry implements transport.ToolRegistry: it aggregates the union of
// capabilities across all online agents for tools/list, and routes
// tools/call to the registered handler by name, after checking that the
// call's target device (from its "clientId" input field) has the tool's
// required capability enabled.
type Registry struct {
	Devices devices.DeviceRegistry

	mu   sync.RWMutex
	defs map[string]*Definition
}

// NewRegistry constructs an empty Registry backed by deviceRegistry for
// online/capability lookups.
func NewRegistry(deviceRegistry devices.DeviceRegistry) *Registry {
	return &Registry{
		Devices: deviceRegistry,
		defs:    map[string]*Definition{},
	}
}

// Register adds or replaces a tool definition.
func (r *Registry) Register(def Definition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defs[def.Name] = &def
}

// ListTools implements transport.ToolRegistry: a tool appears only if it
// has no required capability, or at least one online agent has that
// capability enabled.
func (r *Registry) ListTools(ctx context.Context) []transport.ToolDescriptor {
	online := r.onlineCapabilities(ctx)

	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.defs))
	for name := range r.defs {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]transport.ToolDescriptor, 0, len(names))
	for _, name := range names {
		def := r.defs[name]
		if def.RequiredCapability != "" && !online[def.RequiredCapability] {
			continue
		}
		out = append(out, transport.ToolDescriptor{
			Name:         def.Name,
			Title:        def.Title,
			Description:  def.Description,
			InputSchema:  def.InputSchema,
			OutputSchema: def.OutputSchema,
			Annotations:  def.Annotations,
		})
	}
	return out
}

// CallTool implements transport.ToolRegistry.
func (r *Registry) CallTool(ctx context.Context, sess *session.Session, meta transport.ToolCallMeta, name string, args json.RawMessage) (*transport.ToolCallResult, *transport.RPCError) {
	r.mu.RLock()
	def, ok := r.defs[name]
	r.mu.RUnlock()
	if !ok {
		return nil, &transport.RPCError{Code: -32601, Message: fmt.Sprintf("unknown tool %q", name)}
	}

	// Shared input validation (Section 12.6/13): every tool's arguments
	// are checked against its declared InputSchema here, before capability
	// resolution or the handler run, so "invalid params" has one shape
	// across all tools.
	if len(def.InputSchema) > 0 {
		if verrs := schema.Validate(def.InputSchema, args); len(verrs) > 0 {
			return nil, &transport.RPCError{
				Code:    -32602,
				Message: "invalid params",
				Data:    map[string]any{"validationErrors": verrs},
			}
		}
	}

	if def.RequiredCapability != "" {
		if result := r.checkCapability(ctx, def, args); result != nil {
			return result, nil
		}
	}

	return def.Handler(ctx, sess, meta, args)
}

// checkCapability resolves the target device from args' "clientId" field
// and verifies it is online with def.RequiredCapability enabled. It
// returns a non-nil tool-error ToolCallResult if the call should be
// rejected before reaching the handler, or nil if the call may proceed.
func (r *Registry) checkCapability(ctx context.Context, def *Definition, args json.RawMessage) *transport.ToolCallResult {
	clientID := extractClientID(args)
	if clientID == "" {
		return toolError("missing required field \"clientId\"")
	}

	device, err := r.Devices.Get(ctx, clientID)
	if err != nil {
		return toolError(fmt.Sprintf("Unknown device %s", clientID))
	}
	if !device.Online {
		return toolError(fmt.Sprintf("Device %s is offline", clientID))
	}
	if !hasCapability(device.Capabilities, def.RequiredCapability) {
		return toolError(fmt.Sprintf("Device %s does not have %s enabled", clientID, def.RequiredCapability))
	}
	return nil
}

func (r *Registry) onlineCapabilities(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	if r.Devices == nil {
		return out
	}
	all, err := r.Devices.List(ctx)
	if err != nil {
		return out
	}
	for _, d := range all {
		if !d.Online {
			continue
		}
		for _, c := range d.Capabilities {
			out[c] = true
		}
	}
	return out
}

func hasCapability(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

func extractClientID(args json.RawMessage) string {
	var v struct {
		ClientID string `json:"clientId"`
	}
	if err := json.Unmarshal(args, &v); err != nil {
		return ""
	}
	return v.ClientID
}

func toolError(text string) *transport.ToolCallResult {
	return &transport.ToolCallResult{
		IsError: true,
		Content: []transport.ToolContent{{Type: "text", Text: text}},
	}
}

var _ transport.ToolRegistry = (*Registry)(nil)
