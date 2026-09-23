package obs

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSponsorFiles(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"b.PNG", "a.jpg", "notes.txt"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644)
	}
	os.Mkdir(filepath.Join(dir, "sub.png"), 0o755)
	want := []string{filepath.Join(dir, "a.jpg"), filepath.Join(dir, "b.PNG")}
	if got := SponsorFiles(dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := SponsorFiles(filepath.Join(dir, "missing")); got != nil {
		t.Fatalf("missing dir: %v", got)
	}

	r := &Recorder{cfg: Config{SponsorDir: dir}}
	if l := r.sponsorList(); len(l) != 2 || l[0].Name != "a" || l[0].Img != "/sponsors/a.jpg" {
		t.Fatalf("sponsorList: %+v", l)
	}
	h := r.sponsorsHandler()
	for path, code := range map[string]int{"/sponsors/a.jpg": 200, "/sponsors/notes.txt": 404, "/sponsors/": 404, "/sponsors/gone.png": 404} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != code {
			t.Errorf("%s: got %d want %d", path, rec.Code, code)
		}
	}

	// Live page: strip lists the folder's logos; empty folder = no strip.
	page := func(r *Recorder) string {
		rec := httptest.NewRecorder()
		r.handleLivePage(rec, httptest.NewRequest("GET", "/", nil))
		return rec.Body.String()
	}
	r.cfg.LiveDir = t.TempDir() // one live camera, else the page is the "no cameras" screen
	os.MkdirAll(filepath.Join(r.cfg.LiveDir, "cam1"), 0o755)
	os.WriteFile(filepath.Join(r.cfg.LiveDir, "cam1", "index.m3u8"), nil, 0o644)
	if body := page(r); !strings.Contains(body, `<img src="/sponsors/a.jpg" alt="a"`) || !strings.Contains(body, `<img src="/sponsors/b.PNG"`) {
		t.Error("live page missing sponsor logos from folder")
	}
	os.Remove(filepath.Join(dir, "a.jpg"))
	os.Remove(filepath.Join(dir, "b.PNG"))
	if body := page(r); strings.Contains(body, `<aside class="sponsors">`) {
		t.Error("sponsor strip rendered with empty folder")
	}
}
