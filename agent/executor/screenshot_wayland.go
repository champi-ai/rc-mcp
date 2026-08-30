// This file implements the Wayland screenshot capture backend for
// CaptureScreenshot (screenshot.go), via the grim CLI tool -- the
// pragmatic alternative to a full pipewire/xdg-desktop-portal D-Bus
// integration noted in docs/specs/backend.md Section 19. grim itself talks
// to the compositor's wlr-screencopy (or equivalent) protocol; on
// compositors/portals that require an interactive permission grant per
// capture (e.g. GNOME on Wayland without a wlroots-style protocol), grim
// may fail or hang waiting on a portal dialog the agent has no way to
// answer -- see ErrWaylandCaptureUnavailable.
package executor

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os/exec"
	"time"
)

// ErrWaylandCaptureUnavailable is returned when Wayland screenshot capture
// cannot proceed: grim is not installed, or the capture command itself
// failed (including a portal permission denial). Callers get this as a
// specific, immediate error rather than a hang -- grimCaptureTimeout
// bounds how long the external command may run.
var ErrWaylandCaptureUnavailable = fmt.Errorf("executor: wayland screenshot capture unavailable")

// grimCaptureTimeout bounds how long the grim subprocess may run. A
// portal-gated compositor that requires an interactive permission grant
// the agent cannot answer would otherwise hang the dispatch indefinitely
// (Section 19: "may not work in headless/minimal compositor
// environments -- document this limitation").
const grimCaptureTimeout = 10 * time.Second

// grimPath is a package variable (rather than a direct exec.LookPath call)
// so tests can substitute a fake "grim" without touching PATH.
var grimPath = func() (string, error) { return exec.LookPath("grim") }

// grimRun invokes grim, returning its stdout (raw PNG bytes). Overridable
// in tests to avoid depending on a real Wayland compositor.
var grimRun = func(ctx context.Context, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, "-t", "png", "-")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%w: %s", ErrWaylandCaptureUnavailable, msg)
	}
	return stdout.Bytes(), nil
}

// captureWayland captures the full compositor output via grim, then
// applies the same downscale/re-encode pipeline captureX11 uses so
// screenshot_capture/watch behave identically regardless of backend.
//
// Per-monitor selection (the "monitor" input argument) is not implemented
// for Wayland in this pass: grim without a geometry/output argument
// captures every output composited together, matching the X11 root
// window's "all monitors stitched" default. A future pass can add
// per-output selection via `grim -o <name>` once an output-enumeration
// story exists on the Wayland side.
func captureWayland(maxWidth, quality int) (pngBytes []byte, width, height int, err error) {
	path, err := grimPath()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%w: grim not found: %v", ErrWaylandCaptureUnavailable, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), grimCaptureTimeout)
	defer cancel()
	raw, err := grimRun(ctx, path)
	if err != nil {
		return nil, 0, 0, err
	}

	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("executor: decode grim output: %w", err)
	}

	rgba := toRGBA(img)
	w := rgba.Bounds().Dx()
	if maxWidth > 0 && w > maxWidth {
		rgba = downscale(rgba, maxWidth)
	}

	data, err := encodePNG(rgba, quality)
	if err != nil {
		return nil, 0, 0, err
	}
	b := rgba.Bounds()
	return data, b.Dx(), b.Dy(), nil
}

// toRGBA converts any image.Image to *image.RGBA (grim's PNG output is
// typically NRGBA), since downscale operates on *image.RGBA.
func toRGBA(img image.Image) *image.RGBA {
	if rgba, ok := img.(*image.RGBA); ok {
		return rgba
	}
	b := img.Bounds()
	out := image.NewRGBA(b)
	draw.Draw(out, b, img, b.Min, draw.Src)
	return out
}
