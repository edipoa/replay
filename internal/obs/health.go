package obs

import (
	"context"
	"syscall"
	"time"
)

const (
	heartbeatInterval = 2 * time.Minute

	// A camera is "down" once its newest buffer segment is older than this, and
	// "up" again once a fresh segment appears within cameraUpThreshold.
	cameraDownThreshold = 60 * time.Second
	cameraUpThreshold   = 30 * time.Second
)

// CameraHealth is the per-camera slice of a Snapshot.
type CameraHealth struct {
	ID         string `json:"id"`
	BufferAgeS int    `json:"buffer_age_s"` // -1 when no segment exists yet
	Up         bool   `json:"up"`
}

// Snapshot is the current health of the agent, served by /status.
type Snapshot struct {
	UptimeS          int            `json:"uptime_s"`
	Cameras          []CameraHealth `json:"cameras"`
	USBOK            bool           `json:"usb_ok"`
	LastClipAt       string         `json:"last_clip_at,omitempty"`
	ClipsToday       int            `json:"clips_today"`
	ClipsFailedToday int            `json:"clips_failed_today"`
	DiskFreePct      int            `json:"disk_free_pct"`
	AlertsFiring     []string       `json:"alerts_firing"`
}

// snapshot builds the current health picture from the buffer dirs on disk and
// the events table.
func (r *Recorder) snapshot() Snapshot {
	s := Snapshot{
		UptimeS:      int(time.Since(r.start).Seconds()),
		USBOK:        r.usbOK(),
		AlertsFiring: r.alerts.FiringGroups(),
		DiskFreePct:  diskFreePct(r.cfg.DBPath),
	}

	for id, dir := range r.cfg.BufferDirs {
		ch := CameraHealth{ID: id, BufferAgeS: -1}
		if r.cfg.NewestSegment != nil {
			if newest, ok := r.cfg.NewestSegment(dir); ok {
				age := time.Since(newest)
				ch.BufferAgeS = int(age.Seconds())
				ch.Up = age <= cameraDownThreshold
			}
		}
		s.Cameras = append(s.Cameras, ch)
	}

	if ts, ok := r.lastEventTime("clip.ready"); ok {
		s.LastClipAt = ts
	}
	s.ClipsToday = r.countTodayKind("clip.ready")
	s.ClipsFailedToday = r.countTodayKind("clip.failed")
	return s
}

// usbOK is true unless the most recent USB event was a disconnect.
func (r *Recorder) usbOK() bool {
	var kind string
	r.mu.Lock()
	err := r.db.QueryRow(
		`SELECT kind FROM events WHERE kind IN ('button.usb_connected','button.usb_disconnected') ORDER BY id DESC LIMIT 1`,
	).Scan(&kind)
	r.mu.Unlock()
	if err != nil {
		return true // no USB event seen yet — assume fine
	}
	return kind != "button.usb_disconnected"
}

func (r *Recorder) lastEventTime(kind string) (string, bool) {
	var ts string
	r.mu.Lock()
	err := r.db.QueryRow(`SELECT ts FROM events WHERE kind = ? ORDER BY id DESC LIMIT 1`, kind).Scan(&ts)
	r.mu.Unlock()
	return ts, err == nil
}

func (r *Recorder) countTodayKind(kind string) int {
	midnight := time.Now().UTC().Truncate(24 * time.Hour).Format(time.RFC3339)
	var n int
	r.mu.Lock()
	err := r.db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ? AND ts >= ?`, kind, midnight).Scan(&n)
	r.mu.Unlock()
	if err != nil {
		return 0
	}
	return n
}

// RunHeartbeat records a heartbeat event every heartbeatInterval and raises /
// clears camera.down alerts. Blocks until ctx is cancelled.
func (r *Recorder) RunHeartbeat(ctx context.Context) {
	// camDown tracks which cameras we've already alerted on, so we only emit
	// camera.down / camera.up on the transition.
	camDown := make(map[string]bool)

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s := r.snapshot()

			for _, cam := range s.Cameras {
				switch {
				case cam.BufferAgeS < 0 || cam.BufferAgeS > int(cameraDownThreshold.Seconds()):
					if !camDown[cam.ID] {
						camDown[cam.ID] = true
						Event(Critical, "camera.down", cam.ID, "câmera sem novos segmentos no buffer",
							"buffer_age_s", cam.BufferAgeS)
					}
				case cam.BufferAgeS <= int(cameraUpThreshold.Seconds()):
					if camDown[cam.ID] {
						camDown[cam.ID] = false
						Event(Info, "camera.up", cam.ID, "câmera voltou a produzir segmentos",
							"buffer_age_s", cam.BufferAgeS)
					}
				}
			}

			Event(Info, "heartbeat", "", "heartbeat",
				"uptime_s", s.UptimeS,
				"cameras", s.Cameras,
				"usb_ok", s.USBOK,
				"clips_today", s.ClipsToday,
				"clips_failed_today", s.ClipsFailedToday,
				"disk_free_pct", s.DiskFreePct,
			)
		}
	}
}

// diskFreePct returns the percentage of free space on the filesystem holding
// path, or -1 if it can't be determined.
func diskFreePct(path string) int {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return -1
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	if total == 0 {
		return -1
	}
	return int(free * 100 / total)
}
