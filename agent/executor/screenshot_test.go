package executor

import (
	"context"
	"errors"
	"image"
	"os"
	"sync"
	"testing"
	"time"
)

func newSolidImage(w, h int) *image.RGBA {
	return image.NewRGBA(image.Rect(0, 0, w, h))
}

func TestCaptureScreenshot_NoDisplay_ReturnsClearError(t *testing.T) {
	restoreDisplay := unsetEnvForTest(t, "DISPLAY")
	defer restoreDisplay()
	restoreWayland := unsetEnvForTest(t, "WAYLAND_DISPLAY")
	defer restoreWayland()

	_, _, _, err := CaptureScreenshot("", 0, 0)
	if !errors.Is(err, ErrNoDisplay) {
		t.Fatalf("err = %v, want ErrNoDisplay", err)
	}
}

// unsetEnvForTest unsets key and returns a func restoring its prior value
// (present or absent).
func unsetEnvForTest(t *testing.T, key string) func() {
	t.Helper()
	old, had := os.LookupEnv(key)
	_ = os.Unsetenv(key)
	return func() {
		if had {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	}
}

func TestWatchScreenshots_StopsAtMaxFrames(t *testing.T) {
	var mu sync.Mutex
	var frames []int
	capture := func() ([]byte, int, int, error) {
		return []byte("fake-png"), 10, 10, nil
	}

	result, err := watchScreenshots(context.Background(), WatchOptions{IntervalMs: 500, MaxFrames: 3, DurationSecs: 300}, capture, func(f WatchFrame) {
		mu.Lock()
		frames = append(frames, f.Index)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("watchScreenshots: %v", err)
	}
	if result.StoppedReason != "maxFrames" {
		t.Fatalf("StoppedReason = %q, want maxFrames", result.StoppedReason)
	}
	if result.FramesCaptured != 3 {
		t.Fatalf("FramesCaptured = %d, want 3", result.FramesCaptured)
	}
	if len(frames) != 3 {
		t.Fatalf("onFrame called %d times, want 3", len(frames))
	}
}

func TestWatchScreenshots_StopsAtDuration(t *testing.T) {
	capture := func() ([]byte, int, int, error) {
		return []byte("fake-png"), 10, 10, nil
	}

	result, err := watchScreenshots(context.Background(), WatchOptions{IntervalMs: MinWatchIntervalMs, MaxFrames: 120, DurationSecs: 1}, capture, func(WatchFrame) {})
	if err != nil {
		t.Fatalf("watchScreenshots: %v", err)
	}
	if result.StoppedReason != "duration" {
		t.Fatalf("StoppedReason = %q, want duration", result.StoppedReason)
	}
	if result.FramesCaptured < 1 {
		t.Fatal("expected at least one frame before duration elapsed")
	}
}

func TestWatchScreenshots_CancelStopsPromptly(t *testing.T) {
	capture := func() ([]byte, int, int, error) {
		return []byte("fake-png"), 10, 10, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	result, err := watchScreenshots(ctx, WatchOptions{IntervalMs: 5000, MaxFrames: 120, DurationSecs: 300}, capture, func(WatchFrame) {})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("watchScreenshots: %v", err)
	}
	if result.StoppedReason != "cancelled" {
		t.Fatalf("StoppedReason = %q, want cancelled", result.StoppedReason)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("cancel took too long to stop the watch loop: %s", elapsed)
	}
}

func TestWatchScreenshots_CaptureErrorStopsEarly(t *testing.T) {
	boom := errors.New("boom")
	capture := func() ([]byte, int, int, error) {
		return nil, 0, 0, boom
	}

	result, err := watchScreenshots(context.Background(), WatchOptions{}, capture, func(WatchFrame) {})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if result.StoppedReason != "error" {
		t.Fatalf("StoppedReason = %q, want error", result.StoppedReason)
	}
	if result.FramesCaptured != 0 {
		t.Fatalf("FramesCaptured = %d, want 0", result.FramesCaptured)
	}
}

func TestDownscale_PreservesAspectRatio(t *testing.T) {
	img := newSolidImage(200, 100)
	out := downscale(img, 100)
	if out.Bounds().Dx() != 100 {
		t.Fatalf("width = %d, want 100", out.Bounds().Dx())
	}
	if out.Bounds().Dy() != 50 {
		t.Fatalf("height = %d, want 50", out.Bounds().Dy())
	}
}

func TestDownscale_NoOpWhenAlreadyNarrow(t *testing.T) {
	img := newSolidImage(50, 50)
	out := downscale(img, 100)
	if out.Bounds().Dx() != 50 {
		t.Fatalf("width = %d, want unchanged 50", out.Bounds().Dx())
	}
}
