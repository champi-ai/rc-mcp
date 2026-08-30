package prompts

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/champi-ai/rc-mcp/internal/devices"
	"github.com/champi-ai/rc-mcp/internal/session"
	"github.com/champi-ai/rc-mcp/internal/transport"
)

// diagnoseSystem builds the Section 5.1 template around a live sysinfo
// snapshot of the target device.
func (r *Registry) diagnoseSystem(ctx context.Context, sess *session.Session, device *devices.Device, args map[string]string) (*transport.PromptResult, *transport.RPCError) {
	symptom := args["symptom"]
	if symptom == "" {
		return nil, &transport.RPCError{Code: -32602, Message: "invalid params: \"symptom\" is required"}
	}
	if !device.Online {
		return nil, &transport.RPCError{Code: -32603, Message: fmt.Sprintf("Device %s is offline", device.ID)}
	}

	snapshot := "(sysinfo snapshot unavailable)"
	correlationID, err := newCorrelationID()
	if err == nil {
		input := map[string]any{"clientId": device.ID, "sections": []string{"all"}}
		result, dispatchErr := r.Bridge.Dispatch(ctx, device.ID, correlationID, "sysinfo_get", sess.ID, input, nil)
		if dispatchErr == nil && !result.IsError {
			if data, err := json.MarshalIndent(result.Output, "", "  "); err == nil {
				snapshot = string(data)
			}
		}
	}

	// sortBy metric for process_list, matched to the symptom.
	metric := "cpu"
	switch {
	case contains(symptom, "mem"):
		metric = "memory"
	case contains(symptom, "disk"), contains(symptom, "full"):
		metric = "memory" // disk symptoms still surface hogs; fs_list does the disk work
	}

	text := fmt.Sprintf(`You are diagnosing device %s (%s). Reported symptom: %q.

Current system snapshot (from sysinfo_get, taken just now):
%s

Follow these steps, targeting clientId %q in every tool call:
1. Review the snapshot above and identify anything abnormal related to the symptom.
2. Call sysinfo_get again if you need a fresh or more focused view (use the "sections" argument).
3. Call process_list with sortBy=%q to find the top resource consumers.
4. For any suspicious process, call process_info with its pid.
5. If the symptom is disk-related, use fs_list on likely-large directories (/var/log, /tmp, the user's home) to locate space consumers.
6. Summarize the likely cause and suggest remediation steps. Do NOT take destructive actions (process_signal, fs_delete) without explicitly asking the user first.`,
		device.ID, device.Label, symptom, snapshot, device.ID, metric)

	return &transport.PromptResult{
		Description: fmt.Sprintf("Diagnose %q on device %s", symptom, device.ID),
		Messages:    []transport.PromptMessage{userMessage(text)},
	}, nil
}
