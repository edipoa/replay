package video

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"
)

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	return &Engine{
		cfg:    Config{BufferDir: t.TempDir(), SegmentTime: 2},
		logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1})),
	}
}

func TestIsStalled(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	threshold := 10 * time.Second

	cases := []struct {
		name        string
		now, newest time.Time
		hasSegments bool
		want        bool
	}{
		{"no segments yet, within grace", start.Add(5 * time.Second), time.Time{}, false, false},
		{"no segments yet, past grace", start.Add(11 * time.Second), time.Time{}, false, true},
		{"fresh segment", start.Add(20 * time.Second), start.Add(19 * time.Second), true, false},
		{"stale segment (ffmpeg wedged)", start.Add(20 * time.Second), start.Add(5 * time.Second), true, true},
	}
	for _, c := range cases {
		if got := isStalled(c.now, start, c.newest, c.hasSegments, threshold); got != c.want {
			t.Errorf("%s: isStalled = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNewestSegmentTime(t *testing.T) {
	e := newTestEngine(t)

	if _, ok := e.newestSegmentTime(); ok {
		t.Fatal("expected ok=false on empty buffer dir")
	}

	const older, newer = "seg_20240101_000000.ts", "seg_20240101_000010.ts"
	for _, name := range []string{older, newer} {
		if err := os.WriteFile(e.cfg.BufferDir+"/"+name, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, ok := e.newestSegmentTime()
	if !ok {
		t.Fatal("expected ok=true once segments exist")
	}
	want, _ := parseSegmentTime(newer)
	if !got.Equal(want) {
		t.Fatalf("newestSegmentTime = %v, want %v", got, want)
	}
}

// TestWatchForStallKillsWhenNothingAppears is a smoke test for the goroutine
// wiring: an empty buffer dir must trigger kill() once the threshold passes.
func TestWatchForStallKillsWhenNothingAppears(t *testing.T) {
	e := newTestEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	killed := make(chan struct{})
	kill := func() { close(killed) }

	go e.watchForStallEvery(ctx, kill, 30*time.Millisecond, 10*time.Millisecond)

	select {
	case <-killed:
	case <-time.After(time.Second):
		t.Fatal("watchForStallEvery did not kill FFmpeg after threshold with no segments")
	}
}
