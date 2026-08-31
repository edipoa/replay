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

	"github.com/edipo/replay-saas/internal/obs"
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
		obs.Event(obs.Info, "ingest.started", e.cfg.CameraID, "iniciando FFmpeg de ingestão",
			slog.Int("attempt", attempt))

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
			obs.Event(obs.Warn, "ingest.exited", e.cfg.CameraID, "FFmpeg de ingestão caiu — vai reconectar",
				slog.Any("error", err), slog.Duration("backoff", backoff))
		} else {
			// Unexpected clean exit (should not normally happen for a live stream).
			log.Warn("FFmpeg exited cleanly – will reconnect",
				slog.Duration("backoff", backoff),
			)
			obs.Event(obs.Warn, "ingest.exited", e.cfg.CameraID, "FFmpeg de ingestão saiu limpo (inesperado) — vai reconectar",
				slog.Duration("backoff", backoff))
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

	// procCtx lets the stall watchdog kill FFmpeg on its own, independent of
	// the outer shutdown context, so the normal exit/backoff/reconnect path
	// below handles it exactly like any other FFmpeg crash.
	procCtx, killFFmpeg := context.WithCancel(ctx)
	defer killFFmpeg()

	args := []string{
		"-loglevel", "warning",
		"-rtsp_transport", "tcp",
		// Regenerate PTS/DTS from scratch — fixes "Non-monotonic DTS" warnings
		// that appear when the camera firmware produces irregular timestamps.
		"-fflags", "+genpts",
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

	cmd := exec.CommandContext(procCtx, e.cfg.FFmpegBin, args...)
	// Force UTC so -strftime filenames match the UTC wall clock used everywhere else.
	cmd.Env = append(os.Environ(), "TZ=UTC")

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

	go e.watchForStall(procCtx, killFFmpeg)

	return cmd.Wait()
}

// watchForStall kills FFmpeg if no new segment file has appeared in BufferDir
// for stallThreshold. FFmpeg can wedge on malformed camera timestamps (e.g.
// "Non-monotonic DTS") and stop rotating segments without ever exiting or
// logging an error — invisible to the normal exit-based reconnect logic,
// while the cleanup ticker keeps deleting the aging segment until the buffer
// dir is permanently empty.
func (e *Engine) watchForStall(ctx context.Context, kill context.CancelFunc) {
	threshold := max(3*time.Duration(e.cfg.SegmentTime)*time.Second, 10*time.Second)
	e.watchForStallEvery(ctx, kill, threshold, cleanupInterval)
}

// watchForStallEvery is watchForStall with threshold/checkInterval as
// parameters so the decision logic can be exercised in tests without
// waiting on real timers.
func (e *Engine) watchForStallEvery(ctx context.Context, kill context.CancelFunc, threshold, checkInterval time.Duration) {
	log := e.logger.With(slog.String("component", "ingestion"))

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	start := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			newest, ok := e.newestSegmentTime()
			if isStalled(time.Now(), start, newest, ok, threshold) {
				log.Error("segment output stalled – killing FFmpeg",
					slog.Bool("ever_produced_segment", ok),
					slog.Duration("threshold", threshold),
				)
				obs.Event(obs.Critical, "ingest.stalled", e.cfg.CameraID,
					"ingestão travada (FFmpeg parou de rotacionar segmentos) — matando pra reconectar",
					slog.Bool("ever_produced_segment", ok), slog.Duration("threshold", threshold))
				kill()
				return
			}
		}
	}
}

// isStalled reports whether ingestion should be considered wedged: either no
// segment has ever appeared within threshold of start, or the newest segment
// is older than threshold (FFmpeg stopped rotating but didn't exit).
func isStalled(now, start, newest time.Time, hasSegments bool, threshold time.Duration) bool {
	if !hasSegments {
		return now.Sub(start) > threshold
	}
	return now.Sub(newest) > threshold
}

// newestSegmentTime returns the most recent segment start time found in
// BufferDir, or ok=false if the directory has no valid segment files yet.
func (e *Engine) newestSegmentTime() (newest time.Time, ok bool) {
	return NewestSegmentTime(e.cfg.BufferDir)
}

// NewestSegmentTime returns the most recent segment start time (parsed from the
// filename) found in dir, or ok=false when dir has no valid segment files yet.
// Exported so the observability layer can gauge per-camera buffer freshness.
func NewestSegmentTime(dir string) (newest time.Time, ok bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}, false
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".ts" {
			continue
		}
		segTime, err := parseSegmentTime(entry.Name())
		if err != nil || segTime.Before(newest) {
			continue
		}
		newest, ok = segTime, true
	}
	return newest, ok
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
	cutoff := time.Now().UTC().Add(-e.cfg.BufferDur)
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
	base := strings.TrimSuffix(name, ".ts") // seg_20240315_143022
	ts := strings.TrimPrefix(base, "seg_")  // 20240315_143022

	if len(ts) != len("20060102_150405") {
		return time.Time{}, fmt.Errorf("unexpected segment filename: %q", name)
	}

	return time.Parse(segmentTimeLayout, ts)
}
