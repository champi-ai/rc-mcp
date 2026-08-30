package executor

import "testing"

func TestDetectDisplayServer_Wayland(t *testing.T) {
	defer unsetEnvForTest(t, "WAYLAND_DISPLAY")()
	defer unsetEnvForTest(t, "DISPLAY")()
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")

	if got := DetectDisplayServer(); got != DisplayServerWayland {
		t.Fatalf("got %v, want DisplayServerWayland", got)
	}
}

func TestDetectDisplayServer_X11(t *testing.T) {
	defer unsetEnvForTest(t, "WAYLAND_DISPLAY")()
	defer unsetEnvForTest(t, "DISPLAY")()
	t.Setenv("DISPLAY", ":0")

	if got := DetectDisplayServer(); got != DisplayServerX11 {
		t.Fatalf("got %v, want DisplayServerX11", got)
	}
}

func TestDetectDisplayServer_WaylandPreferredOverXwaylandCompat(t *testing.T) {
	defer unsetEnvForTest(t, "WAYLAND_DISPLAY")()
	defer unsetEnvForTest(t, "DISPLAY")()
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	t.Setenv("DISPLAY", ":0") // Xwayland compatibility, both set

	if got := DetectDisplayServer(); got != DisplayServerWayland {
		t.Fatalf("got %v, want DisplayServerWayland to take precedence", got)
	}
}

func TestDetectDisplayServer_None(t *testing.T) {
	defer unsetEnvForTest(t, "WAYLAND_DISPLAY")()
	defer unsetEnvForTest(t, "DISPLAY")()

	if got := DetectDisplayServer(); got != DisplayServerNone {
		t.Fatalf("got %v, want DisplayServerNone", got)
	}
}
