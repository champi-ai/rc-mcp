// This file implements the screenshot_capture and screenshot_watch
// executors. See docs/specs/backend.md Section 3.2.
package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"time"

	"github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/xproto"
)

// ErrNoDisplay is returned when no X11 display is configured/reachable.
var ErrNoDisplay = errors.New("executor: no display available")

const (
	DefaultScreenshotDisplay = ":0"
	DefaultScreenshotQuality = 6
	DefaultWatchIntervalMs   = 2000
	MinWatchIntervalMs       = 500
	DefaultWatchMaxFrames    = 30
	MaxWatchMaxFrames        = 120
	DefaultWatchDurationSecs = 60
	MaxWatchDurationSecs     = 300
)

// CaptureScreenshot captures the current display, encodes it as PNG, and
// downscales to maxWidth (preserving aspect ratio) if set and exceeded.
// quality maps to PNG compression level 0-9 the same way screenshot_
// capture's input schema documents it (0 = no compression/fastest, 9 =
// best compression); values outside 0-9 fall back to
// DefaultScreenshotQuality. The tool surface (screenshot_capture/watch) is
// identical on X11 and Wayland -- CaptureScreenshot picks the backend at
// call time via DetectDisplayServer (Section 19). display is only
// meaningful for X11 (an explicit :N override); it is ignored under
// Wayland, which has no equivalent per-call display selector.
func CaptureScreenshot(display string, maxWidth int, quality int) (pngBytes []byte, width, height int, err error) {
	switch DetectDisplayServer() {
	case DisplayServerWayland:
		return captureWayland(maxWidth, quality)
	case DisplayServerX11:
		return captureX11(display, maxWidth, quality)
	default:
		return nil, 0, 0, ErrNoDisplay
	}
}

// captureX11 connects to the X11 display, grabs the root window (all
// monitors on that display, stitched into one image the way a typical
// multi-monitor X11 setup already presents a single root window), encodes
// it as PNG, and downscales to maxWidth (preserving aspect ratio) if set
// and exceeded.
func captureX11(display string, maxWidth int, quality int) (pngBytes []byte, width, height int, err error) {
	if display == "" {
		display = os.Getenv("DISPLAY")
	}
	if display == "" {
		display = DefaultScreenshotDisplay
	}
	if os.Getenv("DISPLAY") == "" && display == DefaultScreenshotDisplay {
		// No override was given and $DISPLAY isn't set either -- rather
		// than let xgb attempt (and fail against) a default socket, report
		// the clear "no display" error the spec calls for.
		return nil, 0, 0, ErrNoDisplay
	}

	conn, err := xgb.NewConnDisplay(display)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%w: %v", ErrNoDisplay, err)
	}
	defer conn.Close()

	img, w, h, err := captureRootWindow(conn)
	if err != nil {
		return nil, 0, 0, err
	}

	if maxWidth > 0 && w > maxWidth {
		img = downscale(img, maxWidth)
		w = img.Bounds().Dx()
		h = img.Bounds().Dy()
	}

	data, err := encodePNG(img, quality)
	if err != nil {
		return nil, 0, 0, err
	}
	return data, w, h, nil
}

func captureRootWindow(conn *xgb.Conn) (*image.RGBA, int, int, error) {
	setup := xproto.Setup(conn)
	screen := setup.DefaultScreen(conn)
	width := int(screen.WidthInPixels)
	height := int(screen.HeightInPixels)
	if width <= 0 || height <= 0 {
		return nil, 0, 0, fmt.Errorf("executor: invalid screen dimensions %dx%d", width, height)
	}

	reply, err := xproto.GetImage(
		conn, xproto.ImageFormatZPixmap, xproto.Drawable(screen.Root),
		0, 0, uint16(width), uint16(height), 0xffffffff,
	).Reply()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("executor: X11 GetImage failed: %w", err)
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	data := reply.Data
	stride := width * 4
	for y := 0; y < height; y++ {
		rowOff := y * stride
		for x := 0; x < width; x++ {
			i := rowOff + x*4
			if i+3 >= len(data) {
				continue
			}
			// 24/32-bit ZPixmap on a little-endian X server is BGRX/BGRA.
			b, g, r := data[i], data[i+1], data[i+2]
			img.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}
	return img, width, height, nil
}

// downscale resizes img so its width is maxWidth, preserving aspect ratio,
// using nearest-neighbor sampling (dependency-free; screenshot thumbnails
// don't need higher-quality resampling).
func downscale(img *image.RGBA, maxWidth int) *image.RGBA {
	b := img.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	if srcW <= maxWidth {
		return img
	}
	dstW := maxWidth
	dstH := int(float64(srcH) * float64(dstW) / float64(srcW))
	if dstH < 1 {
		dstH = 1
	}

	out := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for y := 0; y < dstH; y++ {
		srcY := y * srcH / dstH
		for x := 0; x < dstW; x++ {
			srcX := x * srcW / dstW
			out.Set(x, y, img.At(b.Min.X+srcX, b.Min.Y+srcY))
		}
	}
	return out
}

func encodePNG(img image.Image, quality int) ([]byte, error) {
	level := png.DefaultCompression
	switch {
	case quality <= 0:
		level = png.NoCompression
	case quality >= 9:
		level = png.BestCompression
	case quality <= 3:
		level = png.BestSpeed
	}
	var buf bytes.Buffer
	enc := &png.Encoder{CompressionLevel: level}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("executor: png encode failed: %w", err)
	}
	return buf.Bytes(), nil
}

// WatchOptions configures a WatchScreenshots run.
type WatchOptions struct {
	Display      string
	MaxWidth     int
	Quality      int
	IntervalMs   int
	MaxFrames    int
	DurationSecs int
}

// WatchFrame is delivered to onFrame for each successfully captured frame.
type WatchFrame struct {
	PNG   []byte
	Index int // 0-based frame index
}

// WatchResult is the terminal outcome of a WatchScreenshots run.
type WatchResult struct {
	FramesCaptured int
	DurationMs     int64
	StoppedReason  string // "maxFrames" | "duration" | "cancelled" | "error"
}

// WatchScreenshots captures screenshots at opts.IntervalMs, invoking onFrame
// for each one, until opts.MaxFrames frames have been captured,
// opts.DurationSecs elapses, or ctx is cancelled -- whichever comes first.
// A capture error stops the watch early (StoppedReason "error") rather than
// looping forever against a broken display.
func WatchScreenshots(ctx context.Context, opts WatchOptions, onFrame func(WatchFrame)) (WatchResult, error) {
	captureFn := func() ([]byte, int, int, error) {
		return CaptureScreenshot(opts.Display, opts.MaxWidth, opts.Quality)
	}
	return watchScreenshots(ctx, opts, captureFn, onFrame)
}

// watchScreenshots is WatchScreenshots with the capture step injected, so
// tests can exercise the interval/maxFrames/duration/cancel loop logic
// without a real X11 display.
func watchScreenshots(ctx context.Context, opts WatchOptions, captureFn func() ([]byte, int, int, error), onFrame func(WatchFrame)) (WatchResult, error) {
	interval := opts.IntervalMs
	if interval < MinWatchIntervalMs {
		interval = DefaultWatchIntervalMs
	}
	maxFrames := opts.MaxFrames
	if maxFrames <= 0 || maxFrames > MaxWatchMaxFrames {
		if maxFrames > MaxWatchMaxFrames {
			maxFrames = MaxWatchMaxFrames
		} else {
			maxFrames = DefaultWatchMaxFrames
		}
	}
	durationSecs := opts.DurationSecs
	if durationSecs <= 0 || durationSecs > MaxWatchDurationSecs {
		if durationSecs > MaxWatchDurationSecs {
			durationSecs = MaxWatchDurationSecs
		} else {
			durationSecs = DefaultWatchDurationSecs
		}
	}

	start := time.Now()
	deadline := start.Add(time.Duration(durationSecs) * time.Second)

	// Capture the first frame immediately, per typical "watch" semantics,
	// then continue on the interval ticker.
	frames := 0
	for {
		frameData, _, _, err := captureFn()
		if err != nil {
			return WatchResult{FramesCaptured: frames, DurationMs: time.Since(start).Milliseconds(), StoppedReason: "error"}, err
		}
		onFrame(WatchFrame{PNG: frameData, Index: frames})
		frames++

		if frames >= maxFrames {
			return WatchResult{FramesCaptured: frames, DurationMs: time.Since(start).Milliseconds(), StoppedReason: "maxFrames"}, nil
		}
		if time.Now().After(deadline) {
			return WatchResult{FramesCaptured: frames, DurationMs: time.Since(start).Milliseconds(), StoppedReason: "duration"}, nil
		}

		select {
		case <-ctx.Done():
			return WatchResult{FramesCaptured: frames, DurationMs: time.Since(start).Milliseconds(), StoppedReason: "cancelled"}, nil
		case <-time.After(time.Duration(interval) * time.Millisecond):
		}

		if time.Now().After(deadline) {
			return WatchResult{FramesCaptured: frames, DurationMs: time.Since(start).Milliseconds(), StoppedReason: "duration"}, nil
		}
	}
}
