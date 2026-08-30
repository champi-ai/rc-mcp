package prompts

import (
	"fmt"

	"github.com/CloudKeter/rc-mcp/internal/devices"
	"github.com/CloudKeter/rc-mcp/internal/transport"
)

// shellWorkflow builds the Section 5.3 template.
func (r *Registry) shellWorkflow(device *devices.Device, args map[string]string) (*transport.PromptResult, *transport.RPCError) {
	task := args["task"]
	if task == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"task\" is required"}
	}

	text := fmt.Sprintf(`You are working in an interactive shell on device %s (%s). The user's goal: %q.

Setup:
1. Call shell_session_start with clientId %q to open a PTY-backed shell session. Keep the returned shellSessionId for every subsequent call.

Guidelines for multi-turn shell interaction:
- Send input with shell_session_write; include "\n" to press Enter. Output returns in the same call (and may continue in later reads).
- Run one logical command at a time and read its output before deciding the next step.
- Prefer read-only inspection first (ls, cat, ps, df, systemctl status) before anything that mutates state.
- For long-running commands, consider appending " &" or using timeouts so the session stays responsive.
- Ask the user before running anything destructive or irreversible (rm, dd, mkfs, systemctl stop/disable, package removal).
- When the task is complete (or the user is done), call shell_session_close to release the PTY.`,
		device.ID, device.Label, task, device.ID)

	return &transport.PromptResult{
		Description: fmt.Sprintf("Interactive shell on device %s: %s", device.ID, task),
		Messages:    []transport.PromptMessage{userMessage(text)},
	}, nil
}
