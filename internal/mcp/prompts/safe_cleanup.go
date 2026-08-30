package prompts

import (
	"fmt"
	"strconv"

	"github.com/champi-ai/rc-mcp/internal/devices"
	"github.com/champi-ai/rc-mcp/internal/transport"
)

// safeCleanup builds the Section 5.2 template.
func (r *Registry) safeCleanup(device *devices.Device, args map[string]string) (*transport.PromptResult, *transport.RPCError) {
	target := args["target"]
	switch target {
	case "":
		target = "all"
	case "disk", "processes", "all":
	default:
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"target\" must be \"disk\", \"processes\", or \"all\""}
	}
	minSizeMB := 100
	if v := args["minSizeMB"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"minSizeMB\" must be a positive integer"}
		}
		minSizeMB = n
	}

	var steps string
	if target == "disk" || target == "all" {
		steps += fmt.Sprintf(`- Disk: call fs_list on /tmp, /var/tmp, /var/log, and the user's home directory (recursive where useful). Flag files larger than %d MB and anything in temp directories older than 30 days.
`, minSizeMB)
	}
	if target == "processes" || target == "all" {
		steps += `- Processes: call process_list sorted by memory and by cpu. Flag long-running processes that look orphaned (no obvious parent service, defunct/zombie state, or runaway resource usage).
`
	}

	text := fmt.Sprintf(`You are performing a guided cleanup of device %s (%s). Scope: %s. Minimum file size to flag: %d MB.

Investigation (read-only, targeting clientId %q in every tool call):
%s
Then present a numbered confirmation checklist of every proposed action (file deletions via fs_delete, process terminations via process_signal), with the expected space or resources reclaimed per item.

IMPORTANT: propose only — do NOT call fs_delete or process_signal until the user has explicitly confirmed specific checklist items. Never propose deleting system files, package manager state, or anything under /etc, /usr, /boot, or /bin.`,
		device.ID, device.Label, target, minSizeMB, device.ID, steps)

	return &transport.PromptResult{
		Description: fmt.Sprintf("Safe cleanup of device %s (target: %s, min size: %d MB)", device.ID, target, minSizeMB),
		Messages:    []transport.PromptMessage{userMessage(text)},
	}, nil
}
