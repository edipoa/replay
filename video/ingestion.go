package video

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ─── Ingestion Engine ─────────────────────────────────────────────────────────

// runIngestion is the outer retry loop. It restarts FFmpeg whenever the process
// exits (stream drop, network error, etc.) using exponential back-off.
func (e *Engine) runIngestion(ctx context.Context) {
	log := e.logger.With(slog.String("component", "ingestion"))
	backoff := reconnectBaseDelay
	attempt := 0

	for {
		// Bail out immediately if context is already cancelled.
		if ctx.Err() != nil {
			log.Info("stopping – context cancelled")
			return
		}

		attempt++
		log.Info("starting FFmpeg", slog.Int("attempt", attempt))

		err := e.runFFmpegIngestion(ctx)

		// Context cancellation is a clean shutdown, not an error.
		if ctx.Err() != nil {
			log.Info("FFmpeg stopped – context cancelled")
			return
		}

		if err != nil {
			log.Warn("FFmpeg exited with error – will reconnect",
				slog.Any("error", err),
				slog.Duration("backoff", backoff),
			)
		} else {
			// Unexpected clean exit (should not normally happen for a live stream).
			log.Warn("FFmpeg exited cleanly – will reconnect",
				slog.Duration("backoff", backoff),
			)
		}

		select {
		case <-ctx.Done():
			log.Info("stopping – context cancelled during backoff")
			return
		case <-time.After(backoff):
		}

		// Exponential back-off, capped at reconnectMaxDelay.
		backoff = min(backoff*2, reconnectMaxDelay)
	}
}

// runFFmpegIngestion starts a single FFmpeg process that reads the RTSP stream
// and writes rolling .ts segments into BufferDir. It blocks until FFmpeg exits.
//
// FFmpeg command equivalent:
//
//	ffmpeg -loglevel warning -rtsp_transport tcp -i <rtsp_url> \
//	       -c copy -f segment -segment_time 2 -segment_format mpegts \
//	       -strftime 1 -reset_timestamps 1 \
//	       /tmp/replay_buffer/seg_%Y%m%d_%H%M%S.ts
func (e *Engine) runFFmpegIngestion(ctx context.Context) error {
	segPath := filepath.Join(e.cfg.BufferDir, segmentFilePattern)

	args := []string{
		"-loglevel", "warning",
		"-rtsp_transport", "tcp",
		// Use the system wall clock for all timestamps instead of the camera's
		// broken PTS/DTS. This fixes irregular segment durations caused by
		// cameras that reset audio timestamps mid-stream.
		"-use_wallclock_as_timestamps", "1",
		"-i", e.cfg.RTSPUrl,
		// Copy streams without re-encoding for minimal CPU usage.
		"-c", "copy",
		// Segment muxer: one file per SegmentTime seconds.
		"-f", "segment",
		"-segment_time", strconv.Itoa(e.cfg.SegmentTime),
		"-segment_format", "mpegts",
		// Embed wall-clock time into each filename.
		"-strftime", "1",
		// Restart PTS from 0 in every segment to avoid playback drift.
		"-reset_timestamps", "1",
		segPath,
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)

	// Send SIGTERM on context cancellation instead of the default SIGKILL,
	// giving FFmpeg a chance to flush and close the current segment cleanly.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	// If FFmpeg ignores SIGTERM, force-kill after 5 s.
	cmd.WaitDelay = 5 * time.Second

	// Pipe stderr and forward each line through slog so operators can see
	// FFmpeg warnings/errors without grepping raw process output.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	// Read FFmpeg stderr in a separate goroutine to avoid blocking the pipe.
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			e.logger.Warn("ffmpeg", slog.String("component", "ingestion"), slog.String("msg", scanner.Text()))
		}
	}()

	return cmd.Wait()
}

// ─── Cleanup Ticker ───────────────────────────────────────────────────────────

// runCleanup runs a ticker that removes .ts segments older than cfg.BufferDur.
// This keeps the SSD from filling up during long recording sessions.
func (e *Engine) runCleanup(ctx context.Context) {
	log := e.logger.With(slog.String("component", "cleanup"))
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	log.Info("cleanup ticker started",
		slog.Duration("interval", cleanupInterval),
		slog.Duration("buffer_dur", e.cfg.BufferDur),
	)

	for {
		select {
		case <-ctx.Done():
			log.Info("stopping – context cancelled")
			return
		case <-ticker.C:
			e.cleanOldSegments()
		}
	}
}

// cleanOldSegments deletes every .ts file in BufferDir whose segment start time
// is older than now minus cfg.BufferDur.
func (e *Engine) cleanOldSegments() {
	cutoff := time.Now().Add(-e.cfg.BufferDur)
	log := e.logger.With(slog.String("component", "cleanup"), slog.Time("cutoff", cutoff))

	entries, err := os.ReadDir(e.cfg.BufferDir)
	if err != nil {
		log.Error("cannot read buffer dir", slog.Any("error", err))
		return
	}

	var removed, skipped int
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".ts" {
			continue
		}

		segTime, err := parseSegmentTime(entry.Name())
		if err != nil {
			// Fall back to file modification time for any unexpected filenames.
			info, err2 := entry.Info()
			if err2 != nil {
				continue
			}
			segTime = info.ModTime()
		}

		if segTime.Before(cutoff) {
			path := filepath.Join(e.cfg.BufferDir, entry.Name())
			if removeErr := os.Remove(path); removeErr != nil {
				log.Warn("failed to remove segment",
					slog.String("file", entry.Name()),
					slog.Any("error", removeErr),
				)
			} else {
				removed++
			}
		} else {
			skipped++
		}
	}

	if removed > 0 {
		log.Info("pruned old segments",
			slog.Int("removed", removed),
			slog.Int("retained", skipped),
		)
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// parseSegmentTime extracts the wall-clock start time from a segment filename.
//
// Expected format: seg_YYYYMMDD_HHMMSS.ts   (matches segmentFilePattern)
// Example:         seg_20240315_143022.ts   → 2024-03-15 14:30:22 local time
func parseSegmentTime(name string) (time.Time, error) {
	base := strings.TrimSuffix(name, ".ts")        // seg_20240315_143022
	ts := strings.TrimPrefix(base, "seg_")          // 20240315_143022

	if len(ts) != len("20060102_150405") {
		return time.Time{}, fmt.Errorf("unexpected segment filename: %q", name)
	}

	return time.ParseInLocation(segmentTimeLayout, ts, time.Local)
}
