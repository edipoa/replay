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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/edipo/replay-saas/delivery"
	"github.com/edipo/replay-saas/internal/envutil"
	"github.com/edipo/replay-saas/internal/joystick"
	"github.com/edipo/replay-saas/internal/live"
	"github.com/edipo/replay-saas/internal/obs"
	"github.com/edipo/replay-saas/internal/selftest"
	"github.com/edipo/replay-saas/upload"
	"github.com/edipo/replay-saas/video"
)

// ─── Config ───────────────────────────────────────────────────────────────────

type cameraConfig struct {
	ID          string // "cam1", "cam2", …
	RTSPUrl     string
	BufferDir   string
	LiveRTSPUrl string // substream for the near-live view; "" disables live for this camera
}

type appConfig struct {
	// Video ingestion (one entry per configured camera)
	Cameras     []cameraConfig
	OutputDir   string
	SegmentTime int
	BufferDurS  int // seconds of segment buffer retained on disk
	ClipWidth   int

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

	// Observability
	HTTPAddr      string // status server listen addr (":8088")
	AlertThreadID int    // Telegram message_thread_id for critical alerts (0 = main feed)

	// Live view (near-live HLS on the aovivo.* host). On/off is driven by the
	// replay-site schedule (live_control table + slots + games), polled by the
	// agent — there is no local enable flag.
	LiveDir          string // parent dir for per-camera HLS (should be a small tmpfs)
	LivePollInterval time.Duration

	// Dev/test
	Simulate bool
}

func loadConfig() (appConfig, error) {
	simulate := os.Getenv("REPLAY_SIMULATE") == "true"

	cfg := appConfig{
		OutputDir:           envutil.Or("REPLAY_OUTPUT_DIR", "/tmp/replays"),
		SegmentTime:         envutil.IntOr("REPLAY_SEGMENT_TIME_S", 2),
		BufferDurS:          envutil.IntOr("REPLAY_BUFFER_DUR_S", 300),
		ClipWidth:           envutil.IntOr("REPLAY_CLIP_WIDTH", 1280),
		AgendaPath:          envutil.Or("REPLAY_AGENDA_PATH", agendaDefault(simulate)),
		TelegramLogPath:     envutil.Or("REPLAY_TELEGRAM_LOG", "/tmp/telegram.log"),
		WatermarkPath:       envutil.Or("REPLAY_WATERMARK_PATH", "watermark.png"),
		LogoPath:            envutil.Or("REPLAY_LOGO_PATH", "logo.png"),
		BackgroundMusicPath: os.Getenv("REPLAY_MUSIC_PATH"),
		JoystickID:          envutil.IntOr("REPLAY_JOYSTICK_ID", 0),
		DebounceMs:          envutil.IntOr("REPLAY_DEBOUNCE_MS", 2000),
		HTTPAddr:            envutil.Or("REPLAY_HTTP_ADDR", ":8088"),
		AlertThreadID:       envutil.IntOr("REPLAY_ALERT_THREAD_ID", 0),
		LiveDir:             envutil.Or("REPLAY_LIVE_DIR", "/tmp/replay_live"),
		LivePollInterval:    time.Duration(envutil.IntOr("REPLAY_LIVE_POLL_INTERVAL_S", 30)) * time.Second,
		Simulate:            simulate,
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

		// Live source: explicit override, else the camera substream (subtype=1).
		// Whether it actually streams is decided later by the schedule poller.
		liveURL := os.Getenv(fmt.Sprintf("REPLAY_CAM_%d_LIVE_RTSP_URL", i))
		if liveURL == "" {
			if sub := strings.Replace(url, "subtype=0", "subtype=1", 1); sub != url {
				liveURL = sub
			}
		}

		cfg.Cameras = append(cfg.Cameras, cameraConfig{ID: id, RTSPUrl: url, BufferDir: bufDir, LiveRTSPUrl: liveURL})
	}

	if len(cfg.Cameras) == 0 {
		return appConfig{}, fmt.Errorf("required environment variable REPLAY_CAM_1_RTSP_URL (or REPLAY_RTSP_URL) is not set")
	}

	cfg.BotToken = os.Getenv("REPLAY_BOT_TOKEN")
	cfg.ChatID = os.Getenv("REPLAY_CHAT_ID")

	if s := envutil.IntOr("REPLAY_UPLOAD_TIMEOUT_S", 0); s > 0 {
		uploadTimeout = time.Duration(s) * time.Second
	}

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

	// ── 3. Live schedule gate ────────────────────────────────────────────────
	// The live stream is allowed on only during registered slots/games (plus a
	// manual override), computed by the replay-site backend. The gate needs the
	// backend: without it there is no schedule, so the live stays down.
	backendURL := strings.TrimRight(os.Getenv("REPLAY_BACKEND_URL"), "/")
	backendKey := os.Getenv("REPLAY_BACKEND_API_KEY")
	var liveGate *live.Gate
	liveWanted := false
	for _, cam := range cfg.Cameras {
		if cam.LiveRTSPUrl != "" {
			liveWanted = true
		}
	}
	switch {
	case !liveWanted:
		// no substream configured on any camera — nothing to gate
	case backendURL == "" || backendKey == "":
		logger.Warn("live desativada: REPLAY_BACKEND_URL/REPLAY_BACKEND_API_KEY ausentes — " +
			"sem agenda para controlar a transmissão, ela não sobe")
		for i := range cfg.Cameras {
			cfg.Cameras[i].LiveRTSPUrl = ""
		}
	default:
		liveGate = live.NewGate()
	}

	// ── 3b. Video engines (one per camera) ───────────────────────────────────
	var engines []*video.Engine
	for _, cam := range cfg.Cameras {
		eng, err := video.New(video.Config{
			RTSPUrl:             cam.RTSPUrl,
			BufferDir:           cam.BufferDir,
			OutputDir:           cfg.OutputDir,
			SegmentTime:         cfg.SegmentTime,
			BufferDur:           time.Duration(cfg.BufferDurS) * time.Second,
			ClipWidth:           cfg.ClipWidth,
			FFmpegBin:           ffmpegBin,
			WatermarkPath:       cfg.WatermarkPath,
			LogoPath:            cfg.LogoPath,
			BackgroundMusicPath: cfg.BackgroundMusicPath,
			CameraID:            cam.ID,
			LiveDir:             cfg.LiveDir,
			LiveRTSPUrl:         cam.LiveRTSPUrl,
			LiveGate:            liveGate,
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

	// The Telegram bot is used for two independent things:
	//   - operational alerts (obs layer) — on whenever a token+chat are set
	//   - per-slot clip delivery — opt-in via REPLAY_TELEGRAM_CLIPS=true
	// Clips now go to R2/replay-site, so clip delivery defaults off.
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
		logger.Info("telegram bot enabled (alerts)")
	} else {
		logger.Warn("telegram disabled (REPLAY_BOT_TOKEN/REPLAY_CHAT_ID not set) — no alerts")
	}

	telegramClips := os.Getenv("REPLAY_TELEGRAM_CLIPS") == "true"

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
	emitObs := func(level, kind, camera, msg string, kv ...any) {
		obs.Event(obs.Level(level), kind, camera, msg, kv...)
	}

	// ── 5b. Observability (SQLite event log + /status HTTP + Telegram alerts) ─
	bufferDirs := make(map[string]string, len(cfg.Cameras))
	for _, cam := range cfg.Cameras {
		bufferDirs[cam.ID] = cam.BufferDir
	}
	obsCfg := obs.Config{
		DBPath:        filepath.Join(cfg.OutputDir, "replay.db"),
		HTTPAddr:      cfg.HTTPAddr,
		AlertThread:   int64(cfg.AlertThreadID),
		BufferDirs:    bufferDirs,
		NewestSegment: video.NewestSegmentTime,
		TriggerToken:  os.Getenv("REPLAY_TRIGGER_TOKEN"),
	}
	if liveGate != nil {
		obsCfg.LiveDir = cfg.LiveDir
		obsCfg.LiveGate = liveGate
	}
	if bot != nil {
		obsCfg.Bot = bot // only assign non-nil — a typed nil in the interface would panic on send
	}
	rec, err := obs.New(obsCfg)
	if err != nil {
		return fmt.Errorf("observability: %w", err)
	}
	obs.SetDefault(rec)
	defer rec.Close()
	logger.Info("observability enabled",
		slog.String("db", obsCfg.DBPath),
		slog.String("http_addr", cfg.HTTPAddr),
	)

	// ── 6. Context + signal handling ─────────────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go rec.ServeHTTP(ctx)
	go rec.RunHeartbeat(ctx)
	obs.Event(obs.Info, "agent.started", "", "replay-agent iniciado",
		"cameras", len(cfg.Cameras), "simulate", cfg.Simulate)

	// ── 6a. Live schedule poller ─────────────────────────────────────────────
	if liveGate != nil {
		poller := &live.Poller{
			URL:      backendURL + "/api/live/state",
			APIKey:   backendKey,
			Interval: cfg.LivePollInterval,
			Gate:     liveGate,
			Logger:   logger.With(slog.String("component", "live-poll")),
			OnPollError: func(err error, streak int) {
				if streak == 1 || streak%10 == 0 {
					obs.Event(obs.Warn, "live.poll_failed", "",
						"poll do estado da live falhou — mantendo último estado conhecido",
						"streak", streak, "error", err.Error())
				}
			},
		}
		go poller.Run(ctx)
		logger.Info("live schedule poller enabled",
			slog.String("url", poller.URL), slog.Duration("interval", cfg.LivePollInterval))
	}

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
	// nil bot → the worker just moves clips pending/→delivered/ (R2 already has
	// them); it only posts to Telegram when clip delivery is explicitly enabled.
	queueBot := bot
	if !telegramClips {
		queueBot = nil
	} else {
		logger.Info("telegram clip delivery enabled")
	}
	go delivery.RunQueueWorker(ctx, cfg.OutputDir, queueBot, telegramLogger)

	// Persistent R2 retry queue: clips whose live upload didn't fully land are
	// finished here whenever the uplink recovers, across restarts.
	if uploader != nil {
		deliveredDir := filepath.Join(cfg.OutputDir, "delivered")
		go upload.RunRetryQueue(ctx, uploader, upload.QueueDir(cfg.OutputDir), deliveredDir,
			uploadTimeout, &uploadMu, emitObs, logger)
	}

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
		replayWG      sync.WaitGroup
	)

	// source is "físico" (joystick), "simulate" (stdin) or "web" (status page).
	onPress := func(triggerTime time.Time, source string) {
		if replayActive.CompareAndSwap(false, true) {
			obs.Event(obs.Info, "button.press", "", "botão apertado — gerando replay",
				slog.Time("trigger", triggerTime), slog.String("source", source))
			lastTriggerNs.Store(triggerTime.UnixNano())
			replayWG.Add(1)
			go func() {
				defer replayWG.Done()
				t := triggerTime
				for {
					// Fire all cameras concurrently; wait for all before
					// accepting the next queued press or releasing the lock.
					var wg sync.WaitGroup
					for i, eng := range engines {
						wg.Add(1)
						go func(e *video.Engine, camID string) {
							defer wg.Done()
							runReplay(ctx, e, camID, uploader, cfg.OutputDir, logger, t)
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
			obs.Event(obs.Info, "button.repress_dropped", "", "press ignorado (re-press <15s de um replay em andamento)",
				slog.Duration("since_last", since), slog.String("source", source))
			return
		}
		select {
		case pressQueue <- triggerTime:
			obs.Event(obs.Info, "button.press", "", "botão apertado — clipe na fila pra próxima geração",
				slog.Time("trigger", triggerTime), slog.Bool("queued", true), slog.String("source", source))
			logger.Info("clip queued for next generation", slog.Time("trigger", triggerTime))
		default:
			obs.Event(obs.Warn, "button.repress_dropped", "", "press ignorado (fila cheia, replay em andamento)",
				slog.Time("trigger", triggerTime), slog.String("source", source))
			logger.Warn("press queue full, ignoring", slog.Time("trigger", triggerTime))
		}
	}

	// Adapters so the joystick/stdin listeners (which take func(time.Time)) and
	// the status page's POST /trigger all route through the same onPress.
	physicalSource := "físico"
	if cfg.Simulate {
		physicalSource = "simulate"
	}
	listenerPress := func(t time.Time) { onPress(t, physicalSource) }
	rec.SetTrigger(func(t time.Time) {
		if ctx.Err() != nil {
			return // agent shutting down — don't touch replayWG after cancel
		}
		onPress(t, "web")
	})

	// ── 8. Input listener goroutine ───────────────────────────────────────────
	debounce := time.Duration(cfg.DebounceMs) * time.Millisecond
	listenerDone := make(chan error, 1)

	if cfg.Simulate {
		go func() {
			listenerDone <- runStdinListener(ctx, debounce, listenerPress)
		}()
		logger.Info("replay agent ready (simulate)",
			slog.String("trigger", "press Enter"),
			slog.Int("debounce_ms", cfg.DebounceMs),
		)
	} else {
		go func() {
			listenerDone <- runJoystickListener(ctx, cfg.JoystickID, debounce, listenerPress, logger)
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
	obs.Event(obs.Info, "agent.stopped", "", "replay-agent encerrando")
	cancel()
	for _, eng := range engines {
		eng.Wait()
	}
	// Let any in-flight replay (already past clip generation, now uploading to
	// R2) finish instead of abandoning it mid-upload — that upload runs on its
	// own bounded-timeout context (see runReplay), not the cancelled ctx above,
	// so it can actually complete instead of failing with "context canceled".
	replayWG.Wait()
	logger.Info("shutdown complete")
	return nil
}

// ─── Replay orchestration ─────────────────────────────────────────────────────

// uploadTimeout bounds R2 upload/preview/notify so they can outlive a
// shutdown signal (see runReplay) without hanging forever on a dead network.
// Sized to give upload.Client's internal retry-with-backoff room to finish
// under the venue's slow uplink — two camera clips upload concurrently and
// share it, so 2 min was routinely too tight ("context deadline exceeded").
// Overridable with REPLAY_UPLOAD_TIMEOUT_S. Shutdown waits at most this long
// for an in-flight upload (replayWG.Wait), so don't set it absurdly high.
var uploadTimeout = 8 * time.Minute

// uploadMu serializes R2 uploads across the per-camera runReplay goroutines
// (see runReplay) so they don't split the venue's uplink and time out together.
var uploadMu sync.Mutex

// runReplay generates a clip for triggerTime, optionally uploads it to R2,
// and saves it to the pending queue for Telegram delivery.
func runReplay(
	ctx context.Context,
	eng *video.Engine,
	cameraID string,
	uploader *upload.Client,
	outputDir string,
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
		obs.Event(obs.Critical, "clip.failed", cameraID, "falha ao gerar o clipe",
			slog.Time("trigger", triggerTime), slog.Any("error", err))
		return
	}
	log.Info("replay: clip saved to queue", slog.String("path", replayPath))
	obs.Event(obs.Info, "clip.ready", cameraID, "clipe gerado e na fila de entrega",
		slog.Time("trigger", triggerTime), slog.String("path", filepath.Base(replayPath)))

	// Keep the queue worker from moving this file out of pending/ while it's
	// still being read here (R2 upload, preview generation).
	unlock := delivery.Lock(replayPath)
	defer unlock()

	if uploader != nil {
		uploadClip(ctx, eng, cameraID, uploader, outputDir, replayPath, logger, triggerTime)
	}
}

// uploadClip pushes a freshly generated clip to R2 + the backend. Anything that
// doesn't land is handed to the persistent retry queue (upload.RunRetryQueue),
// which finishes it whenever the venue's uplink recovers — even after a
// restart.
func uploadClip(ctx context.Context, eng *video.Engine, cameraID string, uploader *upload.Client, outputDir, replayPath string, logger *slog.Logger, triggerTime time.Time) {
	log := logger.With(slog.Time("trigger", triggerTime), slog.String("camera", cameraID))

	videoID := upload.NewID()
	previewPath := replayPath + ".preview.gif"

	// Upload/preview/notify only read the already-completed local file, so they
	// run on their own bounded context — a shutdown shouldn't cancel them
	// mid-transfer (the retry queue would pick it up, but finishing now is
	// better).
	uploadCtx, cancelUpload := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancelUpload()

	var r2Key string
	var uploadErr, previewErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// Serialize the actual R2 upload across cameras: two clips racing on the
		// venue's thin uplink each get half the bandwidth and both tend to blow
		// uploadTimeout. One at a time, each gets the full pipe.
		uploadMu.Lock()
		r2Key, uploadErr = uploader.Upload(uploadCtx, replayPath, videoID)
		uploadMu.Unlock()
	}()
	go func() {
		defer wg.Done()
		previewErr = eng.GeneratePreview(uploadCtx, replayPath, previewPath)
	}()
	wg.Wait()

	meta := upload.VideoMeta{
		ID:          videoID,
		CameraID:    cameraID,
		R2Key:       r2Key,
		DurationS:   int(eng.ClipDuration().Seconds()),
		SizeBytes:   fileSize(replayPath),
		TriggeredAt: triggerTime,
	}

	var thumbKey string
	if uploadErr == nil && previewErr == nil {
		if k, err := uploader.UploadPreview(uploadCtx, previewPath, videoID); err != nil {
			log.Warn("replay: preview upload failed", slog.Any("error", err))
		} else {
			thumbKey = k
			meta.ThumbnailKey = k
		}
	} else if previewErr != nil {
		log.Warn("replay: preview generation failed", slog.Any("error", previewErr))
	}

	var notifyErr error
	if uploadErr == nil {
		if notifyErr = uploader.Notify(uploadCtx, meta); notifyErr != nil {
			log.Warn("replay: backend notify failed", slog.Any("error", notifyErr))
		}
	}

	if uploadErr == nil && notifyErr == nil {
		_ = os.Remove(previewPath)
		log.Info("replay: uploaded to r2", slog.String("key", r2Key))
		obs.Event(obs.Info, "upload.ok", cameraID, "clipe no R2 + backend notificado",
			slog.String("key", r2Key))
		return
	}

	// Didn't fully land — persist a resumable job. Enqueue moves the .mp4 and
	// .gif into the queue dir so the delivery sweeper / TTL don't touch them.
	job := upload.Job{
		VideoID:         videoID,
		Meta:            meta,
		ClipUploaded:    uploadErr == nil,
		PreviewUploaded: thumbKey != "",
		Notified:        uploadErr == nil && notifyErr == nil,
	}
	if err := upload.Enqueue(upload.QueueDir(outputDir), replayPath, previewPath, job); err != nil {
		log.Error("replay: could not enqueue clip for retry — clip may be lost",
			slog.Any("enqueue_error", err), slog.Any("upload_error", uploadErr))
		obs.Event(obs.Critical, "upload.failed", cameraID, "upload falhou E não foi pra fila de retry — clipe em risco",
			slog.Any("error", err))
		return
	}
	cause := uploadErr
	if cause == nil {
		cause = notifyErr
	}
	log.Warn("replay: upload incomplete, queued for retry", slog.Any("error", cause))
	obs.Event(obs.Warn, "upload.failed", cameraID, "upload/notify falhou — clipe na fila de retry",
		slog.String("video_id", videoID), slog.Any("error", cause))
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
				obs.Event(obs.Critical, "button.usb_disconnected", "", "joystick/botão não encontrado — presses não funcionam",
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
			obs.Event(obs.Info, "button.usb_connected", "", "joystick/botão conectado",
				slog.Int("joystick_id", joystickID))
			warned = false
		}

		err = pollJoystick(ctx, js, &db, onPress)
		js.Close()
		if err == nil {
			return nil // ctx cancelled
		}
		logger.Warn("joystick disconnected, will retry", slog.Any("error", err))
		obs.Event(obs.Critical, "button.usb_disconnected", "", "joystick/botão desconectou — presses não funcionam até reconectar",
			slog.Any("error", err))
		warned = true
	}
}

// pollJoystick reads button state every 20 ms until ctx is cancelled (nil)
// or the device errors out, e.g. unplugged (non-nil).
func pollJoystick(ctx context.Context, js *joystick.Joystick, db *debouncer, onPress func(time.Time)) error {
	var prevButtons uint32
	var lastDebouncedEvent time.Time // throttles button.debounced against contact bounce
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
			} else if time.Since(lastDebouncedEvent) > 2*time.Second {
				lastDebouncedEvent = time.Now()
				obs.Event(obs.Info, "button.debounced", "", "press ignorado por debounce")
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
			obs.Event(obs.Info, "button.debounced", "", "press ignorado por debounce")
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
