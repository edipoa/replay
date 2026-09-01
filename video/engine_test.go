package video

import (
	"testing"
	"time"
)

// TestApplyDefaultsBufferDur guards the buffer retention default: it must
// stay well above the observed worst-case clip-generation time on the field
// hardware, or a queued press's segments get pruned before its own
// GenerateReplay call starts (see RUNBOOK_JOGO.md / video/engine.go).
func TestApplyDefaultsBufferDur(t *testing.T) {
	var c Config
	c.applyDefaults()

	if want := 300 * time.Second; c.BufferDur != want {
		t.Errorf("default BufferDur = %v, want %v", c.BufferDur, want)
	}
}
