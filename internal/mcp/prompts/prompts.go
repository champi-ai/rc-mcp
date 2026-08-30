// Package prompts implements the three operator-defined prompt templates
// from docs/specs/backend.md Section 5: diagnose_system, safe_cleanup, and
// shell_workflow. Each prompt targets one paired device and renders a
// multi-step workflow the LLM client follows with the server's tools.
package prompts

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/champi-ai/rc-mcp/internal/agent"
	"github.com/champi-ai/rc-mcp/internal/devices"
	"github.com/champi-ai/rc-mcp/internal/protocol"
	"github.com/champi-ai/rc-mcp/internal/session"
	"github.com/champi-ai/rc-mcp/internal/transport"
)

// Dispatcher is the subset of *agent.Bridge the diagnose_system prompt
// needs to take a live sysinfo snapshot (same shape as
// tools.ShellDispatcher, injectable for tests).
type Dispatcher interface {
	Dispatch(ctx context.Context, deviceID, correlationID, tool, sessionID string, input any, onProgress agent.ProgressFunc) (protocol.ResultPayload, error)
}

// Registry implements transport.PromptRegistry.
type Registry struct {
	Devices devices.DeviceRegistry
	Bridge  Dispatcher
}

// NewRegistry constructs a prompt Registry.
func NewRegistry(deviceRegistry devices.DeviceRegistry, bridge Dispatcher) *Registry {
	return &Registry{Devices: deviceRegistry, Bridge: bridge}
}

// ListPrompts implements transport.PromptRegistry.
func (r *Registry) ListPrompts(ctx context.Context) []transport.PromptDescriptor {
	return []transport.PromptDescriptor{
		{
			Name:        "diagnose_system",
			Description: "Guided system diagnostics workflow for a specific device. Gathers system info, checks for high resource usage, suggests investigation steps.",
			Arguments: []transport.PromptArgument{
				{Name: "clientId", Description: "Target device", Required: true},
				{Name: "symptom", Description: "e.g. \"high cpu\", \"disk full\", \"network slow\"", Required: true},
			},
		},
		{
			Name:        "safe_cleanup",
			Description: "Guided cleanup workflow for a specific device. Identifies large/old files, orphaned processes, and temp directories, then proposes deletions with confirmation.",
			Arguments: []transport.PromptArgument{
				{Name: "clientId", Description: "Target device", Required: true},
				{Name: "target", Description: "\"disk\", \"processes\", or \"all\" (default: \"all\")"},
				{Name: "minSizeMB", Description: "Minimum file size to flag in MB (default: 100)"},
			},
		},
		{
			Name:        "shell_workflow",
			Description: "Start an interactive shell session on a specific device with context about what the user wants to accomplish.",
			Arguments: []transport.PromptArgument{
				{Name: "clientId", Description: "Target device", Required: true},
				{Name: "task", Description: "Natural language description of the task", Required: true},
			},
		},
	}
}

// GetPrompt implements transport.PromptRegistry.
func (r *Registry) GetPrompt(ctx context.Context, sess *session.Session, name string, args map[string]string) (*transport.PromptResult, *transport.RPCError) {
	clientID := args["clientId"]
	if clientID == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"clientId\" is required"}
	}
	device, err := r.Devices.Get(ctx, clientID)
	if err != nil {
		return nil, &transport.RPCError{Code: -32602, Message: fmt.Sprintf("Unknown device %s", clientID)}
	}

	switch name {
	case "diagnose_system":
		return r.diagnoseSystem(ctx, sess, device, args)
	case "safe_cleanup":
		return r.safeCleanup(device, args)
	case "shell_workflow":
		return r.shellWorkflow(device, args)
	default:
		return nil, &transport.RPCError{Code: -32602, Message: fmt.Sprintf("unknown prompt %q", name)}
	}
}

func userMessage(text string) transport.PromptMessage {
	return transport.PromptMessage{Role: "user", Content: transport.PromptContent{Type: "text", Text: text}}
}

func contains(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), substr)
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

var _ transport.PromptRegistry = (*Registry)(nil)
