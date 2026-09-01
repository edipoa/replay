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
	"syscall"
	"time"

	"github.com/edipo/replay-saas/internal/obs"
)

// ─── Live HLS Engine ──────────────────────────────────────────────────────────
//
// A second, independent FFmpeg per camera that remuxes (no re-encode) the
// camera substream into a small rolling HLS window under <LiveDir>/<CameraID>/.
// Kept separate from runIngestion so a live-stream failure never touches the
// clip pipeline, and because it reads a different URL (the low-bitrate
// substream that fits the venue uplink).

// runLiveHLS is the outer retry loop, mirroring runIngestion: restart FFmpeg
// with exponential back-off whenever it exits.
func (e *Engine) runLiveHLS(ctx context.Context) {
	log := e.logger.With(slog.String("component", "live"))
	liveDir := filepath.Join(e.cfg.LiveDir, e.cfg.CameraID)

	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		log.Error("cannot create live dir – live stream disabled",
			slog.String("dir", liveDir), slog.Any("error", err))
		return
	}

	backoff := reconnectBaseDelay
	attempt := 0

	for {
		if ctx.Err() != nil {
			return
		}

		attempt++
		log.Info("starting live FFmpeg", slog.Int("attempt", attempt))
		obs.Event(obs.Info, "live.started", e.cfg.CameraID, "iniciando FFmpeg da transmissão ao vivo",
			slog.Int("attempt", attempt))

		err := e.runFFmpegLiveHLS(ctx, liveDir)

		if ctx.Err() != nil {
			return
		}

		if err != nil {
			log.Warn("live FFmpeg exited – will reconnect",
				slog.Any("error", err), slog.Duration("backoff", backoff))
			obs.Event(obs.Warn, "live.exited", e.cfg.CameraID,
				"FFmpeg da transmissão ao vivo caiu — vai reconectar",
				slog.Any("error", err), slog.Duration("backoff", backoff))
		} else {
			log.Warn("live FFmpeg exited cleanly – will reconnect", slog.Duration("backoff", backoff))
			obs.Event(obs.Warn, "live.exited", e.cfg.CameraID,
				"FFmpeg da transmissão ao vivo saiu limpo (inesperado) — vai reconectar",
				slog.Duration("backoff", backoff))
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, reconnectMaxDelay)
	}
}

// runFFmpegLiveHLS runs one copy-only FFmpeg that reads the substream and writes
// a rolling HLS window (hls_list_size segments, older ones deleted by FFmpeg).
// Segment filenames reuse the seg_<strftime>.ts convention so the same stall
// watchdog and NewestSegmentTime parser work here unchanged.
//
//	ffmpeg -loglevel warning -rtsp_transport tcp -fflags +genpts -i <substream> \
//	       -c copy -f hls -hls_time <seg> -hls_list_size 6 \
//	       -hls_flags delete_segments+omit_endlist+program_date_time -strftime 1 \
//	       -hls_segment_filename <liveDir>/seg_%Y%m%d_%H%M%S.ts \
//	       <liveDir>/index.m3u8
//
// program_date_time writes an EXT-X-PROGRAM-DATE-TIME tag per segment so the
// browser can line the two cameras up on the wall clock (both FFmpeg run here
// with TZ=UTC, so their timestamps share one clock). See internal/obs/live.go.
func (e *Engine) runFFmpegLiveHLS(ctx context.Context, liveDir string) error {
	procCtx, killFFmpeg := context.WithCancel(ctx)
	defer killFFmpeg()

	args := []string{
		"-loglevel", "warning",
		"-rtsp_transport", "tcp",
		"-fflags", "+genpts",
		"-i", e.cfg.LiveRTSPUrl,
		"-c", "copy",
		"-f", "hls",
		"-hls_time", strconv.Itoa(e.cfg.SegmentTime),
		"-hls_list_size", "6",
		"-hls_flags", "delete_segments+omit_endlist+program_date_time",
		"-hls_segment_type", "mpegts",
		"-strftime", "1",
		"-hls_segment_filename", filepath.Join(liveDir, segmentFilePattern),
		filepath.Join(liveDir, "index.m3u8"),
	}

	cmd := exec.CommandContext(procCtx, e.cfg.FFmpegBin, args...)
	cmd.Env = append(os.Environ(), "TZ=UTC")
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 5 * time.Second

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			e.logger.Warn("ffmpeg", slog.String("component", "live"), slog.String("msg", scanner.Text()))
		}
	}()

	// These budget cameras can wedge FFmpeg silently (Non-monotonic DTS); the
	// same watchdog used for ingestion kills it so the loop above reconnects.
	go e.watchForStall(procCtx, killFFmpeg, liveDir, "live")

	return cmd.Wait()
}
