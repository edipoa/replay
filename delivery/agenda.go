// Package delivery handles routing and uploading replay clips to Telegram topics.
// It has no knowledge of video recording, FFmpeg, or physical triggers.
package delivery

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ─── Portuguese weekday names ─────────────────────────────────────────────────

// weekdaysPT maps time.Weekday (Sunday=0) to the Portuguese name used as the
// first component of an agenda key (e.g. "Segunda", "Terça", "Sábado").
var weekdaysPT = [7]string{
	"Domingo", // 0 – Sunday
	"Segunda", // 1 – Monday
	"Terça",   // 2 – Tuesday
	"Quarta",  // 3 – Wednesday
	"Quinta",  // 4 – Thursday
	"Sexta",   // 5 – Friday
	"Sábado",  // 6 – Saturday
}

func weekdayPT(d time.Weekday) string { return weekdaysPT[d] }

// ─── Agenda ───────────────────────────────────────────────────────────────────

// ErrNoSlotFound is returned by ResolveThreadID when no agenda slot covers the
// requested time. The caller may log the error and skip delivery rather than
// crashing.
var ErrNoSlotFound = errors.New("delivery: no agenda slot found for current time")

// Agenda maps scheduling keys to Telegram message_thread_id values.
//
// Key format: "<Weekday>-<HH:MM>"
//
// Each key marks the START of a time slot. ResolveThreadID returns the thread
// for the most recently started slot on the given day. If no slot has started
// yet today, ErrNoSlotFound is returned.
//
// Example agenda.json:
//
//	{
//	  "Segunda-19:00": 112233,
//	  "Segunda-21:00": 445566,
//	  "Quarta-20:00":  778899,
//	  "Sexta-19:00":   101010,
//	  "Sábado-10:00":  111213
//	}
type Agenda map[string]int64

// LoadAgenda reads and parses a JSON file at path into an Agenda.
// The file is re-read on every call, so operators can update the schedule
// without restarting the service.
func LoadAgenda(path string) (Agenda, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read agenda %q: %w", path, err)
	}

	var a Agenda
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("parse agenda %q: %w", path, err)
	}

	if len(a) == 0 {
		return nil, fmt.Errorf("agenda %q contains no slots", path)
	}

	return a, nil
}

// ResolveThreadID returns the message_thread_id for the most recently started
// slot on the weekday of t that has already begun (slotStart ≤ t).
//
// When multiple slots exist on the same day, it returns the one whose start
// time is closest to (but not after) t — i.e. the currently "active" game.
//
// Returns ErrNoSlotFound if:
//   - there are no slots for the current weekday, or
//   - all slots on that day start after t.
func (a Agenda) ResolveThreadID(t time.Time) (int64, error) {
	day := weekdayPT(t.Weekday())
	prefix := day + "-"

	type candidate struct {
		start    time.Time
		threadID int64
	}

	var best *candidate

	for key, tid := range a {
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		timeStr := strings.TrimPrefix(key, prefix) // e.g. "19:00"
		slotStart, err := parseSlotTime(t, timeStr)
		if err != nil {
			// Malformed key: log-worthy, but don't crash — skip it.
			continue
		}

		// Only consider slots that have already begun.
		if slotStart.After(t) {
			continue
		}

		if best == nil || slotStart.After(best.start) {
			c := candidate{start: slotStart, threadID: tid}
			best = &c
		}
	}

	if best == nil {
		return 0, fmt.Errorf("%w: weekday=%s time=%s",
			ErrNoSlotFound, day, t.Format("15:04"))
	}

	return best.threadID, nil
}

// parseSlotTime reconstructs a full time.Time for a "HH:MM" string on the
// same calendar date as base.
func parseSlotTime(base time.Time, timeStr string) (time.Time, error) {
	parts := strings.SplitN(timeStr, ":", 2)
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("invalid time %q: expected HH:MM", timeStr)
	}

	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return time.Time{}, fmt.Errorf("invalid hour in %q", timeStr)
	}

	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return time.Time{}, fmt.Errorf("invalid minute in %q", timeStr)
	}

	return time.Date(
		base.Year(), base.Month(), base.Day(),
		h, m, 0, 0,
		base.Location(),
	), nil
}
