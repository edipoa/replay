package upload

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// A clip whose live upload failed is handed to this on-disk queue: the .mp4
// (and preview .gif) are moved into <OutputDir>/r2_queue/ next to a
// <videoID>.json job file. A single worker retries each job on a timer,
// resuming from whichever sub-step (clip upload / preview / backend notify)
// last failed. Jobs survive process restarts.

const (
	retryQueueInterval = 2 * time.Minute
	// jobStuckAfter is how long a job can sit unfinished before it raises a
	// critical alert (once). It keeps retrying regardless.
	jobStuckAfter = 30 * time.Minute
)

// Job is the persisted state of one clip waiting to reach R2 + the backend.
type Job struct {
	VideoID         string    `json:"video_id"`
	ClipPath        string    `json:"clip_path"`
	PreviewPath     string    `json:"preview_path,omitempty"`
	Meta            VideoMeta `json:"meta"`
	QueuedAt        time.Time `json:"queued_at"`
	ClipUploaded    bool      `json:"clip_uploaded"`
	PreviewUploaded bool      `json:"preview_uploaded"`
	Notified        bool      `json:"notified"`
	StuckAlerted    bool      `json:"stuck_alerted"`
}

func (j *Job) done() bool { return j.ClipUploaded && j.Notified }

// uploaderAPI is the slice of *Client the retry worker needs (an interface so
// tests can substitute a fake).
type uploaderAPI interface {
	Upload(ctx context.Context, filePath, videoID string) (string, error)
	UploadPreview(ctx context.Context, gifPath, videoID string) (string, error)
	Notify(ctx context.Context, meta VideoMeta) error
}

// EventFunc lets the worker report to the observability layer without this
// package importing it. level is "info" | "warn" | "critical".
type EventFunc func(level, kind, camera, msg string, kv ...any)

// QueueDir returns the retry-queue directory for a given OutputDir.
func QueueDir(outputDir string) string { return filepath.Join(outputDir, "r2_queue") }

// Enqueue moves clipPath (and previewPath, if it exists) into dir and writes
// j as <videoID>.json. j's ClipPath/PreviewPath/QueuedAt are set here.
func Enqueue(dir, clipPath, previewPath string, j Job) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	j.QueuedAt = time.Now().UTC()

	clipDst := filepath.Join(dir, filepath.Base(clipPath))
	if err := os.Rename(clipPath, clipDst); err != nil {
		return fmt.Errorf("move clip into queue: %w", err)
	}
	j.ClipPath = clipDst

	if previewPath != "" {
		if _, err := os.Stat(previewPath); err == nil {
			pdst := filepath.Join(dir, filepath.Base(previewPath))
			if os.Rename(previewPath, pdst) == nil {
				j.PreviewPath = pdst
			}
		}
	}
	return saveJob(dir, &j)
}

// RunRetryQueue processes dir every retryQueueInterval until ctx is done.
// Completed clips are moved to deliveredDir. attemptTimeout bounds each upload
// attempt; mu is shared with live uploads so they never run concurrently.
func RunRetryQueue(ctx context.Context, u uploaderAPI, dir, deliveredDir string, attemptTimeout time.Duration, mu *sync.Mutex, emit EventFunc, logger *slog.Logger) {
	ticker := time.NewTicker(retryQueueInterval)
	defer ticker.Stop()

	processQueue(ctx, u, dir, deliveredDir, attemptTimeout, mu, emit, logger) // sweep leftovers on startup

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			processQueue(ctx, u, dir, deliveredDir, attemptTimeout, mu, emit, logger)
		}
	}
}

func processQueue(ctx context.Context, u uploaderAPI, dir, deliveredDir string, attemptTimeout time.Duration, mu *sync.Mutex, emit EventFunc, logger *slog.Logger) {
	jobs, err := listJobs(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Error("upload queue: read dir", slog.Any("error", err))
		}
		return
	}
	for _, name := range jobs {
		if ctx.Err() != nil {
			return
		}
		j, err := loadJob(dir, name)
		if err != nil {
			logger.Warn("upload queue: bad job file, skipping", slog.String("file", name), slog.Any("error", err))
			continue
		}

		age := time.Since(j.QueuedAt)
		if age > jobStuckAfter && !j.StuckAlerted {
			emit("critical", "upload.stuck", j.Meta.CameraID,
				"clipe preso na fila de upload — internet do campo não sobe",
				"video_id", j.VideoID, "idade_min", int(age.Minutes()))
			j.StuckAlerted = true
			_ = saveJob(dir, j)
		}

		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		err = tryJob(attemptCtx, u, dir, j, mu)
		cancel()

		if !j.done() {
			if err != nil {
				logger.Warn("upload queue: retry failed, will try again",
					slog.String("video_id", j.VideoID), slog.Any("error", err))
			}
			continue
		}

		// Success: move the clip to delivered/ (local 24h copy) and drop the job.
		if err := os.MkdirAll(deliveredDir, 0o755); err == nil {
			_ = os.Rename(j.ClipPath, filepath.Join(deliveredDir, filepath.Base(j.ClipPath)))
		}
		if j.PreviewPath != "" {
			_ = os.Remove(j.PreviewPath)
		}
		_ = os.Remove(filepath.Join(dir, name))
		emit("info", "upload.ok", j.Meta.CameraID, "clipe subiu pro R2 pela fila de retry",
			"key", j.Meta.R2Key, "atraso_s", int(time.Since(j.QueuedAt).Seconds()))
	}
}

// tryJob runs the still-pending steps in order, saving progress after each so a
// crash or timeout never repeats a completed step. Preview failure is
// non-fatal. Returns the last error when the job is still not done.
func tryJob(ctx context.Context, u uploaderAPI, dir string, j *Job, mu *sync.Mutex) error {
	if !j.ClipUploaded {
		mu.Lock()
		key, err := u.Upload(ctx, j.ClipPath, j.VideoID)
		mu.Unlock()
		if err != nil {
			return err
		}
		j.ClipUploaded = true
		j.Meta.R2Key = key
		_ = saveJob(dir, j)
	}

	if !j.PreviewUploaded && j.PreviewPath != "" {
		if key, err := u.UploadPreview(ctx, j.PreviewPath, j.VideoID); err == nil {
			j.PreviewUploaded = true
			j.Meta.ThumbnailKey = key
			_ = saveJob(dir, j)
		}
	}

	if !j.Notified {
		if err := u.Notify(ctx, j.Meta); err != nil {
			return err
		}
		j.Notified = true
		_ = saveJob(dir, j)
	}
	return nil
}

// ─── Job file I/O ────────────────────────────────────────────────────────────

func listJobs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out) // stable order = oldest first (videoIDs aren't ordered, but deterministic)
	return out, nil
}

func loadJob(dir, name string) (*Job, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	var j Job
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

func saveJob(dir string, j *Job) error {
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, j.VideoID+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, j.VideoID+".json"))
}
