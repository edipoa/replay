//go:build !windows

// Package joystick provides cross-platform USB joystick/gamepad input.
// On Linux it reads from /dev/input/js{id} using the kernel joystick API.
// On Windows it calls winmm.dll joyGetPosEx.
package joystick

import (
	"encoding/binary"
	"fmt"
	"os"
	"sync"
)

// Joystick represents an open joystick device.
type Joystick struct {
	f       *os.File
	mu      sync.Mutex
	buttons uint32
	done    chan struct{}
}

// jsEvent mirrors the Linux kernel struct js_event (8 bytes, native endian).
type jsEvent struct {
	Time   uint32
	Value  int16
	Type   uint8
	Number uint8
}

const jsEventButton = 0x01

// Open opens the joystick at /dev/input/js{id}.
func Open(id int) (*Joystick, error) {
	f, err := os.Open(fmt.Sprintf("/dev/input/js%d", id))
	if err != nil {
		return nil, err
	}
	j := &Joystick{f: f, done: make(chan struct{})}
	go j.readLoop()
	return j, nil
}

// readLoop reads joystick events in a dedicated goroutine and maintains the
// current button bitmask. It exits when the file is closed.
func (j *Joystick) readLoop() {
	defer close(j.done)
	var ev jsEvent
	for {
		if err := binary.Read(j.f, binary.NativeEndian, &ev); err != nil {
			return
		}
		// Mask out the init flag (0x80); we only care about button events.
		if ev.Type&0x7F != jsEventButton {
			continue
		}
		j.mu.Lock()
		if ev.Value == 1 {
			j.buttons |= 1 << uint(ev.Number)
		} else {
			j.buttons &^= 1 << uint(ev.Number)
		}
		j.mu.Unlock()
	}
}

// Poll returns the current button bitmask (bit N = button N pressed).
// Returns an error if the device has been disconnected.
func (j *Joystick) Poll() (uint32, error) {
	select {
	case <-j.done:
		return 0, fmt.Errorf("joystick disconnected")
	default:
	}
	j.mu.Lock()
	b := j.buttons
	j.mu.Unlock()
	return b, nil
}

// Close closes the device and waits for the read goroutine to exit.
func (j *Joystick) Close() {
	j.f.Close()
	<-j.done
}
