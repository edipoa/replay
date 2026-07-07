// seed insere vídeos de simulação no backend PHP para testar o site.
//
// Para cada câmera e cada trigger simulado:
//  1. Gera um MP4 de teste com FFmpeg (barras coloridas + overlay de timestamp)
//  2. Faz upload para o Cloudflare R2
//  3. Notifica o backend PHP via POST /api/videos
//
// Uso:
//
//	go run ./cmd/seed/
//	go run ./cmd/seed/ -n 10 -days 7 -cam cam1 -duration 30
//
// Variáveis de ambiente (ou .env na raiz):
//
//	REPLAY_R2_ACCOUNT_ID, REPLAY_R2_ACCESS_KEY_ID, REPLAY_R2_SECRET_ACCESS_KEY
//	REPLAY_R2_BUCKET, REPLAY_BACKEND_URL, REPLAY_BACKEND_API_KEY
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/edipo/replay-saas/internal/envutil"
	"github.com/edipo/replay-saas/upload"
)

func main() {
	n := flag.Int("n", 5, "número de vídeos por câmera")
	days := flag.Int("days", 30, "distribuir triggers nos últimos N dias")
	camList := flag.String("cam", "cam1,cam2", "câmeras (separadas por vírgula)")
	durationS := flag.Int("duration", 30, "duração de cada clipe em segundos")
	dryRun := flag.Bool("dry-run", false, "gera os vídeos mas não envia ao backend")
	ffmpegBin := flag.String("ffmpeg", "ffmpeg", "caminho para o binário ffmpeg")
	flag.Parse()

	_ = godotenv.Load()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cameras := strings.Split(*camList, ",")
	for i, c := range cameras {
		cameras[i] = strings.TrimSpace(c)
	}

	var uploader *upload.Client
	if !*dryRun {
		u, err := upload.New(upload.Config{
			AccountID:   envutil.MustEnv("REPLAY_R2_ACCOUNT_ID"),
			AccessKeyID: envutil.MustEnv("REPLAY_R2_ACCESS_KEY_ID"),
			SecretKey:   envutil.MustEnv("REPLAY_R2_SECRET_ACCESS_KEY"),
			Bucket:      envutil.MustEnv("REPLAY_R2_BUCKET"),
			BackendURL:  envutil.MustEnv("REPLAY_BACKEND_URL"),
			APIKey:      os.Getenv("REPLAY_BACKEND_API_KEY"),
		}, logger)
		if err != nil {
			logger.Error("upload client", "err", err)
			os.Exit(1)
		}
		uploader = u
	}

	tmpDir, err := os.MkdirTemp("", "replay-seed-*")
	if err != nil {
		logger.Error("mkdirtemp", "err", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	triggers := randomTriggers(*n*len(cameras), *days)
	idx := 0

	total, failed := 0, 0
	ctx := context.Background()

	for _, cam := range cameras {
		for i := 0; i < *n; i++ {
			t := triggers[idx]
			idx++

			logger.Info("gerando clipe", "cam", cam, "trigger", t.Format(time.RFC3339), "seq", i+1)

			videoPath, err := generateTestVideo(*ffmpegBin, tmpDir, cam, t, *durationS)
			if err != nil {
				logger.Error("ffmpeg falhou", "cam", cam, "err", err)
				failed++
				continue
			}

			total++

			if *dryRun {
				info, _ := os.Stat(videoPath)
				logger.Info("dry-run: clipe gerado",
					"path", videoPath,
					"size_kb", info.Size()/1024,
				)
				continue
			}

			videoID := upload.NewID()
			r2Key, err := uploader.Upload(ctx, videoPath, videoID)
			if err != nil {
				logger.Error("upload R2 falhou", "cam", cam, "err", err)
				failed++
				continue
			}

			info, _ := os.Stat(videoPath)
			meta := upload.VideoMeta{
				ID:          videoID,
				CameraID:    cam,
				R2Key:       r2Key,
				DurationS:   *durationS,
				SizeBytes:   info.Size(),
				TriggeredAt: t,
			}

			if err := uploader.Notify(ctx, meta); err != nil {
				logger.Warn("backend notify falhou", "cam", cam, "err", err)
			} else {
				logger.Info("ok",
					"cam", cam,
					"id", videoID,
					"r2_key", r2Key,
					"trigger", t.Format(time.RFC3339),
				)
			}

			os.Remove(videoPath)
		}
	}

	fmt.Printf("\n--- seed concluído ---\n")
	fmt.Printf("gerados: %d | falhas: %d\n", total, failed)
	if *dryRun {
		fmt.Println("(dry-run: nenhum upload realizado)")
	}
}

// generateTestVideo cria um MP4 de teste com barras SMPTE + overlay de texto.
func generateTestVideo(ffmpegBin, dir, camID string, t time.Time, durationS int) (string, error) {
	// Texto simples sem caracteres especiais para o parser de filtros do FFmpeg.
	label := fmt.Sprintf("SIMULACAO - %s - %s", strings.ToUpper(camID), t.Format("02-01-2006 15.04.05"))
	outPath := filepath.Join(dir, fmt.Sprintf("seed_%s_%s.mp4", camID, t.Format("20060102_150405")))

	// Escreve o texto em arquivo para evitar problemas de escaping no filtro drawtext.
	txtPath := outPath + ".txt"
	if err := os.WriteFile(txtPath, []byte(label), 0o600); err != nil {
		return "", fmt.Errorf("write textfile: %w", err)
	}
	defer os.Remove(txtPath)

	args := []string{
		"-y",
		"-f", "lavfi",
		// SMPTE color bars at 720p, 30fps
		"-i", fmt.Sprintf("smptebars=size=1280x720:rate=30:duration=%d", durationS),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", durationS),
		// burn label from textfile (evita escaping manual)
		"-vf", fmt.Sprintf(
			"drawtext=fontsize=36:fontcolor=white:x=(w-text_w)/2:y=(h-text_h)/2:textfile=%s:box=1:boxcolor=black@0.6:boxborderw=10",
			txtPath,
		),
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "28",
		"-c:a", "aac", "-b:a", "64k",
		"-movflags", "+faststart",
		"-t", fmt.Sprintf("%d", durationS),
		outPath,
	}

	cmd := exec.Command(ffmpegBin, args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg: %w", err)
	}
	return outPath, nil
}

// randomTriggers gera n timestamps aleatórios nos últimos days dias.
func randomTriggers(n, days int) []time.Time {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	now := time.Now().UTC()
	window := time.Duration(days) * 24 * time.Hour

	triggers := make([]time.Time, n)
	for i := range triggers {
		offset := time.Duration(rng.Int63n(int64(window)))
		triggers[i] = now.Add(-offset)
	}
	return triggers
}

