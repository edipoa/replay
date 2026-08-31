package upload

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Config holds Cloudflare R2 and PHP backend credentials.
type Config struct {
	AccountID   string // Cloudflare account ID
	AccessKeyID string
	SecretKey   string
	Bucket      string
	BackendURL  string // e.g. "https://seusite.com.br" (no trailing slash)
	APIKey      string // X-Api-Key sent to the PHP backend
}

// Client uploads replay clips to R2 and notifies the PHP backend.
// Safe for concurrent use.
type Client struct {
	s3      *s3.Client
	bucket  string
	backend string
	apiKey  string
	http    *http.Client
	logger  *slog.Logger
}

// VideoMeta is the payload sent to POST /api/videos.
type VideoMeta struct {
	ID           string    `json:"id"`
	CameraID     string    `json:"camera_id"`
	R2Key        string    `json:"r2_key"`
	ThumbnailKey string    `json:"thumbnail_key,omitempty"`
	DurationS    int       `json:"duration_s"`
	SizeBytes    int64     `json:"size_bytes"`
	TriggeredAt  time.Time `json:"triggered_at"`
}

// New validates cfg and returns a ready Client.
func New(cfg Config, logger *slog.Logger) (*Client, error) {
	for _, v := range []struct{ name, val string }{
		{"REPLAY_R2_ACCOUNT_ID", cfg.AccountID},
		{"REPLAY_R2_ACCESS_KEY_ID", cfg.AccessKeyID},
		{"REPLAY_R2_SECRET_ACCESS_KEY", cfg.SecretKey},
		{"REPLAY_R2_BUCKET", cfg.Bucket},
		{"REPLAY_BACKEND_URL", cfg.BackendURL},
	} {
		if v.val == "" {
			return nil, fmt.Errorf("upload: %s must not be empty", v.name)
		}
	}
	if logger == nil {
		logger = slog.Default()
	}

	endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.AccountID)
	s3client := s3.New(s3.Options{
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretKey, ""),
		Region:       "auto",
		UsePathStyle: true,
	})

	return &Client{
		s3:      s3client,
		bucket:  cfg.Bucket,
		backend: strings.TrimRight(cfg.BackendURL, "/"),
		apiKey:  cfg.APIKey,
		// No client-level timeout: every call path passes a context with its
		// own deadline (uploadCtx in runReplay). A fixed http timeout here just
		// double-bounds it and used to kill legitimately-progressing large
		// uploads on the venue's slow uplink.
		http:   &http.Client{},
		logger: logger,
	}, nil
}

// NewID returns a random 32-char hex string to use as a video ID.
func NewID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Upload streams filePath directly to R2 (no in-memory buffering) and returns
// the object key used.
func (c *Client) Upload(ctx context.Context, filePath, videoID string) (string, error) {
	key := fmt.Sprintf("videos/%s/%s", videoID, filepath.Base(filePath))
	if err := c.putFile(ctx, filePath, key, "video/mp4"); err != nil {
		return "", err
	}
	return key, nil
}

// UploadPreview uploads gifPath to R2 under previews/{videoID}/preview.gif and
// returns the object key. The previews/ prefix is meant to be publicly readable.
func (c *Client) UploadPreview(ctx context.Context, gifPath, videoID string) (string, error) {
	key := fmt.Sprintf("previews/%s/preview.gif", videoID)
	if err := c.putFile(ctx, gifPath, key, "image/gif"); err != nil {
		return "", err
	}
	return key, nil
}

// Retry tuning for putFile, mirrored from the ingestion FFmpeg reconnect
// backoff (video/engine.go) — the venue's internet has real packet loss, so a
// single attempt routinely loses an already-generated clip to a transient
// network blip.
const (
	uploadRetryBaseDelay = 2 * time.Second
	uploadRetryMaxDelay  = 30 * time.Second
	uploadMaxAttempts    = 4 // ponytail: fixed cap, revisit if 4 tries still isn't enough in the field
)

// putFile opens filePath and streams it to R2 under the given key and
// contentType, retrying with exponential backoff on failure — bounded by
// both uploadMaxAttempts and ctx's deadline, whichever comes first.
func (c *Client) putFile(ctx context.Context, filePath, key, contentType string) error {
	return retryWithBackoff(ctx, uploadMaxAttempts, uploadRetryBaseDelay, uploadRetryMaxDelay,
		func() error { return c.putFileOnce(ctx, filePath, key, contentType) },
		func(attempt int, err error) {
			c.logger.Warn("upload: retrying after failure",
				slog.String("key", key),
				slog.Int("attempt", attempt),
				slog.Any("error", err),
			)
		},
	)
}

// retryWithBackoff calls fn up to maxAttempts times, sleeping with
// exponential backoff (capped at maxDelay) between attempts. It returns nil
// on the first success, or fn's last error once attempts are exhausted or
// ctx is done. onRetry, if non-nil, is called before each backoff sleep.
func retryWithBackoff(ctx context.Context, maxAttempts int, baseDelay, maxDelay time.Duration, fn func() error, onRetry func(attempt int, err error)) error {
	backoff := baseDelay
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if attempt == maxAttempts {
			break
		}
		if onRetry != nil {
			onRetry(attempt, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxDelay)
	}
	return lastErr
}

func (c *Client) putFileOnce(ctx context.Context, filePath, key, contentType string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open %q: %w", filePath, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %q: %w", filePath, err)
	}

	_, err = c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          f,
		ContentLength: aws.Int64(info.Size()),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("r2 put %q: %w", key, err)
	}
	return nil
}

// Notify sends video metadata to POST /api/videos on the PHP backend,
// retrying with the same backoff as putFile — a clip that survived the R2
// upload shouldn't be lost to the same network blips on this much smaller
// request. A non-2xx response after all attempts is returned to the caller
// to log and continue.
func (c *Client) Notify(ctx context.Context, meta VideoMeta) error {
	return retryWithBackoff(ctx, uploadMaxAttempts, uploadRetryBaseDelay, uploadRetryMaxDelay,
		func() error { return c.notifyOnce(ctx, meta) },
		func(attempt int, err error) {
			c.logger.Warn("upload: retrying notify after failure",
				slog.String("video_id", meta.ID),
				slog.Int("attempt", attempt),
				slog.Any("error", err),
			)
		},
	)
}

func (c *Client) notifyOnce(ctx context.Context, meta VideoMeta) error {
	body, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.backend+"/api/videos", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("X-Api-Key", c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("backend returned %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
