//go:build windows

// Package joystick provides cross-platform USB joystick/gamepad input.
// On Windows it calls winmm.dll joyGetPosEx (Windows Multimedia API).
package joystick

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	winmm       = syscall.NewLazyDLL("winmm.dll")
	joyGetPosEx = winmm.NewProc("joyGetPosEx")
)

// joyinfoex mirrors the Win32 JOYINFOEX struct (52 bytes).
type joyinfoex struct {
	Size      uint32
	Flags     uint32
	Xpos      uint32
	Ypos      uint32
	Zpos      uint32
	Rpos      uint32
	Upos      uint32
	Vpos      uint32
	Buttons   uint32
	ButtonNum uint32
	POV       uint32
	Reserved1 uint32
	Reserved2 uint32
}

const joyReturnButtons = 0x80

// Joystick represents an open joystick device.
type Joystick struct{ id int }

// Open verifies the joystick at index id exists and returns a handle.
func Open(id int) (*Joystick, error) {
	info := joyinfoex{
		Size:  uint32(unsafe.Sizeof(joyinfoex{})),
		Flags: joyReturnButtons,
	}
	ret, _, _ := joyGetPosEx.Call(uintptr(id), uintptr(unsafe.Pointer(&info)))
	if ret != 0 {
		return nil, fmt.Errorf("joystick %d not found (winmm error %d)", id, ret)
	}
	return &Joystick{id: id}, nil
}

// Poll returns the current button bitmask (bit N = button N pressed).
func (j *Joystick) Poll() (uint32, error) {
	info := joyinfoex{
		Size:  uint32(unsafe.Sizeof(joyinfoex{})),
		Flags: joyReturnButtons,
	}
	ret, _, _ := joyGetPosEx.Call(uintptr(j.id), uintptr(unsafe.Pointer(&info)))
	if ret != 0 {
		return 0, fmt.Errorf("joyGetPosEx error: %d", ret)
	}
	return info.Buttons, nil
}

// Close is a no-op on Windows (no file handle to close).
func (j *Joystick) Close() {}
