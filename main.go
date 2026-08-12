package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/edipo/replay-saas/delivery"
	"github.com/edipo/replay-saas/internal/envutil"
	"github.com/edipo/replay-saas/internal/joystick"
	"github.com/edipo/replay-saas/internal/selftest"
	"github.com/edipo/replay-saas/upload"
	"github.com/edipo/replay-saas/video"
)

// ─── Config ───────────────────────────────────────────────────────────────────

type cameraConfig struct {
	ID        string // "cam1", "cam2", …
	RTSPUrl   string
	BufferDir string
}

type appConfig struct {
	// Video ingestion (one entry per configured camera)
	Cameras     []cameraConfig
	OutputDir   string
	SegmentTime int

	// Delivery
	BotToken            string
	ChatID              string
	AgendaPath          string
	TelegramLogPath     string
	WatermarkPath       string
	LogoPath            string
	BackgroundMusicPath string

	// Hardware
	JoystickID int // index of the USB joystick (0 = first device)
	DebounceMs int

	// Dev/test
	Simulate bool
}

func loadConfig() (appConfig, error) {
	simulate := os.Getenv("REPLAY_SIMULATE") == "true"

	cfg := appConfig{
		OutputDir:       envutil.Or("REPLAY_OUTPUT_DIR", "/tmp/replays"),
		SegmentTime:     envutil.IntOr("REPLAY_SEGMENT_TIME_S", 2),
		AgendaPath:      envutil.Or("REPLAY_AGENDA_PATH", agendaDefault(simulate)),
		TelegramLogPath: envutil.Or("REPLAY_TELEGRAM_LOG", "/tmp/telegram.log"),
		WatermarkPath:       envutil.Or("REPLAY_WATERMARK_PATH", "watermark.png"),
		LogoPath:            envutil.Or("REPLAY_LOGO_PATH", "logo.png"),
		BackgroundMusicPath: os.Getenv("REPLAY_MUSIC_PATH"),
		JoystickID:      envutil.IntOr("REPLAY_JOYSTICK_ID", 0),
		DebounceMs:      envutil.IntOr("REPLAY_DEBOUNCE_MS", 2000),
		Simulate:        simulate,
	}

	// ── Camera discovery ──────────────────────────────────────────────────────
	// Supports REPLAY_CAM_1_RTSP_URL, REPLAY_CAM_2_RTSP_URL, …
	// REPLAY_RTSP_URL and REPLAY_BUFFER_DIR are accepted as aliases for cam1
	// so existing single-camera deployments need no changes.
	// ponytail: scans a fixed range instead of stopping at the first gap, so
	// REPLAY_CAM_2_RTSP_URL still works even if cam1 is unset; raise
	// maxCameraSlots if a rig ever needs more than 8 cameras.
	const maxCameraSlots = 8
	for i := 1; i <= maxCameraSlots; i++ {
		id := fmt.Sprintf("cam%d", i)
		url := os.Getenv(fmt.Sprintf("REPLAY_CAM_%d_RTSP_URL", i))
		if url == "" && i == 1 {
			url = os.Getenv("REPLAY_RTSP_URL")
		}
		if url == "" {
			continue
		}

		bufDir := os.Getenv(fmt.Sprintf("REPLAY_CAM_%d_BUFFER_DIR", i))
		if bufDir == "" && i == 1 {
			bufDir = envutil.Or("REPLAY_BUFFER_DIR", "/tmp/replay_buffer")
		}
		if bufDir == "" {
			bufDir = fmt.Sprintf("/tmp/replay_buffer_%d", i)
		}

		cfg.Cameras = append(cfg.Cameras, cameraConfig{ID: id, RTSPUrl: url, BufferDir: bufDir})
	}

	if len(cfg.Cameras) == 0 {
		return appConfig{}, fmt.Errorf("required environment variable REPLAY_CAM_1_RTSP_URL (or REPLAY_RTSP_URL) is not set")
	}

	cfg.BotToken = os.Getenv("REPLAY_BOT_TOKEN")
	cfg.ChatID = os.Getenv("REPLAY_CHAT_ID")

	return cfg, nil
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	_ = godotenv.Load()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("fatal", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	// ── 1. Config ─────────────────────────────────────────────────────────────
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// ── 2. FFmpeg binary ──────────────────────────────────────────────────────
	ffmpegBin := findFFmpeg()
	if ffmpegBin == "ffmpeg" {
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			return fmt.Errorf("no working ffmpeg found (bundled binary failed its self-test and none on PATH) — run: sudo apt install ffmpeg")
		}
	}
	logger.Info("ffmpeg binary", slog.String("path", ffmpegBin))

	// ── 3. Video engines (one per camera) ────────────────────────────────────
	var engines []*video.Engine
	for _, cam := range cfg.Cameras {
		eng, err := video.New(video.Config{
			RTSPUrl:             cam.RTSPUrl,
			BufferDir:           cam.BufferDir,
			OutputDir:           cfg.OutputDir,
			SegmentTime:         cfg.SegmentTime,
			FFmpegBin:           ffmpegBin,
			WatermarkPath:       cfg.WatermarkPath,
			LogoPath:            cfg.LogoPath,
			BackgroundMusicPath: cfg.BackgroundMusicPath,
			CameraID:            cam.ID,
		}, logger.With(slog.String("camera", cam.ID)))
		if err != nil {
			return fmt.Errorf("video engine %s: %w", cam.ID, err)
		}
		engines = append(engines, eng)
	}

	// ── 4. Delivery bot ───────────────────────────────────────────────────────
	telegramLogFile, err := os.OpenFile(cfg.TelegramLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open telegram log %q: %w", cfg.TelegramLogPath, err)
	}
	defer telegramLogFile.Close()

	telegramLogger := slog.New(slog.NewJSONHandler(telegramLogFile, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	var bot *delivery.Bot
	if cfg.BotToken != "" && cfg.ChatID != "" {
		b, err := delivery.New(delivery.Config{
			BotToken:   cfg.BotToken,
			ChatID:     cfg.ChatID,
			AgendaPath: cfg.AgendaPath,
		}, telegramLogger)
		if err != nil {
			return fmt.Errorf("delivery bot: %w", err)
		}
		bot = b
		logger.Info("telegram delivery enabled")
	} else {
		logger.Warn("telegram delivery disabled (REPLAY_BOT_TOKEN/REPLAY_CHAT_ID not set)")
	}

	// ── 5. Upload client (optional — only when R2 vars are set) ──────────────
	var uploader *upload.Client
	if os.Getenv("REPLAY_R2_ACCOUNT_ID") != "" {
		u, err := upload.New(upload.Config{
			AccountID:   os.Getenv("REPLAY_R2_ACCOUNT_ID"),
			AccessKeyID: os.Getenv("REPLAY_R2_ACCESS_KEY_ID"),
			SecretKey:   os.Getenv("REPLAY_R2_SECRET_ACCESS_KEY"),
			Bucket:      os.Getenv("REPLAY_R2_BUCKET"),
			BackendURL:  os.Getenv("REPLAY_BACKEND_URL"),
			APIKey:      os.Getenv("REPLAY_BACKEND_API_KEY"),
		}, logger)
		if err != nil {
			return fmt.Errorf("upload client: %w", err)
		}
		uploader = u
		logger.Info("r2 upload enabled", slog.String("bucket", os.Getenv("REPLAY_R2_BUCKET")))
	}

	// ── 6. Context + signal handling ─────────────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// ── 6. Start video ingestion ──────────────────────────────────────────────
	for i, eng := range engines {
		eng.Start(ctx)
		logger.Info("camera started",
			slog.String("id", cfg.Cameras[i].ID),
			slog.String("rtsp_url", cfg.Cameras[i].RTSPUrl),
			slog.String("buffer_dir", cfg.Cameras[i].BufferDir),
		)
	}

	// ── 6b. Start delivery queue worker ──────────────────────────────────────
	go delivery.RunQueueWorker(ctx, cfg.OutputDir, bot, telegramLogger)

	// ── 7. Button press handler ───────────────────────────────────────────────
	//
	// pressQueue (cap 1) holds at most one pending trigger while a replay is
	// in progress. A second press is only enqueued if it arrives >15 s after
	// the trigger that started the current replay — anything sooner is treated
	// as an accidental re-press and silently dropped.
	var (
		replayActive  atomic.Bool
		lastTriggerNs atomic.Int64
		pressQueue    = make(chan time.Time, 1)
	)

	onPress := func(triggerTime time.Time) {
		if replayActive.CompareAndSwap(false, true) {
			lastTriggerNs.Store(triggerTime.UnixNano())
			go func() {
				t := triggerTime
				for {
					// Fire all cameras concurrently; wait for all before
					// accepting the next queued press or releasing the lock.
					var wg sync.WaitGroup
					for i, eng := range engines {
						wg.Add(1)
						go func(e *video.Engine, camID string) {
							defer wg.Done()
							runReplay(ctx, e, camID, uploader, logger, t)
						}(eng, cfg.Cameras[i].ID)
					}
					wg.Wait()

					if ctx.Err() != nil {
						replayActive.Store(false)
						return
					}
					select {
					case t = <-pressQueue:
						lastTriggerNs.Store(t.UnixNano())
					default:
						replayActive.Store(false)
						return
					}
				}
			}()
			return
		}

		since := triggerTime.Sub(time.Unix(0, lastTriggerNs.Load()))
		if since < 15*time.Second {
			return
		}
		select {
		case pressQueue <- triggerTime:
			logger.Info("clip queued for next generation", slog.Time("trigger", triggerTime))
		default:
			logger.Warn("press queue full, ignoring", slog.Time("trigger", triggerTime))
		}
	}

	// ── 8. Input listener goroutine ───────────────────────────────────────────
	debounce := time.Duration(cfg.DebounceMs) * time.Millisecond
	listenerDone := make(chan error, 1)

	if cfg.Simulate {
		go func() {
			listenerDone <- runStdinListener(ctx, debounce, onPress)
		}()
		logger.Info("replay agent ready (simulate)",
			slog.String("trigger", "press Enter"),
			slog.Int("debounce_ms", cfg.DebounceMs),
		)
	} else {
		go func() {
			listenerDone <- runJoystickListener(ctx, cfg.JoystickID, debounce, onPress, logger)
		}()
		logger.Info("replay agent ready",
			slog.Int("joystick_id", cfg.JoystickID),
			slog.Int("debounce_ms", cfg.DebounceMs),
		)
	}

	// ── 9. Block until signal or listener failure ─────────────────────────────
	select {
	case sig := <-sigCh:
		logger.Info("received OS signal", slog.String("signal", sig.String()))
	case err := <-listenerDone:
		if err != nil {
			logger.Error("button listener exited unexpectedly", slog.Any("error", err))
		}
	}

	logger.Info("shutting down…")
	cancel()
	for _, eng := range engines {
		eng.Wait()
	}
	logger.Info("shutdown complete")
	return nil
}

// ─── Replay orchestration ─────────────────────────────────────────────────────

// runReplay generates a clip for triggerTime, optionally uploads it to R2,
// and saves it to the pending queue for Telegram delivery.
func runReplay(
	ctx context.Context,
	eng *video.Engine,
	cameraID string,
	uploader *upload.Client,
	logger *slog.Logger,
	triggerTime time.Time,
) {
	log := logger.With(slog.Time("trigger", triggerTime), slog.String("camera", cameraID))
	log.Info("replay: generating clip")

	replayPath, err := eng.GenerateReplay(ctx, triggerTime)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Error("replay: generation failed", slog.Any("error", err))
		return
	}
	log.Info("replay: clip saved to queue", slog.String("path", replayPath))

	// Keep the queue worker from moving this file out of pending/ while it's
	// still being read here (R2 upload, preview generation).
	unlock := delivery.Lock(replayPath)
	defer unlock()

	if uploader != nil {
		videoID := upload.NewID()
		previewPath := replayPath + ".preview.gif"
		// GeneratePreview may or may not have written the file depending on
		// uploadErr/previewErr below; always try to clean it up so a failed
		// R2 upload doesn't leave the gif orphaned in pending/ forever (only
		// .mp4 files are swept out of pending/ by the delivery queue).
		defer func() { _ = os.Remove(previewPath) }()

		// GIF generation only reads the local file — it doesn't depend on the
		// upload result — so run it concurrently with the (network-bound) R2
		// upload instead of waiting for the upload to finish first.
		var r2Key string
		var uploadErr, previewErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			r2Key, uploadErr = uploader.Upload(ctx, replayPath, videoID)
		}()
		go func() {
			defer wg.Done()
			previewErr = eng.GeneratePreview(ctx, replayPath, previewPath)
		}()
		wg.Wait()

		if uploadErr != nil {
			log.Error("replay: r2 upload failed", slog.Any("error", uploadErr))
		} else {
			log.Info("replay: uploaded to r2", slog.String("key", r2Key))

			var thumbnailKey string
			if previewErr != nil {
				log.Warn("replay: preview generation failed", slog.Any("error", previewErr))
			} else {
				if key, err := uploader.UploadPreview(ctx, previewPath, videoID); err != nil {
					log.Warn("replay: preview upload failed", slog.Any("error", err))
				} else {
					thumbnailKey = key
					log.Info("replay: preview uploaded", slog.String("key", key))
				}
			}

			meta := upload.VideoMeta{
				ID:           videoID,
				CameraID:     cameraID,
				R2Key:        r2Key,
				ThumbnailKey: thumbnailKey,
				DurationS:    int(eng.ClipDuration().Seconds()),
				SizeBytes:    fileSize(replayPath),
				TriggeredAt:  triggerTime,
			}
			if err := uploader.Notify(ctx, meta); err != nil {
				log.Warn("replay: backend notify failed", slog.Any("error", err))
			}
		}
	}
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// ─── Debouncer ───────────────────────────────────────────────────────────────

// debouncer gates rapid repeated calls: Allow returns (now, true) only when at
// least d has elapsed since the last allowed call.
type debouncer struct {
	d    time.Duration
	last time.Time
}

func (db *debouncer) Allow() (time.Time, bool) {
	now := time.Now().UTC()
	if now.Sub(db.last) < db.d {
		return now, false
	}
	db.last = now
	return now, true
}

// ─── Joystick listener ────────────────────────────────────────────────────────

// joystickRetryInterval is how long runJoystickListener waits before
// retrying after the device is missing or disconnects — e.g. a loose USB
// cable on the arcade button. A missing joystick must never bring down
// camera capture and delivery, so this loop retries forever instead of
// returning an error that would kill the whole process.
const joystickRetryInterval = 2 * time.Second

// runJoystickListener polls a USB joystick/gamepad every 20 ms and calls
// onPress on every button press outside the debounce window.
// Works on Linux (/dev/input/js{id}) and Windows (winmm.dll joyGetPosEx).
//
// If the device is absent or disconnects mid-run, it logs once and keeps
// retrying until ctx is cancelled — it only ever returns nil.
func runJoystickListener(
	ctx context.Context,
	joystickID int,
	d time.Duration,
	onPress func(time.Time),
	logger *slog.Logger,
) error {
	db := debouncer{d: d}
	warned := false

	for {
		js, err := joystick.Open(joystickID)
		if err != nil {
			if !warned {
				logger.Warn("joystick not found, will keep retrying",
					slog.Int("joystick_id", joystickID), slog.Any("error", err))
				warned = true
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(joystickRetryInterval):
				continue
			}
		}
		if warned {
			logger.Info("joystick connected", slog.Int("joystick_id", joystickID))
			warned = false
		}

		err = pollJoystick(ctx, js, &db, onPress)
		js.Close()
		if err == nil {
			return nil // ctx cancelled
		}
		logger.Warn("joystick disconnected, will retry", slog.Any("error", err))
	}
}

// pollJoystick reads button state every 20 ms until ctx is cancelled (nil)
// or the device errors out, e.g. unplugged (non-nil).
func pollJoystick(ctx context.Context, js *joystick.Joystick, db *debouncer, onPress func(time.Time)) error {
	var prevButtons uint32
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			buttons, err := js.Poll()
			if err != nil {
				return err
			}

			// Detect 0→1 transitions (press, not hold).
			pressed := buttons &^ prevButtons
			prevButtons = buttons

			if pressed == 0 {
				continue
			}

			if now, ok := db.Allow(); ok {
				onPress(now)
			}
		}
	}
}

// ─── Stdin listener (simulate mode) ──────────────────────────────────────────

func runStdinListener(ctx context.Context, d time.Duration, onPress func(time.Time)) error {
	scanner := bufio.NewScanner(os.Stdin)
	db := debouncer{d: d}

	fmt.Println("[ simulate ] press Enter to trigger a replay (Ctrl+C to quit)")

	for {
		type scanResult struct{ ok bool }
		ch := make(chan scanResult, 1)
		go func() { ch <- scanResult{ok: scanner.Scan()} }()

		select {
		case <-ctx.Done():
			return nil
		case res := <-ch:
			if !res.ok {
				return scanner.Err()
			}
		}

		if now, ok := db.Allow(); ok {
			onPress(now)
		} else {
			fmt.Printf("[ simulate ] debounce — ignoring (wait %v)\n", d-time.Since(db.last))
		}
	}
}

// ─── FFmpeg discovery ─────────────────────────────────────────────────────────

// findFFmpeg returns the path to the ffmpeg binary. It first looks for a
// bundled binary alongside the executable, then falls back to PATH.
//
// The bundled binary is a prebuilt static build and isn't guaranteed to run
// on every host — some CPU/kernel combos make it segfault on real demuxing
// while "-version" still succeeds, so it's health-checked with an actual
// decode before being trusted.
func findFFmpeg() string {
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		for _, name := range []string{"ffmpeg", "ffmpeg.exe"} {
			candidate := filepath.Join(dir, name)
			if _, err := os.Stat(candidate); err == nil && ffmpegWorks(candidate) {
				return candidate
			}
		}
	}
	return "ffmpeg"
}

// ffmpegWorks demuxes and decodes a tiny embedded real H264/MPEG-TS clip to
// confirm the binary actually works on this host. A synthetic lavfi source
// isn't enough — it bypasses the file demuxer and H264 decoder entirely, the
// exact code paths that segfault on some prebuilt static ffmpeg binaries
// even though "-version" runs fine.
func ffmpegWorks(path string) bool {
	tmp, err := os.CreateTemp("", "ffmpeg-selftest-*.ts")
	if err != nil {
		return false
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(selftest.ClipTS); err != nil {
		tmp.Close()
		return false
	}
	tmp.Close()

	cmd := exec.Command(path, "-v", "quiet", "-i", tmp.Name(), "-f", "null", "-")
	return cmd.Run() == nil
}


// ─── Env helpers ──────────────────────────────────────────────────────────────

func agendaDefault(simulate bool) string {
	if simulate {
		return "./agenda.json"
	}
	return "/etc/replay/agenda.json"
}
