// Package obs is the replay-agent's observability layer: every notable event
// (button press, USB drop, ingestion stall, clip failure, delivery failure) is
// recorded to a local SQLite database, exposed over a small HTTP status server,
// and — for critical events — pushed to Telegram with dedup.
//
// The SQLite file is the source of truth and works fully offline; the venue's
// internet is exactly what drops when things break, so nothing here depends on
// reaching a remote backend.
//
// Event kinds (stable strings — grep for these when building metrics):
//
//	button.press          button.debounced      button.repress_dropped
//	button.usb_connected   button.usb_disconnected
//	ingest.started         ingest.exited         ingest.stalled
//	ingest.behind          camera.down           camera.up
//	clip.ready             clip.failed           clip.stale
//	deliver.ok             deliver.failed        deliver.orphaned
//	upload.ok              upload.failed         notify.failed
//	agent.started          agent.stopped         heartbeat
package obs

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Level classifies an event. Critical events additionally go to the alerter.
type Level string

const (
	Info     Level = "info"
	Warn     Level = "warn"
	Critical Level = "critical"
)

// eventRetention is how long rows are kept; older rows are pruned on startup
// and once a day. Event volume is a few hundred rows/day, so 30 days is tiny.
const eventRetention = 30 * 24 * time.Hour

// TelegramSender is the subset of *delivery.Bot the alerter needs. Kept as an
// interface here so this package doesn't import delivery (which imports obs).
type TelegramSender interface {
	SendText(ctx context.Context, text string, threadID int64) error
}

// Config configures a Recorder.
type Config struct {
	DBPath      string            // SQLite file, e.g. <output>/replay.db
	HTTPAddr    string            // status server listen addr, e.g. ":8088"
	Bot         TelegramSender    // nil disables alerting (events still recorded)
	AlertThread int64             // Telegram message_thread_id for alerts (0 = main feed)
	BufferDirs  map[string]string // cameraID -> buffer dir, for the health snapshot

	// TriggerToken, when non-empty, is the shared PIN required to fire a replay
	// via POST /trigger (and the /botao phone page). Blank = no auth, fine on a
	// trusted LAN; set it before exposing the server to the internet.
	TriggerToken string

	// NewestSegment reports the newest buffer segment time in dir. Injected
	// (rather than importing video) to avoid an import cycle, since video
	// records events through this package. nil disables per-camera freshness.
	NewestSegment func(dir string) (time.Time, bool)
}

// Recorder is the singleton set via SetDefault and reached through Event.
type Recorder struct {
	cfg   Config
	db    *sql.DB
	mu    sync.Mutex // serialises writes (SQLite, single connection)
	start time.Time

	alerts *alerter

	triggerMu sync.RWMutex
	trigger   func(time.Time) // set by SetTrigger; fires a replay from POST /trigger
}

// SetTrigger registers the callback that POST /trigger invokes — the same
// onPress the physical button uses. Called once from main after onPress exists.
func (r *Recorder) SetTrigger(fn func(time.Time)) {
	r.triggerMu.Lock()
	r.trigger = fn
	r.triggerMu.Unlock()
}

// New opens (creating if needed) the SQLite database, applies the schema,
// prunes old rows, and returns a ready Recorder. The HTTP server and heartbeat
// are started separately via ServeHTTP / RunHeartbeat.
func New(cfg Config) (*Recorder, error) {
	db, err := sql.Open("sqlite", cfg.DBPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// One connection: writes are already serialised by r.mu, and this avoids
	// SQLITE_BUSY on the low-power deploy box.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}

	r := &Recorder{cfg: cfg, db: db, start: time.Now()}
	r.alerts = newAlerter(cfg.Bot, cfg.AlertThread)
	r.prune()
	return r, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS events (
  id     INTEGER PRIMARY KEY AUTOINCREMENT,
  ts     TEXT NOT NULL,
  level  TEXT NOT NULL,
  kind   TEXT NOT NULL,
  camera TEXT,
  msg    TEXT,
  fields TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);
CREATE INDEX IF NOT EXISTS idx_events_kind_ts ON events(kind, ts);
`

// Close flushes and closes the database.
func (r *Recorder) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
}

// prune deletes rows older than eventRetention.
func (r *Recorder) prune() {
	cutoff := time.Now().UTC().Add(-eventRetention).Format(time.RFC3339)
	r.mu.Lock()
	_, err := r.db.Exec(`DELETE FROM events WHERE ts < ?`, cutoff)
	r.mu.Unlock()
	if err != nil {
		slog.Warn("obs: prune failed", slog.Any("error", err))
	}
}

// ─── Singleton ────────────────────────────────────────────────────────────────

var (
	defaultMu sync.RWMutex
	def       *Recorder
)

// SetDefault installs r as the package-wide recorder (mirrors slog.SetDefault).
func SetDefault(r *Recorder) {
	defaultMu.Lock()
	def = r
	defaultMu.Unlock()
}

// Default returns the recorder set by SetDefault, or nil.
func Default() *Recorder {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return def
}

// Event records an event: mirrored to slog (so journald keeps working), written
// to SQLite, and — when level is Critical — handed to the alerter. kv is an
// alternating key/value list like slog. Safe to call on a nil Recorder (no-op
// beyond the slog line) and from any goroutine.
func Event(level Level, kind, camera, msg string, kv ...any) {
	// Always emit a structured log line regardless of recorder state.
	attrs := []any{slog.String("kind", kind)}
	if camera != "" {
		attrs = append(attrs, slog.String("camera", camera))
	}
	attrs = append(attrs, kv...)
	switch level {
	case Critical:
		slog.Error(msg, attrs...)
	case Warn:
		slog.Warn(msg, attrs...)
	default:
		slog.Info(msg, attrs...)
	}

	r := Default()
	if r == nil {
		return
	}
	r.record(level, kind, camera, msg, kvToJSON(kv))

	if level == Critical {
		r.alerts.fire(kind, camera, msg)
	} else if healthyKinds[kind] {
		r.alerts.clear(kind, camera)
	}
}

func (r *Recorder) record(level Level, kind, camera, msg, fields string) {
	r.mu.Lock()
	_, err := r.db.Exec(
		`INSERT INTO events (ts, level, kind, camera, msg, fields) VALUES (?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), string(level), kind, camera, msg, fields,
	)
	r.mu.Unlock()
	if err != nil {
		slog.Warn("obs: event insert failed", slog.Any("error", err), slog.String("kind", kind))
	}
}

// kvToJSON renders a slog-style argument list to a JSON object. It accepts both
// loose "key", value pairs and slog.Attr values (e.g. slog.Time("t", x)), so
// callers can use whichever reads best.
func kvToJSON(kv []any) string {
	if len(kv) == 0 {
		return ""
	}
	m := make(map[string]any, len(kv))
	for i := 0; i < len(kv); i++ {
		switch a := kv[i].(type) {
		case slog.Attr:
			m[a.Key] = jsonValue(a.Value.Any())
		case string:
			if i+1 < len(kv) {
				m[a] = jsonValue(kv[i+1])
				i++
			}
		}
	}
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// jsonValue coerces values that don't JSON-encode usefully (errors, durations)
// into readable strings.
func jsonValue(v any) any {
	switch x := v.(type) {
	case error:
		return x.Error()
	case time.Duration:
		return x.String()
	default:
		return v
	}
}
