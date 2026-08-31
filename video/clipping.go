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

	"github.com/edipo/replay-saas/internal/obs"
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

	// Ingestion writes segment filenames using wall-clock time at the moment
	// FFmpeg opens each file, so a wedged/backlogged FFmpeg (see watchForStall)
	// can still be rotating files while lagging behind real time. If the
	// newest segment on disk doesn't even reach this trigger's own window end,
	// the clip we're about to build is likely to contain stale footage — flag
	// it loudly instead of only surfacing as "wrong video" on the site.
	if newest, ok := e.newestSegmentTime(); ok && newest.Before(windowEnd) {
		log.Warn("ingestion appears behind — clip may contain stale footage",
			slog.Time("newest_segment", newest),
			slog.Duration("lag", windowEnd.Sub(newest)),
		)
		obs.Event(obs.Warn, "ingest.behind", e.cfg.CameraID,
			"ingestão atrasada no momento do clipe — footage pode estar velha",
			slog.Duration("lag", windowEnd.Sub(newest)))
	}

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
		e.cfg.OutputDir, "pending",
		fmt.Sprintf("replay_%s_%s.mp4", triggerTime.Format("20060102_150405"), e.cfg.CameraID),
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

// GeneratePreview creates an animated GIF from mp4Path covering the full clip (5 fps, 640 px wide)
// and writes it to outputPath.
func (e *Engine) GeneratePreview(ctx context.Context, mp4Path, outputPath string) error {
	return e.runFFmpegPass(ctx, "gif encode", []string{
		"-loglevel", "warning",
		"-i", mp4Path,
		// flags=fast_bilinear: lanczos is noticeably heavier on the low-power
		// deploy hardware (2011 iMac, i5-2400S) for a barely-visible gain on a
		// small preview GIF.
		"-vf", "fps=5,scale=640:-1:flags=fast_bilinear,split[s0][s1];[s0]palettegen[p];[s1][p]paletteuse",
		"-y",
		outputPath,
	})
}

// runFFmpegPass runs a single ffmpeg invocation and returns a labelled error on failure.
func (e *Engine) runFFmpegPass(ctx context.Context, passName string, args []string) error {
	if out, err := exec.CommandContext(ctx, e.cfg.FFmpegBin, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w\n%s", passName, err, out)
	}
	return nil
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

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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
		// Forward slashes are required: FFmpeg's concat demuxer treats backslash
		// paths as relative on Windows (no drive letter → prepends concat dir).
		seg = filepath.ToSlash(seg)
		// Escape single quotes inside the path (POSIX shell style).
		escaped := strings.ReplaceAll(seg, "'", `'\''`)
		if _, err := fmt.Fprintf(w, "file '%s'\n", escaped); err != nil {
			return err
		}
	}
	return w.Flush()
}

// runFFmpegConcat concatenates segments into a final MP4 in two passes:
//
//  1. Concat with -c copy into a temp file — preserves correct timestamps
//     (re-encoding directly from broken-DTS segments produces wrong durations).
//  2. Re-encode the clean temp file with libx264 to reduce size for Telegram.
func (e *Engine) runFFmpegConcat(ctx context.Context, concatPath, outputPath string) error {
	// ── Pass 1: concat → temp .ts with stream copy ───────────────────────────
	// Keep the intermediate as MPEG-TS (not MP4) so H264 stays in Annex B
	// format throughout. Converting TS→MP4 in stream-copy mode requires a
	// bitstream format change (Annex B → AVCC) that some FFmpeg builds
	// mishandle, producing an MP4 with garbage NAL unit sizes that cause
	// nearly all frames to fail decoding in pass 2.
	tempPath := outputPath + ".tmp.ts"
	defer os.Remove(tempPath)

	pass1 := []string{
		"-loglevel", "warning",
		"-f", "concat",
		"-safe", "0",
		"-i", concatPath,
		"-c", "copy",
		"-y",
		tempPath,
	}
	if err := e.runFFmpegPass(ctx, "concat pass", pass1); err != nil {
		return err
	}

	// ── Pass 2: re-encode temp .ts → final MP4 for Telegram ──────────────────
	// Reading from MPEG-TS (native Annex B H264) avoids the AVCC decode errors
	// that occur when reading a stream-copied MP4 with broken extradata.
	// -fflags +discardcorrupt: skip malformed AAC frames at TS segment
	// boundaries (common with IP Webcam / phone RTSP sources).
	// -max_error_rate 1.0: don't abort on high decode error rate (newer FFmpeg
	// default is 0.666 which kills the process before producing any output).
	hasWatermark := fileExists(e.cfg.WatermarkPath)
	hasLogo := fileExists(e.cfg.LogoPath)
	hasMusic := e.cfg.BackgroundMusicPath != "" && fileExists(e.cfg.BackgroundMusicPath)

	// When music is requested, pass 2 encodes to a temp file; pass 3 muxes the
	// music in. This avoids filter-graph buffer overflows that occur when a
	// stream_loop audio input races against corrupt video frames.
	pass2Out := outputPath
	tempVideoPath := ""
	if hasMusic {
		// Keep the intermediate file out of "pending/": the delivery queue
		// sweeps that dir every 30s and matches anything ending in ".mp4"
		// (filepath.Ext only checks the last dot), so a file left there
		// mid-pipeline could get delivered/moved before pass 3 reads it.
		tempVideoPath = filepath.Join(e.cfg.OutputDir, filepath.Base(outputPath)+".tmp.mp4")
		pass2Out = tempVideoPath
		defer os.Remove(tempVideoPath)
	}

	// ── Pass 2: re-encode with overlays ──────────────────────────────────────
	pass2 := []string{
		"-fflags", "+discardcorrupt",
		"-max_error_rate", "1.0",
		"-loglevel", "warning",
		"-i", tempPath,
	}

	if hasWatermark {
		e.logger.Info("applying watermark", slog.String("path", e.cfg.WatermarkPath))
		pass2 = append(pass2, "-i", e.cfg.WatermarkPath)
	} else {
		e.logger.Warn("watermark not found, skipping", slog.String("path", e.cfg.WatermarkPath))
	}
	if hasLogo {
		e.logger.Info("applying arena logo", slog.String("path", e.cfg.LogoPath))
		pass2 = append(pass2, "-i", e.cfg.LogoPath)
	} else {
		e.logger.Warn("arena logo not found, skipping", slog.String("path", e.cfg.LogoPath))
	}

	// Build the video portion of the filter graph.
	// watermark → bottom-left  (10:H-h-10)
	// logo      → bottom-right (W-w-10:H-h-10)
	sc := fmt.Sprintf("scale=%d:-2", e.cfg.ClipWidth)
	switch {
	case hasWatermark && hasLogo:
		pass2 = append(pass2,
			"-filter_complex", "[0:v]"+sc+"[sc];[2:v]scale=80:-1[logo];[sc][1:v]overlay=10:H-h-10[wm];[wm][logo]overlay=W-w-10:H-h-10[outv]",
			"-map", "[outv]",
			"-map", "0:a?",
		)
	case hasWatermark:
		pass2 = append(pass2,
			"-filter_complex", "[0:v]"+sc+"[sc];[sc][1:v]overlay=10:H-h-10[outv]",
			"-map", "[outv]",
			"-map", "0:a?",
		)
	case hasLogo:
		pass2 = append(pass2,
			"-filter_complex", "[0:v]"+sc+"[sc];[1:v]scale=80:-1[logo];[sc][logo]overlay=W-w-10:H-h-10[outv]",
			"-map", "[outv]",
			"-map", "0:a?",
		)
	default:
		pass2 = append(pass2, "-vf", sc)
	}

	pass2 = append(pass2,
		"-c:v", "libx264", "-crf", "28", "-preset", "ultrafast",
		"-c:a", "aac", "-b:a", "128k",
	)
	if !hasMusic {
		// This pass writes straight to outputPath, so make it stream-friendly.
		pass2 = append(pass2, "-movflags", "+faststart")
	}
	pass2 = append(pass2, "-y", pass2Out)
	if err := e.runFFmpegPass(ctx, "encode pass", pass2); err != nil {
		return err
	}

	// ── Pass 3 (optional): mux background music ───────────────────────────────
	// Video is already encoded, so this pass uses stream-copy for video (fast).
	// Mixes the clip's own audio with the music bed instead of replacing it —
	// -shortest stops both when the video ends.
	if hasMusic {
		e.logger.Info("applying background music", slog.String("path", e.cfg.BackgroundMusicPath))
		// The camera clip may have no audio track at all; [0:a] would then
		// match no stream and fail the whole filtergraph. Probe for it and
		// fall back to using the music alone as the audio track.
		filterComplex := "[0:a]volume=1.0[a0];[1:a]volume=0.07[a1];[a0][a1]amix=inputs=2:duration=first:dropout_transition=0[aout]"
		if !e.hasAudioStream(ctx, tempVideoPath) {
			e.logger.Warn("clip has no audio track, using music as sole audio", slog.String("camera", e.cfg.CameraID))
			filterComplex = "[1:a]volume=1.0[aout]"
		}
		pass3 := []string{
			"-loglevel", "warning",
			"-i", tempVideoPath,
			"-stream_loop", "-1",
			"-i", e.cfg.BackgroundMusicPath,
			"-filter_complex", filterComplex,
			"-map", "0:v",
			"-map", "[aout]",
			"-c:v", "copy",
			"-c:a", "aac", "-b:a", "128k",
			"-shortest",
			"-movflags", "+faststart",
			"-y",
			outputPath,
		}
		if err := e.runFFmpegPass(ctx, "music pass", pass3); err != nil {
			return err
		}
	}

	return nil
}

// hasAudioStream uses ffmpeg to probe the file and returns true if it contains at least one audio stream.
// It relies on e.cfg.FFmpegBin instead of ffprobe, as only the ffmpeg binary is guaranteed to be bundled.
func (e *Engine) hasAudioStream(ctx context.Context, path string) bool {
	cmd := exec.CommandContext(ctx, e.cfg.FFmpegBin, "-i", path)
	out, _ := cmd.CombinedOutput()
	outStr := string(out)
	for _, line := range strings.Split(outStr, "\n") {
		// Look for a stream definition, e.g.:
		//   Stream #0:1(und): Audio: aac (LC)...
		if strings.Contains(line, "Stream #") && strings.Contains(line, "Audio:") {
			return true
		}
	}
	return false
}
