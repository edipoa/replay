package upload

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeUploader struct {
	uploadErr, previewErr, notifyErr error
	uploadCalls, notifyCalls         int
}

func (f *fakeUploader) Upload(_ context.Context, _, _ string) (string, error) {
	f.uploadCalls++
	if f.uploadErr != nil {
		return "", f.uploadErr
	}
	return "videos/x/clip.mp4", nil
}
func (f *fakeUploader) UploadPreview(_ context.Context, _, _ string) (string, error) {
	if f.previewErr != nil {
		return "", f.previewErr
	}
	return "previews/x/preview.gif", nil
}
func (f *fakeUploader) Notify(_ context.Context, _ VideoMeta) error {
	f.notifyCalls++
	return f.notifyErr
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEnqueueMovesFiles(t *testing.T) {
	dir := t.TempDir()
	qdir := QueueDir(dir)
	clip := filepath.Join(dir, "replay_x_cam1.mp4")
	gif := clip + ".preview.gif"
	writeFile(t, clip, "video")
	writeFile(t, gif, "gif")

	err := Enqueue(qdir, clip, gif, Job{VideoID: "vid1", Meta: VideoMeta{CameraID: "cam1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(clip); !os.IsNotExist(err) {
		t.Error("original clip should have been moved")
	}
	for _, want := range []string{"replay_x_cam1.mp4", "replay_x_cam1.mp4.preview.gif", "vid1.json"} {
		if _, err := os.Stat(filepath.Join(qdir, want)); err != nil {
			t.Errorf("expected %s in queue dir: %v", want, err)
		}
	}
}

func TestRetryQueueCompletesAndResumes(t *testing.T) {
	dir := t.TempDir()
	qdir := QueueDir(dir)
	delivered := filepath.Join(dir, "delivered")
	clip := filepath.Join(dir, "replay_x_cam1.mp4")
	writeFile(t, clip, "video")

	if err := Enqueue(qdir, clip, "", Job{VideoID: "vid1", Meta: VideoMeta{CameraID: "cam1"}}); err != nil {
		t.Fatal(err)
	}

	var events []string
	emit := func(level, kind, _, _ string, _ ...any) { events = append(events, level+":"+kind) }
	mu := &sync.Mutex{}

	// First pass: upload fails → job stays.
	fu := &fakeUploader{uploadErr: errors.New("network")}
	processQueue(context.Background(), fu, qdir, delivered, time.Minute, mu, emit, quietLogger())
	if _, err := os.Stat(filepath.Join(qdir, "vid1.json")); err != nil {
		t.Fatal("job should still be queued after a failed pass")
	}

	// Second pass: upload ok, notify fails → clip uploaded flag persists.
	fu = &fakeUploader{notifyErr: errors.New("backend 500")}
	processQueue(context.Background(), fu, qdir, delivered, time.Minute, mu, emit, quietLogger())
	j, err := loadJob(qdir, "vid1.json")
	if err != nil || !j.ClipUploaded || j.Notified {
		t.Fatalf("expected clip_uploaded=true notified=false, got %+v (%v)", j, err)
	}

	// Third pass: everything ok → job done, clip in delivered/, must NOT re-upload.
	fu = &fakeUploader{}
	processQueue(context.Background(), fu, qdir, delivered, time.Minute, mu, emit, quietLogger())
	if fu.uploadCalls != 0 {
		t.Errorf("resumed job re-uploaded the clip (%d calls)", fu.uploadCalls)
	}
	if _, err := os.Stat(filepath.Join(qdir, "vid1.json")); !os.IsNotExist(err) {
		t.Error("completed job file should be gone")
	}
	if _, err := os.Stat(filepath.Join(delivered, "replay_x_cam1.mp4")); err != nil {
		t.Error("completed clip should be in delivered/")
	}

	if len(events) == 0 || events[len(events)-1] != "info:upload.ok" {
		t.Errorf("expected final event info:upload.ok, got %v", events)
	}
}

func TestRetryQueueStuckAlert(t *testing.T) {
	dir := t.TempDir()
	qdir := QueueDir(dir)
	clip := filepath.Join(dir, "replay_x_cam1.mp4")
	writeFile(t, clip, "video")
	if err := Enqueue(qdir, clip, "", Job{VideoID: "vid1", Meta: VideoMeta{CameraID: "cam1"}}); err != nil {
		t.Fatal(err)
	}
	// Backdate the job past the stuck threshold.
	j, _ := loadJob(qdir, "vid1.json")
	j.QueuedAt = time.Now().Add(-40 * time.Minute)
	_ = saveJob(qdir, j)

	var crit int
	emit := func(level, kind, _, _ string, _ ...any) {
		if level == "critical" && kind == "upload.stuck" {
			crit++
		}
	}
	fu := &fakeUploader{uploadErr: errors.New("still offline")}
	mu := &sync.Mutex{}

	processQueue(context.Background(), fu, qdir, filepath.Join(dir, "delivered"), time.Minute, mu, emit, quietLogger())
	processQueue(context.Background(), fu, qdir, filepath.Join(dir, "delivered"), time.Minute, mu, emit, quietLogger())

	if crit != 1 {
		t.Fatalf("stuck alert should fire exactly once, fired %d times", crit)
	}
}
