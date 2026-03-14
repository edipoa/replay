package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// ─── Constants ────────────────────────────────────────────────────────────────

const (
	telegramSendVideo = "https://api.telegram.org/bot%s/sendVideo"

	// maxRetries is how many times UploadVideo will attempt the upload before
	// giving up and returning an error to the caller.
	maxRetries = 3

	// retryBaseDelay is the initial wait between retry attempts.
	// It doubles on each failure (capped at retryMaxDelay).
	retryBaseDelay = 5 * time.Second
	retryMaxDelay  = 60 * time.Second

	// defaultHTTPTimeout covers the full upload, including streaming the video
	// body. 90 s is generous for typical amateur-football clip sizes (~10 MB).
	defaultHTTPTimeout = 90 * time.Second
)

// ─── Config ───────────────────────────────────────────────────────────────────

// Config holds credentials and routing parameters for the Bot.
type Config struct {
	// BotToken is the Telegram Bot API token (e.g. "123456:ABCDEF...").
	BotToken string

	// ChatID is the Telegram group/channel ID (negative integer as string,
	// e.g. "-1001234567890").
	ChatID string

	// AgendaPath is the file-system path to agenda.json.
	AgendaPath string

	// HTTPTimeout overrides the default HTTP client timeout (90 s).
	// Set to a larger value for slow uplinks.
	HTTPTimeout time.Duration
}

// ─── Bot ──────────────────────────────────────────────────────────────────────

// Bot delivers replay videos to the correct Telegram topic based on the current
// schedule. Create one with New; it is safe for concurrent use.
type Bot struct {
	cfg    Config
	client *http.Client
	logger *slog.Logger
}

// New validates cfg, builds an HTTP client, and returns a ready Bot.
func New(cfg Config, logger *slog.Logger) (*Bot, error) {
	if cfg.BotToken == "" {
		return nil, fmt.Errorf("delivery: BotToken must not be empty")
	}
	if cfg.ChatID == "" {
		return nil, fmt.Errorf("delivery: ChatID must not be empty")
	}
	if cfg.AgendaPath == "" {
		return nil, fmt.Errorf("delivery: AgendaPath must not be empty")
	}
	if logger == nil {
		logger = slog.Default()
	}

	timeout := cfg.HTTPTimeout
	if timeout == 0 {
		timeout = defaultHTTPTimeout
	}

	return &Bot{
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
		logger: logger,
	}, nil
}

// ─── High-level entry point ───────────────────────────────────────────────────

// Deliver is the single entry point for the upper layer.
//
// It:
//  1. Re-reads agenda.json to resolve the correct Telegram topic for time t.
//  2. Calls UploadVideo with retry logic.
//
// Returns ErrNoSlotFound (unwrappable with errors.Is) if the schedule has no
// slot for the current day/time — the caller should log and skip rather than
// retry, as the schedule must first be corrected.
//
// Any other error is transient (network, Telegram API) and can be retried
// later by the caller.
func (b *Bot) Deliver(ctx context.Context, filePath string, t time.Time) error {
	agenda, err := LoadAgenda(b.cfg.AgendaPath)
	if err != nil {
		return fmt.Errorf("load agenda: %w", err)
	}

	threadID, err := agenda.ResolveThreadID(t)
	if err != nil {
		return err // already wraps ErrNoSlotFound
	}

	b.logger.Info("delivery: slot resolved",
		slog.Int64("thread_id", threadID),
		slog.String("file", filePath),
		slog.Time("trigger", t),
	)

	return b.UploadVideo(ctx, filePath, threadID)
}

// ─── Upload with retry ────────────────────────────────────────────────────────

// UploadVideo sends the video at filePath to the Telegram group topic identified
// by threadID. Pass 0 for threadID to send to the group's main feed.
//
// Retry policy:
//   - Up to maxRetries (3) attempts.
//   - Waits retryBaseDelay (5 s) between attempts, doubling each time (max 60 s).
//   - If Telegram responds with 429 (rate-limit), the Retry-After value is
//     respected instead of the default delay.
//   - Permanent errors (wrong token, invalid chat ID, etc.) are returned
//     immediately without further retries.
//   - Context cancellation aborts the wait between retries and the in-flight
//     request, returning a wrapped ctx.Err().
func (b *Bot) UploadVideo(ctx context.Context, filePath string, threadID int64) error {
	log := b.logger.With(
		slog.String("component", "delivery"),
		slog.String("file", filepath.Base(filePath)),
		slog.Int64("thread_id", threadID),
	)

	delay := retryBaseDelay
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			log.Warn("retrying upload",
				slog.Int("attempt", attempt),
				slog.Duration("delay", delay),
				slog.Any("last_error", lastErr),
			)
			select {
			case <-ctx.Done():
				return fmt.Errorf("upload cancelled before attempt %d: %w", attempt, ctx.Err())
			case <-time.After(delay):
			}
		}

		err := b.sendVideo(ctx, filePath, threadID)
		if err == nil {
			log.Info("upload successful", slog.Int("attempt", attempt))
			return nil
		}

		// Inspect the error type to decide whether to retry.
		var tgErr *TelegramError
		if errors.As(err, &tgErr) {
			if !tgErr.retryable() {
				// Permanent API error (4xx other than 429): no point retrying.
				return fmt.Errorf("permanent Telegram error (attempt %d): %w", attempt, tgErr)
			}
			// Rate-limited: honour the server's requested wait time.
			if tgErr.RetryAfter > 0 {
				delay = time.Duration(tgErr.RetryAfter+1) * time.Second
				log.Warn("rate-limited by Telegram",
					slog.Int("retry_after_s", tgErr.RetryAfter),
				)
			}
		}

		lastErr = err
		delay = min(delay*2, retryMaxDelay)
	}

	return fmt.Errorf("upload failed after %d attempts: %w", maxRetries, lastErr)
}

// ─── Single-attempt upload ────────────────────────────────────────────────────

// sendVideo performs exactly one multipart/form-data POST to the Telegram
// sendVideo endpoint. It streams the file directly from disk — no full
// in-memory buffering — so it works on low-RAM edge devices.
func (b *Bot) sendVideo(ctx context.Context, filePath string, threadID int64) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open video %q: %w", filePath, err)
	}
	defer f.Close()

	// io.Pipe lets us write multipart fields in a goroutine while the HTTP
	// client reads from the other end — no intermediate buffer for the video.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		var writeErr error
		defer func() {
			mw.Close()
			// CloseWithError(nil) is equivalent to Close(); any non-nil error
			// is surfaced to the HTTP client as a read error.
			pw.CloseWithError(writeErr)
		}()

		if writeErr = mw.WriteField("chat_id", b.cfg.ChatID); writeErr != nil {
			return
		}
		if threadID != 0 {
			if writeErr = mw.WriteField("message_thread_id", strconv.FormatInt(threadID, 10)); writeErr != nil {
				return
			}
		}

		part, err := mw.CreateFormFile("video", filepath.Base(filePath))
		if err != nil {
			writeErr = fmt.Errorf("create form file: %w", err)
			return
		}

		if _, err := io.Copy(part, f); err != nil {
			writeErr = fmt.Errorf("stream video body: %w", err)
		}
	}()

	apiURL := fmt.Sprintf(telegramSendVideo, b.cfg.BotToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, pr)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := b.client.Do(req)
	if err != nil {
		// Closing pr unblocks the writer goroutine: pw.Write returns an error
		// instead of blocking forever when nobody is reading from the pipe.
		_ = pr.CloseWithError(err)
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	return parseTelegramResponse(resp)
}

// ─── Telegram API response ────────────────────────────────────────────────────

// telegramResponse mirrors the Telegram Bot API envelope.
type telegramResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description,omitempty"`
	ErrorCode   int    `json:"error_code,omitempty"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after,omitempty"`
	} `json:"parameters,omitempty"`
}

// TelegramError represents an API-level error returned by Telegram.
// Callers may use errors.As to inspect Code and RetryAfter.
type TelegramError struct {
	Code        int
	Description string
	RetryAfter  int // non-zero only for HTTP 429
}

func (e *TelegramError) Error() string {
	return fmt.Sprintf("telegram API error %d: %s", e.Code, e.Description)
}

// retryable returns true for errors that might resolve themselves:
//   - 429 Too Many Requests
//   - 5xx server errors
func (e *TelegramError) retryable() bool {
	return e.Code == 429 || e.Code >= 500
}

// parseTelegramResponse decodes the API envelope from resp.Body and returns
// a *TelegramError if ok == false, nil on success.
func parseTelegramResponse(resp *http.Response) error {
	var tg telegramResponse
	if err := json.NewDecoder(resp.Body).Decode(&tg); err != nil {
		// If we can't decode the body, treat it as a retryable server error.
		return &TelegramError{
			Code:        resp.StatusCode,
			Description: fmt.Sprintf("decode response body: %s", err),
		}
	}

	if tg.OK {
		return nil
	}

	tgErr := &TelegramError{
		Code:        tg.ErrorCode,
		Description: tg.Description,
	}
	if tg.Parameters != nil {
		tgErr.RetryAfter = tg.Parameters.RetryAfter
	}
	return tgErr
}
