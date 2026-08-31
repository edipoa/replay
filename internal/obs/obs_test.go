package obs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeBot records the alert messages the alerter would send to Telegram.
type fakeBot struct {
	mu   sync.Mutex
	msgs []string
}

func (f *fakeBot) SendText(_ context.Context, text string, _ int64) error {
	f.mu.Lock()
	f.msgs = append(f.msgs, text)
	f.mu.Unlock()
	return nil
}

func (f *fakeBot) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.msgs)
}

func newTestRecorder(t *testing.T, bot TelegramSender) *Recorder {
	t.Helper()
	r, err := New(Config{
		DBPath: filepath.Join(t.TempDir(), "test.db"),
		Bot:    bot,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	SetDefault(r)
	t.Cleanup(func() { SetDefault(nil) })
	return r
}

func TestEventRoundTrip(t *testing.T) {
	r := newTestRecorder(t, nil)

	Event(Info, "button.press", "", "apertou")
	Event(Warn, "ingest.behind", "cam1", "atrasado")
	Event(Critical, "clip.failed", "cam2", "sem segmentos")

	// All three via /events.
	rr := httptest.NewRecorder()
	r.handleEvents(rr, httptest.NewRequest(http.MethodGet, "/events", nil))
	var all []eventRow
	if err := json.Unmarshal(rr.Body.Bytes(), &all); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 events, got %d", len(all))
	}

	// Filter by level.
	rr = httptest.NewRecorder()
	r.handleEvents(rr, httptest.NewRequest(http.MethodGet, "/events?level=critical", nil))
	var crit []eventRow
	_ = json.Unmarshal(rr.Body.Bytes(), &crit)
	if len(crit) != 1 || crit[0].Kind != "clip.failed" {
		t.Fatalf("level filter: got %+v", crit)
	}
}

func TestAlerterDedupAndRecovery(t *testing.T) {
	bot := &fakeBot{}
	newTestRecorder(t, bot)

	// Same broken group three times → one alert.
	for i := 0; i < 3; i++ {
		Event(Critical, "camera.down", "cam1", "sem segmentos")
	}
	waitFor(t, func() bool { return bot.count() == 1 })

	// A different group → a second alert.
	Event(Critical, "clip.failed", "cam1", "concat falhou")
	waitFor(t, func() bool { return bot.count() == 2 })

	// Healthy signal for cam1's camera group → recovery message.
	Event(Info, "camera.up", "cam1", "voltou")
	waitFor(t, func() bool { return bot.count() == 3 })

	bot.mu.Lock()
	last := bot.msgs[2]
	bot.mu.Unlock()
	if want := "recuperado"; !contains(last, want) {
		t.Fatalf("recovery msg = %q, want to contain %q", last, want)
	}
}

func TestIndexPageRenders(t *testing.T) {
	r := newTestRecorder(t, nil)
	r.cfg.BufferDirs = map[string]string{"cam1": "/x"}
	Event(Info, "button.press", "", "teste")

	rr := httptest.NewRecorder()
	r.handleIndex(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rr.Body.String()
	for _, want := range []string{"<title>replay-agent status", "Gerar replay agora", "testar alerta", "post('trigger'", "post('test-alert'", "toLocaleString"} {
		if !contains(body, want) {
			t.Fatalf("index page missing %q", want)
		}
	}

	// On the botao.* hostname, "/" serves the bare button page, no dashboard.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "botao.vianasociety.com.br"
	rr = httptest.NewRecorder()
	r.handleIndex(rr, req)
	body = rr.Body.String()
	if !contains(body, "GERAR<br>REPLAY") || contains(body, "últimos eventos") {
		t.Fatalf("botao host should serve button-only page, got:\n%s", body)
	}
}

func TestTriggerEndpoint(t *testing.T) {
	r := newTestRecorder(t, nil)

	// Before SetTrigger: 200 with ok:false, no panic.
	rr := httptest.NewRecorder()
	r.handleTrigger(rr, httptest.NewRequest(http.MethodPost, "/trigger", nil))
	if got := rr.Body.String(); !contains(got, `"ok": false`) {
		t.Fatalf("pre-SetTrigger: want ok:false, got %s", got)
	}

	// GET is rejected.
	rr = httptest.NewRecorder()
	r.handleTrigger(rr, httptest.NewRequest(http.MethodGet, "/trigger", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /trigger: want 405, got %d", rr.Code)
	}

	// After SetTrigger: the callback fires with a UTC time.
	var got time.Time
	r.SetTrigger(func(tt time.Time) { got = tt })
	rr = httptest.NewRecorder()
	r.handleTrigger(rr, httptest.NewRequest(http.MethodPost, "/trigger", nil))
	if got.IsZero() || time.Since(got) > time.Minute {
		t.Fatalf("trigger callback not invoked with a fresh time: %v", got)
	}
	if !contains(rr.Body.String(), `"ok": true`) {
		t.Fatalf("want ok:true, got %s", rr.Body.String())
	}
}

func TestTriggerToken(t *testing.T) {
	r := newTestRecorder(t, nil)
	r.cfg.TriggerToken = "1234"
	fired := 0
	r.SetTrigger(func(time.Time) { fired++ })

	// No / wrong PIN → 401, callback never runs.
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/trigger", nil),
		httptest.NewRequest(http.MethodPost, "/trigger?token=nope", nil),
	} {
		rr := httptest.NewRecorder()
		r.handleTrigger(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", rr.Code)
		}
	}

	// Correct PIN via header and via query param both fire.
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/trigger?token=1234", nil),
		httptest.NewRequest(http.MethodPost, "/trigger", nil),
	} {
		req.Header.Set("X-Trigger-Token", "1234")
		rr := httptest.NewRecorder()
		r.handleTrigger(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (%s)", rr.Code, rr.Body.String())
		}
	}
	if fired != 2 {
		t.Fatalf("want 2 fires, got %d", fired)
	}
}

func TestSnapshotCameraDown(t *testing.T) {
	r, err := New(Config{
		DBPath:     filepath.Join(t.TempDir(), "s.db"),
		BufferDirs: map[string]string{"cam1": "/x", "cam2": "/y"},
		NewestSegment: func(dir string) (time.Time, bool) {
			if dir == "/x" {
				return time.Now(), true // fresh
			}
			return time.Now().Add(-5 * time.Minute), true // stale
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	byID := map[string]CameraHealth{}
	for _, c := range r.snapshot().Cameras {
		byID[c.ID] = c
	}
	if !byID["cam1"].Up {
		t.Errorf("cam1 should be up")
	}
	if byID["cam2"].Up {
		t.Errorf("cam2 should be down (5min stale)")
	}
}

func TestPrune(t *testing.T) {
	r := newTestRecorder(t, nil)

	old := time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339)
	r.mu.Lock()
	_, err := r.db.Exec(`INSERT INTO events (ts, level, kind, msg) VALUES (?, 'info', 'heartbeat', 'ancient')`, old)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	Event(Info, "heartbeat", "", "recent")

	r.prune()

	var n int
	r.mu.Lock()
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n)
	r.mu.Unlock()
	if n != 1 {
		t.Fatalf("after prune want 1 row, got %d", n)
	}
}

func TestProblems(t *testing.T) {
	if p := problems(Snapshot{USBOK: true, DiskFreePct: 50, Cameras: []CameraHealth{{ID: "cam1", Up: true}}}); len(p) != 0 {
		t.Fatalf("healthy snapshot should have no problems, got %v", p)
	}
	p := problems(Snapshot{
		USBOK:            false,
		DiskFreePct:      4,
		ClipsFailedToday: 2,
		Cameras:          []CameraHealth{{ID: "cam1", Up: true}, {ID: "cam2", Up: false}},
		AlertsFiring:     []string{"camera/cam2", "upload"},
	})
	// USB, cam2, disk, failures, upload alert — camera/cam2 is deduped against cam2.
	if len(p) != 5 {
		t.Fatalf("want 5 problems, got %d: %v", len(p), p)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
