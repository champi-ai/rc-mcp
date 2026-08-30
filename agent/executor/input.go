// This file implements the input_key, input_mouse_click, input_mouse_move,
// and input_type executors via xdotool -- the pragmatic X11 implementation
// noted in docs/specs/backend.md Section 19 ("via xdotool or the XTest
// extension"). This is the `input` capability: a higher-risk area,
// independently toggle-able from shell/screenshot/filesystem/process/
// sysinfo, requiring mandatory elicitation on every single action at the
// server layer (internal/mcp/tools/input.go) -- this file only performs
// the already-confirmed action.
package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// ErrInputUnavailable is returned when xdotool is not installed or the
// injected action itself fails (e.g. no X11 display reachable).
var ErrInputUnavailable = errors.New("executor: input injection unavailable (xdotool not found or action failed)")

// inputActionTimeout bounds each xdotool invocation. These are near-
// instant local operations; a long hang almost certainly means xdotool is
// stuck waiting on a display that isn't responding.
const inputActionTimeout = 5 * time.Second

// xdotoolPath/xdotoolRun are package vars (rather than direct exec calls)
// so tests can substitute a fake without a real X11 session or the
// xdotool binary installed.
var (
	xdotoolPath = func() (string, error) { return exec.LookPath("xdotool") }
	xdotoolRun  = func(ctx context.Context, path string, args ...string) error {
		cmd := exec.CommandContext(ctx, path, args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			msg := stderr.String()
			if msg == "" {
				msg = err.Error()
			}
			return fmt.Errorf("%w: %s", ErrInputUnavailable, msg)
		}
		return nil
	}
)

func runXdotool(args ...string) error {
	path, err := xdotoolPath()
	if err != nil {
		return fmt.Errorf("%w: xdotool not found: %v", ErrInputUnavailable, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), inputActionTimeout)
	defer cancel()
	return xdotoolRun(ctx, path, args...)
}

// InputKey sends a keypress (or key combo, e.g. "ctrl+c") via `xdotool key`.
func InputKey(key string) error {
	return runXdotool("key", key)
}

// InputMouseMove moves the mouse cursor to absolute coordinates (x, y) via
// `xdotool mousemove`.
func InputMouseMove(x, y int) error {
	return runXdotool("mousemove", fmt.Sprintf("%d", x), fmt.Sprintf("%d", y))
}

// mouseButtonCode maps a button name to xdotool's numeric button code
// (1=left, 2=middle, 3=right -- the standard X11 button numbering).
func mouseButtonCode(button string) (string, error) {
	switch button {
	case "", "left":
		return "1", nil
	case "middle":
		return "2", nil
	case "right":
		return "3", nil
	default:
		return "", fmt.Errorf("executor: unknown mouse button %q (want left, middle, or right)", button)
	}
}

// InputMouseClick moves to (x, y) and clicks button ("left"/"middle"/
// "right", default "left") via `xdotool mousemove ... click`.
func InputMouseClick(x, y int, button string) error {
	code, err := mouseButtonCode(button)
	if err != nil {
		return err
	}
	return runXdotool("mousemove", fmt.Sprintf("%d", x), fmt.Sprintf("%d", y), "click", code)
}

// InputType types literal text via `xdotool type`. The "--" separator
// prevents text starting with "-" from being parsed as an xdotool flag.
func InputType(text string) error {
	return runXdotool("type", "--", text)
}
