package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/edipo/replay-saas/delivery"
	"github.com/edipo/replay-saas/video"

	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/host/v3"
)

// ─── Config ───────────────────────────────────────────────────────────────────

// appConfig holds all runtime settings loaded from environment variables.
// Every field has a default except the three marked as required.
type appConfig struct {
	// Video ingestion
	RTSPUrl     string
	BufferDir   string
	OutputDir   string
	SegmentTime int

	// Delivery
	BotToken       string // required: REPLAY_BOT_TOKEN
	ChatID         string // required: REPLAY_CHAT_ID
	AgendaPath     string
	TelegramLogPath string

	// Hardware
	GPIOPin    string // BCM pin name, e.g. "GPIO17"
	DebounceMs int    // milliseconds to ignore re-presses after a trigger

	// Dev/test
	Simulate bool // REPLAY_SIMULATE=true → Enter key instead of GPIO
}

func loadConfig() (appConfig, error) {
	simulate := os.Getenv("REPLAY_SIMULATE") == "true"

	cfg := appConfig{
		BufferDir:   envOr("REPLAY_BUFFER_DIR", "/tmp/replay_buffer"),
		OutputDir:   envOr("REPLAY_OUTPUT_DIR", "/tmp/replays"),
		SegmentTime: envIntOr("REPLAY_SEGMENT_TIME_S", 2),
		AgendaPath:      envOr("REPLAY_AGENDA_PATH", agendaDefault(simulate)),
		TelegramLogPath: envOr("REPLAY_TELEGRAM_LOG", "/tmp/telegram.log"),
		GPIOPin:     envOr("REPLAY_GPIO_PIN", "GPIO17"),
		DebounceMs:  envIntOr("REPLAY_DEBOUNCE_MS", 2000),
		Simulate:    simulate,
	}

	// Required variables — fail fast so the operator knows immediately.
	for _, r := range []struct {
		env string
		dst *string
	}{
		{"REPLAY_RTSP_URL", &cfg.RTSPUrl},
		{"REPLAY_BOT_TOKEN", &cfg.BotToken},
		{"REPLAY_CHAT_ID", &cfg.ChatID},
	} {
		v := os.Getenv(r.env)
		if v == "" {
			return appConfig{}, fmt.Errorf("required environment variable %s is not set", r.env)
		}
		*r.dst = v
	}

	return cfg, nil
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
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

	// ── 2. GPIO or simulate ───────────────────────────────────────────────────
	var pin gpio.PinIn
	if !cfg.Simulate {
		if _, err := host.Init(); err != nil {
			return fmt.Errorf("periph host init: %w", err)
		}
		p := gpioreg.ByName(cfg.GPIOPin)
		if p == nil {
			return fmt.Errorf("GPIO pin %q not found — check REPLAY_GPIO_PIN", cfg.GPIOPin)
		}
		pin = p
		logger.Info("GPIO pin acquired", slog.String("pin", pin.Name()))
	} else {
		logger.Info("simulation mode: press Enter to trigger a replay")
	}

	// ── 3. Video engine ───────────────────────────────────────────────────────
	eng, err := video.New(video.Config{
		RTSPUrl:     cfg.RTSPUrl,
		BufferDir:   cfg.BufferDir,
		OutputDir:   cfg.OutputDir,
		SegmentTime: cfg.SegmentTime,
	}, logger)
	if err != nil {
		return fmt.Errorf("video engine: %w", err)
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

	bot, err := delivery.New(delivery.Config{
		BotToken:   cfg.BotToken,
		ChatID:     cfg.ChatID,
		AgendaPath: cfg.AgendaPath,
	}, telegramLogger)
	if err != nil {
		return fmt.Errorf("delivery bot: %w", err)
	}

	// ── 5. Context + signal handling ─────────────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// ── 6. Start video ingestion ──────────────────────────────────────────────
	eng.Start(ctx)
	logger.Info("video ingestion started", slog.String("rtsp_url", cfg.RTSPUrl))

	// ── 7. Button press handler ───────────────────────────────────────────────
	//
	// atomic flag ensures only one replay is generated at a time.
	// If the button is pressed while a replay is already in progress, the
	// press is logged and discarded rather than queued — the 2 s debounce
	// already filters accidental double-presses, but the replay itself takes
	// ~25 s, so we need the extra guard.
	var replayActive atomic.Bool

	onPress := func(triggerTime time.Time) {
		if !replayActive.CompareAndSwap(false, true) {
			logger.Warn("button pressed – replay already in progress, ignoring")
			return
		}
		go func() {
			defer replayActive.Store(false)
			runReplay(ctx, eng, bot, logger, triggerTime)
		}()
	}

	// ── 8. Input listener goroutine (GPIO or stdin) ───────────────────────────
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
			listenerDone <- runButtonListener(ctx, pin, debounce, onPress)
		}()
		logger.Info("replay agent ready",
			slog.String("gpio_pin", cfg.GPIOPin),
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
	eng.Wait() // blocks until ingestion + cleanup goroutines stop
	logger.Info("shutdown complete")
	return nil
}

// ─── Replay orchestration ─────────────────────────────────────────────────────

// runReplay is called in its own goroutine every time the button is pressed.
// It chains GenerateReplay → Deliver and logs the outcome; it never panics.
func runReplay(
	ctx context.Context,
	eng *video.Engine,
	bot *delivery.Bot,
	logger *slog.Logger,
	triggerTime time.Time,
) {
	log := logger.With(slog.Time("trigger", triggerTime))
	log.Info("replay: generating clip")

	replayPath, err := eng.GenerateReplay(ctx, triggerTime)
	if err != nil {
		if ctx.Err() != nil {
			return // graceful shutdown, not an error
		}
		log.Error("replay: generation failed", slog.Any("error", err))
		return
	}
	log.Info("replay: clip ready, delivering", slog.String("path", replayPath))

	if err := bot.Deliver(ctx, replayPath, triggerTime); err != nil {
		if errors.Is(err, delivery.ErrNoSlotFound) {
			// Configuration issue, not a transient error — warn and move on.
			log.Warn("replay: no agenda slot found, delivery skipped", slog.Any("error", err))
			return
		}
		log.Error("replay: delivery failed", slog.Any("error", err))
		return
	}

	log.Info("replay: delivered successfully")
}

// ─── GPIO button listener ─────────────────────────────────────────────────────

// runButtonListener configures pin as an input with a pull-up resistor and
// calls onPress with the current time whenever a falling edge (button pressed
// to GND) is detected outside the debounce window.
//
// It returns nil when ctx is cancelled (clean shutdown) or a non-nil error if
// the GPIO driver reports a failure.
//
// Wiring assumption: one leg of the button to the GPIO pin, other leg to GND.
// The internal pull-up holds the pin HIGH when idle; pressing pulls it LOW.
func runButtonListener(
	ctx context.Context,
	pin gpio.PinIn,
	debounce time.Duration,
	onPress func(time.Time),
) error {
	if err := pin.In(gpio.PullUp, gpio.BothEdges); err != nil {
		return fmt.Errorf("configure pin %s: %w", pin.Name(), err)
	}

	const pollInterval = 200 * time.Millisecond
	var lastPress time.Time

	for {
		// Check for shutdown before blocking.
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// WaitForEdge blocks for up to pollInterval, then returns false (timeout).
		// This keeps the loop responsive to context cancellation.
		if !pin.WaitForEdge(pollInterval) {
			continue
		}

		// Read the current level to distinguish press (Low) from release (High).
		if pin.Read() != gpio.Low {
			continue // rising edge → button released, ignore
		}

		// Falling edge → button pressed.
		now := time.Now()
		if now.Sub(lastPress) < debounce {
			continue // within debounce window, discard
		}
		lastPress = now

		onPress(now)
	}
}

// ─── Stdin listener (simulate mode) ──────────────────────────────────────────

// runStdinListener reads lines from stdin. Every Enter key press (empty line
// or any text) is treated as a button press, subject to the same debounce
// window used in production. Ctrl+C still exits via the OS signal handler.
func runStdinListener(ctx context.Context, debounce time.Duration, onPress func(time.Time)) error {
	scanner := bufio.NewScanner(os.Stdin)
	var lastPress time.Time

	fmt.Println("[ simulate ] press Enter to trigger a replay (Ctrl+C to quit)")

	for {
		// bufio.Scanner.Scan() blocks — run it in a goroutine so we can also
		// watch for context cancellation without leaking the goroutine.
		type scanResult struct{ ok bool }
		ch := make(chan scanResult, 1)
		go func() { ch <- scanResult{ok: scanner.Scan()} }()

		select {
		case <-ctx.Done():
			return nil
		case res := <-ch:
			if !res.ok {
				return scanner.Err() // EOF or read error
			}
		}

		now := time.Now()
		if now.Sub(lastPress) < debounce {
			fmt.Printf("[ simulate ] debounce — ignoring (wait %v)\n",
				debounce-now.Sub(lastPress))
			continue
		}
		lastPress = now
		onPress(now)
	}
}

// ─── Env helpers ──────────────────────────────────────────────────────────────

// agendaDefault returns a sensible default path for agenda.json depending
// on whether we're running in simulate (local dev) or production (Docker).
func agendaDefault(simulate bool) string {
	if simulate {
		return "./agenda.json"
	}
	return "/etc/replay/agenda.json"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
