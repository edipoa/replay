package obs

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// alertGroup collapses related kinds so one root cause is one alert.
// e.g. "camera.down" for cam1 and "clip.failed" for cam1 are different groups,
// but repeated "camera.down" for cam1 is the same group.
func alertGroup(kind, camera string) string {
	switch kind {
	case "button.usb_disconnected", "button.usb_connected":
		return "usb"
	case "camera.down", "camera.up", "ingest.stalled":
		if camera != "" {
			return "camera/" + camera
		}
		return "camera"
	case "clip.failed", "clip.ready":
		return "clip"
	case "upload.failed", "upload.ok", "upload.stuck":
		return "upload"
	case "deliver.failed", "deliver.ok":
		return "delivery"
	default:
		return kind
	}
}

// healthyKinds are the events that clear a firing alert for their group.
var healthyKinds = map[string]bool{
	"button.usb_connected": true,
	"camera.up":            true,
	"clip.ready":           true,
	"upload.ok":            true,
	"deliver.ok":           true,
}

// reAlertInterval is the minimum spacing between repeat alerts for a group that
// stays broken — a safety net; dedup by "firing" flag is the main mechanism.
const reAlertInterval = 5 * time.Minute

type firing struct {
	since    time.Time
	lastSent time.Time
}

// alerter turns critical events into deduplicated Telegram messages plus a
// "recovered" message when the group goes healthy again.
type alerter struct {
	bot      TelegramSender
	threadID int64

	mu      sync.Mutex
	firing  map[string]*firing
	events  chan alertMsg
	started bool
}

type alertMsg struct {
	text  string
	group string
}

func newAlerter(bot TelegramSender, threadID int64) *alerter {
	a := &alerter{
		bot:      bot,
		threadID: threadID,
		firing:   make(map[string]*firing),
		events:   make(chan alertMsg, 32),
	}
	if bot != nil {
		a.started = true
		go a.sendLoop()
	}
	return a
}

// fire is called for every critical event.
func (a *alerter) fire(kind, camera, msg string) {
	group := alertGroup(kind, camera)
	now := time.Now()

	a.mu.Lock()
	f := a.firing[group]
	if f == nil {
		f = &firing{since: now}
		a.firing[group] = f
	}
	send := f.lastSent.IsZero() || now.Sub(f.lastSent) >= reAlertInterval
	if send {
		f.lastSent = now
	}
	a.mu.Unlock()

	if !send {
		return
	}
	a.enqueue(alertMsg{
		group: group,
		text:  fmt.Sprintf("⚠️ %s\n%s", group, msg),
	})
}

// clear is called for every healthy event; if its group was firing, sends a
// recovery message.
func (a *alerter) clear(kind, camera string) {
	group := alertGroup(kind, camera)

	a.mu.Lock()
	f := a.firing[group]
	if f == nil {
		a.mu.Unlock()
		return
	}
	down := time.Since(f.since).Round(time.Second)
	delete(a.firing, group)
	a.mu.Unlock()

	a.enqueue(alertMsg{
		group: group,
		text:  fmt.Sprintf("✅ recuperado: %s (fora por %s)", group, down),
	})
}

// FiringGroups returns the names of currently-firing alert groups (for /status).
func (a *alerter) FiringGroups() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.firing))
	for g := range a.firing {
		out = append(out, g)
	}
	return out
}

func (a *alerter) enqueue(m alertMsg) {
	if !a.started {
		return
	}
	select {
	case a.events <- m:
	default:
		slog.Warn("obs: alert queue full, dropping", slog.String("group", m.group))
	}
}

// sendLoop delivers queued alerts best-effort with one short retry. Events are
// already durable in SQLite and /status is the real fallback, so a persistent
// queue would be over-engineering here.
func (a *alerter) sendLoop() {
	for m := range a.events {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := a.bot.SendText(ctx, m.text, a.threadID)
		if err != nil {
			time.Sleep(2 * time.Second)
			err = a.bot.SendText(ctx, m.text, a.threadID)
		}
		cancel()
		if err != nil {
			slog.Warn("obs: alert send failed", slog.String("group", m.group), slog.Any("error", err))
		}
	}
}
