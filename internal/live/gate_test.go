package live

import (
	"context"
	"testing"
	"time"
)

func TestGateWaitOnReleasesWhenSetOn(t *testing.T) {
	g := NewGate()

	done := make(chan bool, 1)
	go func() { done <- g.WaitOn(context.Background()) }()

	// Still off — WaitOn must be blocked.
	select {
	case <-done:
		t.Fatal("WaitOn returned before the gate was turned on")
	case <-time.After(20 * time.Millisecond):
	}

	g.Set(State{On: true, Reason: "slot"})

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("WaitOn returned false after Set(on)")
		}
	case <-time.After(time.Second):
		t.Fatal("WaitOn did not wake after Set(on)")
	}
}

func TestGateWaitOnRespectsContext(t *testing.T) {
	g := NewGate()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan bool, 1)
	go func() { done <- g.WaitOn(ctx) }()

	cancel()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("WaitOn returned true after ctx cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("WaitOn did not wake on ctx cancel")
	}
}

func TestGateWaitOffWakesOnSetOff(t *testing.T) {
	g := NewGate()
	g.Set(State{On: true})

	done := make(chan struct{})
	go func() { g.WaitOff(context.Background()); close(done) }()

	select {
	case <-done:
		t.Fatal("WaitOff returned while the gate was still on")
	case <-time.After(20 * time.Millisecond):
	}

	g.Set(State{On: false, Reason: "idle"})

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("WaitOff did not wake after Set(off)")
	}
}

func TestGateSnapshot(t *testing.T) {
	g := NewGate()
	if g.Snapshot().On {
		t.Fatal("zero gate should be off")
	}
	want := State{On: true, Mode: "auto", Reason: "slot"}
	g.Set(want)
	if got := g.Snapshot(); got != want {
		t.Fatalf("Snapshot = %+v, want %+v", got, want)
	}
}
