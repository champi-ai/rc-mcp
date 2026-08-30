package executor

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// withFakeGrim overrides grimPath/grimRun for the duration of the test and
// restores the originals on cleanup.
func withFakeGrim(t *testing.T, pathFn func() (string, error), runFn func(ctx context.Context, path string) ([]byte, error)) {
	t.Helper()
	origPath, origRun := grimPath, grimRun
	grimPath, grimRun = pathFn, runFn
	t.Cleanup(func() { grimPath, grimRun = origPath, origRun })
}

func encodeTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

func TestCaptureWayland_GrimNotFound_ReturnsSpecificError(t *testing.T) {
	withFakeGrim(t,
		func() (string, error) { return "", errors.New("no such file") },
		nil,
	)
	_, _, _, err := captureWayland(0, 6)
	if !errors.Is(err, ErrWaylandCaptureUnavailable) {
		t.Fatalf("err = %v, want ErrWaylandCaptureUnavailable", err)
	}
}

func TestCaptureWayland_CaptureCommandFails(t *testing.T) {
	withFakeGrim(t,
		func() (string, error) { return "/usr/bin/grim", nil },
		func(ctx context.Context, path string) ([]byte, error) {
			return nil, errors.New("wrapped: permission denied by portal")
		},
	)
	_, _, _, err := captureWayland(0, 6)
	if err == nil {
		t.Fatal("expected an error when the grim command fails")
	}
}

func TestCaptureWayland_Success_ProducesValidPNG(t *testing.T) {
	raw := encodeTestPNG(t, 100, 50)
	withFakeGrim(t,
		func() (string, error) { return "/usr/bin/grim", nil },
		func(ctx context.Context, path string) ([]byte, error) { return raw, nil },
	)

	data, w, h, err := captureWayland(0, 6)
	if err != nil {
		t.Fatalf("captureWayland: %v", err)
	}
	if w != 100 || h != 50 {
		t.Fatalf("dimensions = %dx%d, want 100x50", w, h)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("output is not a valid PNG: %v", err)
	}
	if img.Bounds().Dx() != 100 || img.Bounds().Dy() != 50 {
		t.Fatalf("decoded bounds = %v, want 100x50", img.Bounds())
	}
}

func TestCaptureWayland_DownscalesToMaxWidth(t *testing.T) {
	raw := encodeTestPNG(t, 200, 100)
	withFakeGrim(t,
		func() (string, error) { return "/usr/bin/grim", nil },
		func(ctx context.Context, path string) ([]byte, error) { return raw, nil },
	)

	data, w, h, err := captureWayland(50, 6)
	if err != nil {
		t.Fatalf("captureWayland: %v", err)
	}
	if w != 50 || h != 25 {
		t.Fatalf("dimensions = %dx%d, want 50x25 (aspect preserved)", w, h)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("output is not a valid PNG: %v", err)
	}
	if img.Bounds().Dx() != 50 {
		t.Fatalf("decoded width = %d, want 50", img.Bounds().Dx())
	}
}

func TestCaptureWayland_MalformedOutputIsAClearError(t *testing.T) {
	withFakeGrim(t,
		func() (string, error) { return "/usr/bin/grim", nil },
		func(ctx context.Context, path string) ([]byte, error) { return []byte("not a png"), nil },
	)
	_, _, _, err := captureWayland(0, 6)
	if err == nil {
		t.Fatal("expected a decode error for malformed grim output")
	}
}

func TestCaptureScreenshot_RoutesToWaylandBackend(t *testing.T) {
	defer unsetEnvForTest(t, "WAYLAND_DISPLAY")()
	defer unsetEnvForTest(t, "DISPLAY")()
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")

	raw := encodeTestPNG(t, 64, 64)
	called := false
	withFakeGrim(t,
		func() (string, error) { return "/usr/bin/grim", nil },
		func(ctx context.Context, path string) ([]byte, error) { called = true; return raw, nil },
	)

	_, w, h, err := CaptureScreenshot("", 0, 6)
	if err != nil {
		t.Fatalf("CaptureScreenshot: %v", err)
	}
	if !called {
		t.Fatal("CaptureScreenshot did not route to the Wayland backend")
	}
	if w != 64 || h != 64 {
		t.Fatalf("dimensions = %dx%d, want 64x64", w, h)
	}
}
