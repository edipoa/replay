package video

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ─── Clipping Engine ──────────────────────────────────────────────────────────

// GenerateReplay builds a replay clip centred on triggerTime.
//
// Timeline:
//
//	triggerTime - PreCapture  ◄──── clip ────►  triggerTime + PostCapture
//
// The function blocks until:
//  1. The post-capture window has fully elapsed (triggerTime + PostCapture).
//  2. An extra flush buffer gives FFmpeg time to close the last segment.
//  3. The concat FFmpeg pass finishes writing the output .mp4.
//
// Returns the absolute path of the generated file, or an error.
// Cancelling ctx aborts the wait and the FFmpeg concat pass.
func (e *Engine) GenerateReplay(ctx context.Context, triggerTime time.Time) (string, error) {
	log := e.logger.With(
		slog.String("component", "clipping"),
		slog.Time("trigger", triggerTime),
	)

	// ── 1. Wait until all post-capture segments exist ────────────────────────
	captureEnd := triggerTime.Add(e.cfg.PostCapture)
	waitFor := time.Until(captureEnd) + extraFlushBuffer

	if waitFor > 0 {
		log.Info("waiting for post-capture window to close",
			slog.Duration("wait", waitFor),
			slog.Time("capture_end", captureEnd),
		)
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("GenerateReplay cancelled while waiting: %w", ctx.Err())
		case <-time.After(waitFor):
		}
	}

	// ── 2. Collect target segments ───────────────────────────────────────────
	windowStart := triggerTime.Add(-e.cfg.PreCapture)
	windowEnd := triggerTime.Add(e.cfg.PostCapture)

	log.Info("collecting segments",
		slog.Time("window_start", windowStart),
		slog.Time("window_end", windowEnd),
	)

	segments, err := e.collectSegments(windowStart, windowEnd)
	if err != nil {
		return "", fmt.Errorf("collect segments: %w", err)
	}
	if len(segments) == 0 {
		return "", fmt.Errorf(
			"no segments found in window [%s – %s]; buffer may be too short",
			windowStart.Format(time.TimeOnly),
			windowEnd.Format(time.TimeOnly),
		)
	}

	log.Info("segments collected", slog.Int("count", len(segments)))

	// ── 3. Write concat list ─────────────────────────────────────────────────
	concatPath := filepath.Join(
		e.cfg.BufferDir,
		fmt.Sprintf("concat_%d.txt", triggerTime.UnixMilli()),
	)
	if err := writeConcatFile(concatPath, segments); err != nil {
		return "", fmt.Errorf("write concat file: %w", err)
	}
	defer func() {
		if removeErr := os.Remove(concatPath); removeErr != nil {
			log.Warn("failed to remove concat file",
				slog.String("path", concatPath),
				slog.Any("error", removeErr),
			)
		}
	}()

	// ── 4. Run FFmpeg concat pass ─────────────────────────────────────────────
	outputPath := filepath.Join(
		e.cfg.OutputDir,
		fmt.Sprintf("replay_%s.mp4", triggerTime.Format("20060102_150405")),
	)

	log.Info("running FFmpeg concat", slog.String("output", outputPath))

	if err := e.runFFmpegConcat(ctx, concatPath, outputPath); err != nil {
		// Remove any partial output file to avoid leaving corrupted files.
		_ = os.Remove(outputPath)
		return "", fmt.Errorf("ffmpeg concat: %w", err)
	}

	log.Info("replay ready", slog.String("output", outputPath))
	return outputPath, nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// collectSegments returns the absolute paths of .ts files in BufferDir whose
// time range [segStart, segStart+SegmentTime) overlaps [windowStart, windowEnd].
// Results are sorted chronologically.
func (e *Engine) collectSegments(windowStart, windowEnd time.Time) ([]string, error) {
	entries, err := os.ReadDir(e.cfg.BufferDir)
	if err != nil {
		return nil, fmt.Errorf("read buffer dir: %w", err)
	}

	type segEntry struct {
		path string
		t    time.Time
	}

	segDur := time.Duration(e.cfg.SegmentTime) * time.Second
	var segs []segEntry

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".ts" {
			continue
		}

		segStart, err := parseSegmentTime(entry.Name())
		if err != nil {
			// Unknown filename format; skip silently (may be a leftover file).
			continue
		}
		segEnd := segStart.Add(segDur)

		// Overlap check: segment [segStart, segEnd) ∩ [windowStart, windowEnd) ≠ ∅
		if segStart.Before(windowEnd) && segEnd.After(windowStart) {
			segs = append(segs, segEntry{
				path: filepath.Join(e.cfg.BufferDir, entry.Name()),
				t:    segStart,
			})
		}
	}

	sort.Slice(segs, func(i, j int) bool {
		return segs[i].t.Before(segs[j].t)
	})

	paths := make([]string, len(segs))
	for i, s := range segs {
		paths[i] = s.path
	}
	return paths, nil
}

// writeConcatFile writes an FFmpeg concat demuxer input file.
//
// Format:
//
//	file '/absolute/path/to/seg.ts'
//	file '/absolute/path/to/seg2.ts'
//	...
func writeConcatFile(path string, segments []string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %q: %w", path, err)
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	for _, seg := range segments {
		// Escape single quotes inside the path (POSIX shell style).
		escaped := strings.ReplaceAll(seg, "'", `'\''`)
		if _, err := fmt.Fprintf(w, "file '%s'\n", escaped); err != nil {
			return err
		}
	}
	return w.Flush()
}

// runFFmpegConcat runs:
//
//	ffmpeg -f concat -safe 0 -i <concatPath> -c copy -movflags +faststart <outputPath>
//
// It blocks until FFmpeg finishes and returns a descriptive error (with FFmpeg's
// stderr) on failure.
func (e *Engine) runFFmpegConcat(ctx context.Context, concatPath, outputPath string) error {
	args := []string{
		"-loglevel", "warning",
		"-f", "concat",
		"-safe", "0", // Allow absolute paths in the concat file.
		"-i", concatPath,
		"-c", "copy",          // No re-encode; just remux into MP4.
		"-movflags", "+faststart", // Move MOOV atom to the front for streaming.
		"-y",        // Overwrite output without asking.
		outputPath,
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)

	// Capture combined output for error reporting.
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("exit error: %w\nffmpeg output:\n%s", err, string(out))
	}
	return nil
}
