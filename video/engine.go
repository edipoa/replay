// Package video manages continuous RTSP ingestion and on-demand replay generation.
// It has no knowledge of delivery mechanisms (Telegram, physical buttons, etc.).
package video

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// ─── Constants ────────────────────────────────────────────────────────────────

const (
	// segmentFilePattern is the strftime(3) pattern passed to FFmpeg -strftime.
	// Produces filenames like: seg_20240315_143022.ts
	segmentFilePattern = "seg_%Y%m%d_%H%M%S.ts"

	// segmentTimeLayout is the Go time.Parse counterpart of segmentFilePattern.
	segmentTimeLayout = "20060102_150405"

	cleanupInterval   = 10 * time.Second
	reconnectBaseDelay = 2 * time.Second
	reconnectMaxDelay  = 30 * time.Second

	// extraFlushBuffer gives FFmpeg time to finish writing the last segment
	// before the clipping engine tries to read it.
	extraFlushBuffer = 3 * time.Second
)

// ─── Config ───────────────────────────────────────────────────────────────────

// Config holds all tuneable parameters for the Engine.
// Zero values are replaced with sensible defaults by New.
type Config struct {
	// RTSPUrl is the source stream, e.g. "rtsp://192.168.1.10:554/live".
	RTSPUrl string

	// BufferDir is where rolling .ts segments are written.
	// Default: /tmp/replay_buffer
	BufferDir string

	// OutputDir is where finished replay .mp4 files are placed.
	// Default: /tmp/replays
	OutputDir string

	// SegmentTime is the duration of each .ts segment in seconds.
	// Default: 2
	SegmentTime int

	// BufferDur is the rolling window kept on disk (older segments are deleted).
	// Default: 60s
	BufferDur time.Duration

	// PreCapture is how far before the trigger to include in the replay.
	// Default: 15s
	PreCapture time.Duration

	// PostCapture is how far after the trigger to include in the replay.
	// Default: 10s
	PostCapture time.Duration
}

func (c *Config) applyDefaults() {
	if c.BufferDir == "" {
		c.BufferDir = "/tmp/replay_buffer"
	}
	if c.OutputDir == "" {
		c.OutputDir = "/tmp/replays"
	}
	if c.SegmentTime == 0 {
		c.SegmentTime = 2
	}
	if c.BufferDur == 0 {
		c.BufferDur = 60 * time.Second
	}
	if c.PreCapture == 0 {
		c.PreCapture = 15 * time.Second
	}
	if c.PostCapture == 0 {
		c.PostCapture = 10 * time.Second
	}
}

// ─── Engine ───────────────────────────────────────────────────────────────────

// Engine orchestrates continuous video ingestion and replay generation.
// Create one with New, then call Start to begin background work.
type Engine struct {
	cfg    Config
	logger *slog.Logger
	wg     sync.WaitGroup // tracks background goroutines launched by Start
}

// New validates configuration, creates required directories, and returns an Engine.
func New(cfg Config, logger *slog.Logger) (*Engine, error) {
	cfg.applyDefaults()

	if cfg.RTSPUrl == "" {
		return nil, fmt.Errorf("video: RTSPUrl must not be empty")
	}
	if logger == nil {
		logger = slog.Default()
	}

	if err := os.MkdirAll(cfg.BufferDir, 0o755); err != nil {
		return nil, fmt.Errorf("video: create buffer dir %q: %w", cfg.BufferDir, err)
	}
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		return nil, fmt.Errorf("video: create output dir %q: %w", cfg.OutputDir, err)
	}

	return &Engine{cfg: cfg, logger: logger}, nil
}

// Start launches the Ingestion Engine (FFmpeg + cleanup ticker) as background
// goroutines. Cancel ctx to stop all background work gracefully.
// Start returns immediately; call Wait to block until all goroutines exit.
func (e *Engine) Start(ctx context.Context) {
	e.logger.Info("video engine starting",
		"rtsp_url", e.cfg.RTSPUrl,
		"buffer_dir", e.cfg.BufferDir,
		"output_dir", e.cfg.OutputDir,
		"segment_time_s", e.cfg.SegmentTime,
		"buffer_dur", e.cfg.BufferDur,
	)
	e.wg.Add(2)
	go func() { defer e.wg.Done(); e.runIngestion(ctx) }()
	go func() { defer e.wg.Done(); e.runCleanup(ctx) }()
}

// Wait blocks until all background goroutines started by Start have returned.
// Typically called after cancelling the context passed to Start:
//
//	cancel()
//	engine.Wait()
func (e *Engine) Wait() {
	e.wg.Wait()
	e.logger.Info("video engine stopped")
}
