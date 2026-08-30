package executor

import (
	"context"
	"errors"
	"testing"
)

func withFakeXdotool(t *testing.T, pathFn func() (string, error), runFn func(ctx context.Context, path string, args ...string) error) {
	t.Helper()
	origPath, origRun := xdotoolPath, xdotoolRun
	xdotoolPath, xdotoolRun = pathFn, runFn
	t.Cleanup(func() { xdotoolPath, xdotoolRun = origPath, origRun })
}

func TestInputKey_NotFound(t *testing.T) {
	withFakeXdotool(t, func() (string, error) { return "", errors.New("not found") }, nil)
	if err := InputKey("Return"); !errors.Is(err, ErrInputUnavailable) {
		t.Fatalf("err = %v, want ErrInputUnavailable", err)
	}
}

func TestInputKey_Success(t *testing.T) {
	var gotArgs []string
	withFakeXdotool(t,
		func() (string, error) { return "/usr/bin/xdotool", nil },
		func(ctx context.Context, path string, args ...string) error { gotArgs = args; return nil },
	)
	if err := InputKey("ctrl+c"); err != nil {
		t.Fatalf("InputKey: %v", err)
	}
	want := []string{"key", "ctrl+c"}
	if len(gotArgs) != len(want) || gotArgs[0] != want[0] || gotArgs[1] != want[1] {
		t.Fatalf("args = %v, want %v", gotArgs, want)
	}
}

func TestInputKey_CommandFails(t *testing.T) {
	withFakeXdotool(t,
		func() (string, error) { return "/usr/bin/xdotool", nil },
		func(ctx context.Context, path string, args ...string) error { return errors.New("no display") },
	)
	if err := InputKey("Return"); err == nil {
		t.Fatal("expected an error when xdotool fails")
	}
}

func TestInputMouseMove_Success(t *testing.T) {
	var gotArgs []string
	withFakeXdotool(t,
		func() (string, error) { return "/usr/bin/xdotool", nil },
		func(ctx context.Context, path string, args ...string) error { gotArgs = args; return nil },
	)
	if err := InputMouseMove(100, 200); err != nil {
		t.Fatalf("InputMouseMove: %v", err)
	}
	want := []string{"mousemove", "100", "200"}
	for i, w := range want {
		if gotArgs[i] != w {
			t.Fatalf("args = %v, want %v", gotArgs, want)
		}
	}
}

func TestInputMouseClick_ButtonMapping(t *testing.T) {
	cases := []struct {
		button, wantCode string
	}{
		{"", "1"}, {"left", "1"}, {"middle", "2"}, {"right", "3"},
	}
	for _, c := range cases {
		var gotArgs []string
		withFakeXdotool(t,
			func() (string, error) { return "/usr/bin/xdotool", nil },
			func(ctx context.Context, path string, args ...string) error { gotArgs = args; return nil },
		)
		if err := InputMouseClick(10, 20, c.button); err != nil {
			t.Fatalf("button %q: InputMouseClick: %v", c.button, err)
		}
		want := []string{"mousemove", "10", "20", "click", c.wantCode}
		if len(gotArgs) != len(want) {
			t.Fatalf("button %q: args = %v, want %v", c.button, gotArgs, want)
		}
		for i := range want {
			if gotArgs[i] != want[i] {
				t.Fatalf("button %q: args = %v, want %v", c.button, gotArgs, want)
			}
		}
	}
}

func TestInputMouseClick_UnknownButton(t *testing.T) {
	if err := InputMouseClick(0, 0, "double-super-click"); err == nil {
		t.Fatal("expected an error for an unrecognized button name")
	}
}

func TestInputType_Success(t *testing.T) {
	var gotArgs []string
	withFakeXdotool(t,
		func() (string, error) { return "/usr/bin/xdotool", nil },
		func(ctx context.Context, path string, args ...string) error { gotArgs = args; return nil },
	)
	if err := InputType("-rf hello"); err != nil {
		t.Fatalf("InputType: %v", err)
	}
	want := []string{"type", "--", "-rf hello"}
	if len(gotArgs) != len(want) {
		t.Fatalf("args = %v, want %v", gotArgs, want)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Fatalf("args = %v, want %v (the -- separator must precede text starting with '-')", gotArgs, want)
		}
	}
}
