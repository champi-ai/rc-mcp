package executor

import "os"

// DisplayServer identifies which windowing system is active on the agent's
// desktop session (Section 19: "detect the active display server at
// runtime").
type DisplayServer int

const (
	// DisplayServerNone means neither a Wayland nor an X11 session was
	// detected (e.g. a headless machine with no desktop session at all).
	DisplayServerNone DisplayServer = iota
	DisplayServerX11
	DisplayServerWayland
)

// DetectDisplayServer inspects the standard environment variables a
// desktop session sets to determine which display server is active.
// WAYLAND_DISPLAY takes precedence when both are set (common under
// Xwayland compatibility, where DISPLAY is also present) -- the native
// Wayland capture path is preferred whenever the session actually is
// Wayland.
func DetectDisplayServer() DisplayServer {
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		return DisplayServerWayland
	}
	if os.Getenv("DISPLAY") != "" {
		return DisplayServerX11
	}
	return DisplayServerNone
}
