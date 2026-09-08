// Package live carries the runtime on/off state of the near-live stream.
//
// The replay-site backend owns the schedule (slots + one-off games + a manual
// override in the live_control table). This agent is dumb: a poller fetches
// GET /api/live/state every ~30s and calls Gate.Set; the per-camera live FFmpeg
// loop blocks on Gate.WaitOn and is killed when the gate goes off. On a poll
// failure the gate keeps its last value, so a network blip never drops a live
// stream mid-game.
package live

import (
	"context"
	"sync"
	"time"
)

// State is the payload from GET /api/live/state, plus what the aovivo page needs
// to render the "fora do ar" screen.
type State struct {
	Mode            string     `json:"mode"`   // auto | force_on | force_off
	On              bool       `json:"on"`
	Reason          string     `json:"reason"` // slot | forced_on | forced_off | idle
	WindowStart     *time.Time `json:"window_start"`
	WindowEnd       *time.Time `json:"window_end"`
	NextWindowStart *time.Time `json:"next_window_start"`
}

// Gate is a single shared on/off latch (the live stream is all-cameras-or-none).
// The zero value is not usable — call NewGate. Safe for concurrent use.
type Gate struct {
	mu    sync.Mutex
	cond  *sync.Cond
	on    bool
	state State
}

// NewGate returns a Gate that starts off (no stream until the first poll).
func NewGate() *Gate {
	g := &Gate{}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// Set replaces the state and wakes every waiter. Called by the poller.
func (g *Gate) Set(s State) {
	g.mu.Lock()
	g.on = s.On
	g.state = s
	g.mu.Unlock()
	g.cond.Broadcast()
}

// IsOn reports the current latch value.
func (g *Gate) IsOn() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.on
}

// Snapshot returns the last state received from the backend (zero value until
// the first successful poll).
func (g *Gate) Snapshot() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

// WaitOn blocks until the gate is on or ctx is done. Returns true if it is now
// on, false if ctx ended first.
func (g *Gate) WaitOn(ctx context.Context) bool {
	stop := context.AfterFunc(ctx, g.cond.Broadcast)
	defer stop()

	g.mu.Lock()
	defer g.mu.Unlock()
	for !g.on && ctx.Err() == nil {
		g.cond.Wait()
	}
	return g.on && ctx.Err() == nil
}

// WaitOff blocks until the gate is off or ctx is done. Used by the live loop to
// kill FFmpeg as soon as the schedule (or the admin) turns the stream off.
func (g *Gate) WaitOff(ctx context.Context) {
	stop := context.AfterFunc(ctx, g.cond.Broadcast)
	defer stop()

	g.mu.Lock()
	defer g.mu.Unlock()
	for g.on && ctx.Err() == nil {
		g.cond.Wait()
	}
}
