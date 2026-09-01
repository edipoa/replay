package obs

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiveCamerasAndPage(t *testing.T) {
	dir := t.TempDir()
	// cam1 has a playlist, cam2 has only a stray segment (no playlist yet),
	// "notacam" is a plain file — only cam1 should be listed.
	if err := os.MkdirAll(filepath.Join(dir, "cam1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cam1", "index.m3u8"), []byte("#EXTM3U"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "cam2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cam2", "seg_1.ts"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notacam"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	r := &Recorder{cfg: Config{LiveDir: dir}}

	got := r.liveCameras()
	if len(got) != 1 || got[0] != "cam1" {
		t.Fatalf("liveCameras() = %v, want [cam1]", got)
	}

	rec := httptest.NewRecorder()
	r.handleLivePage(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `id="v-cam1"`) || !strings.Contains(body, `var cams = ["cam1"]`) {
		t.Fatalf("live page missing cam1 player:\n%s", body)
	}

	// Two cameras → the panoramic strip: both players, the strip container,
	// and the wall-clock sync loop.
	two := t.TempDir()
	for _, c := range []string{"cam1", "cam2"} {
		if err := os.MkdirAll(filepath.Join(two, c), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(two, c, "index.m3u8"), []byte("#EXTM3U"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rec = httptest.NewRecorder()
	(&Recorder{cfg: Config{LiveDir: two}}).handleLivePage(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if b := rec.Body.String(); !strings.Contains(b, `class="strip"`) ||
		!strings.Contains(b, `id="v-cam1"`) || !strings.Contains(b, `id="v-cam2"`) ||
		!strings.Contains(b, `var cams = ["cam1","cam2"]`) ||
		!strings.Contains(b, "function correction(") {
		t.Fatalf("two-camera page missing panorama strip / sync:\n%s", b)
	}

	// No cameras → the empty-state page, no <video>.
	empty := &Recorder{cfg: Config{LiveDir: t.TempDir()}}
	rec = httptest.NewRecorder()
	empty.handleLivePage(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if b := rec.Body.String(); strings.Contains(b, "<video") || !strings.Contains(b, "Nenhuma câmera") {
		t.Fatalf("expected empty-state page, got:\n%s", b)
	}
}

func TestLiveHandlerServesAndBlocksListing(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "cam1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cam1", "index.m3u8"), []byte("#EXTM3U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := (&Recorder{cfg: Config{LiveDir: dir}}).liveHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/live/cam1/index.m3u8", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("playlist GET = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Fatalf("playlist Content-Type = %q", ct)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/live/cam1/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("directory listing = %d, want 404", rec.Code)
	}
}
