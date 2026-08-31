package delivery

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/edipo/replay-saas/internal/obs"
)

const (
	deliveryTickInterval = 30 * time.Second
	cleanupTickInterval  = time.Hour
	deliveredTTL         = 24 * time.Hour
)

// inFlight tracks pending files currently being read by runReplay (R2 upload,
// preview generation) so the queue worker doesn't rename them out from under
// that in-progress work.
var inFlight sync.Map

// Lock marks path as in-flight; call the returned func once the caller is
// done reading/writing it.
func Lock(path string) func() {
	inFlight.Store(path, struct{}{})
	return func() { inFlight.Delete(path) }
}

// RunQueueWorker attempts to deliver pending clips to Telegram on a 30 s tick,
// moving each successful file from pending/ to delivered/. A separate hourly
// tick removes delivered files older than 24 h.
// It blocks until ctx is cancelled.
func RunQueueWorker(ctx context.Context, outputDir string, bot *Bot, logger *slog.Logger) {
	pendingDir := filepath.Join(outputDir, "pending")
	deliveredDir := filepath.Join(outputDir, "delivered")
	orphanedDir := filepath.Join(outputDir, "orphaned")

	deliveryTick := time.NewTicker(deliveryTickInterval)
	cleanupTick := time.NewTicker(cleanupTickInterval)
	defer deliveryTick.Stop()
	defer cleanupTick.Stop()

	// Attempt delivery immediately on startup rather than waiting 30 s.
	processPending(ctx, pendingDir, deliveredDir, orphanedDir, bot, logger)

	for {
		select {
		case <-ctx.Done():
			return
		case <-deliveryTick.C:
			processPending(ctx, pendingDir, deliveredDir, orphanedDir, bot, logger)
		case <-cleanupTick.C:
			cleanDelivered(deliveredDir, logger)
		}
	}
}

func processPending(ctx context.Context, pendingDir, deliveredDir, orphanedDir string, bot *Bot, logger *slog.Logger) {
	entries, err := os.ReadDir(pendingDir)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Error("queue: read pending dir", slog.Any("error", err))
		}
		return
	}

	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".mp4" {
			continue
		}

		srcPath := filepath.Join(pendingDir, entry.Name())
		dstPath := filepath.Join(deliveredDir, entry.Name())

		if _, busy := inFlight.Load(srcPath); busy {
			continue
		}

		triggerTime, err := parseReplayFilename(entry.Name())
		if err != nil {
			logger.Warn("queue: unparseable filename, moving to delivered",
				slog.String("file", entry.Name()))
			_ = os.Rename(srcPath, dstPath)
			continue
		}

		if bot == nil {
			// No Telegram configured: nothing to deliver here, but still move
			// the file out of pending/ so cleanDelivered's TTL sweep reclaims it.
			_ = os.Rename(srcPath, dstPath)
			continue
		}
		if err := bot.Deliver(ctx, srcPath, triggerTime); err != nil {
			if errors.Is(err, ErrNoSlotFound) {
				_ = os.MkdirAll(orphanedDir, 0o755)
				if renErr := os.Rename(srcPath, filepath.Join(orphanedDir, entry.Name())); renErr != nil {
					logger.Error("queue: failed to move to orphaned",
						slog.String("file", entry.Name()),
						slog.Any("error", renErr))
				} else {
					logger.Warn("queue: no slot found, moved to orphaned",
						slog.String("file", entry.Name()))
					obs.Event(obs.Warn, "deliver.orphaned", "", "clipe sem horário correspondente na agenda — movido pra orphaned/",
						slog.String("file", entry.Name()))
				}
			} else {
				logger.Warn("queue: delivery failed, will retry next tick",
					slog.String("file", entry.Name()),
					slog.Any("error", err))
				obs.Event(obs.Critical, "deliver.failed", "", "envio do clipe pro Telegram falhou — vai tentar de novo",
					slog.String("file", entry.Name()), slog.Any("error", err))
			}
			continue
		}

		logger.Info("queue: delivered", slog.String("file", entry.Name()))
		obs.Event(obs.Info, "deliver.ok", "", "clipe entregue no Telegram", slog.String("file", entry.Name()))
		_ = os.Rename(srcPath, dstPath)
	}
}

func cleanDelivered(deliveredDir string, logger *slog.Logger) {
	entries, err := os.ReadDir(deliveredDir)
	if err != nil {
		return
	}
	cutoff := time.Now().UTC().Add(-deliveredTTL)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(deliveredDir, entry.Name())
			if err := os.Remove(path); err != nil {
				logger.Warn("queue: failed to remove old clip",
					slog.String("path", path), slog.Any("error", err))
			} else {
				logger.Info("queue: removed old delivered clip", slog.String("path", path))
			}
		}
	}
}

// parseReplayFilename extracts the trigger time from filenames like
// replay_20060102_150405_cam1.mp4 (or the legacy replay_20060102_150405.mp4).
func parseReplayFilename(name string) (time.Time, error) {
	name = strings.TrimSuffix(name, ".mp4")
	name = strings.TrimPrefix(name, "replay_")
	// Drop optional _cameraID suffix: "20060102_150405_cam1" → "20060102_150405"
	parts := strings.SplitN(name, "_", 3)
	if len(parts) >= 2 {
		name = parts[0] + "_" + parts[1]
	}
	return time.Parse("20060102_150405", name)
}
