package main

import (
	"log"
	"sync"
	"time"
)

// startAttribution is how long after a start command a clean is credited to
// whoever sent it. Cleans that start otherwise were started from the
// vacuum's own app or button.
const startAttribution = 2 * time.Minute

// tracker turns a vacuum's status changes into CleanRuns.
type tracker struct {
	vacuumID string
	save     func(CleanRun)

	mu        sync.Mutex
	run       *CleanRun
	pendingBy string
	pendingAt time.Time
}

func newTracker(vacuumID string, save func(CleanRun)) *tracker {
	return &tracker{vacuumID: vacuumID, save: save}
}

// NoteStart records who just asked for a clean.
func (t *tracker) NoteStart(by string) {
	t.mu.Lock()
	t.pendingBy, t.pendingAt = by, time.Now()
	t.mu.Unlock()
}

// Update is the Device's onChange hook.
func (t *tracker) Update(before, after State) {
	t.mu.Lock()
	var finished *CleanRun
	now := time.Now()

	if t.run == nil && after.Phase == "cleaning" {
		by := "vacuum"
		if t.pendingBy != "" && now.Sub(t.pendingAt) < startAttribution {
			by = t.pendingBy
		}
		t.pendingBy = ""
		t.run = &CleanRun{
			VacuumID:     t.vacuumID,
			StartedAt:    now,
			StartedBy:    by,
			Mode:         after.Mode,
			BatteryStart: after.Battery,
		}
		log.Printf("🧹 %s: clean started (%s, by %s)", t.vacuumID, after.Mode, by)
	}

	if r := t.run; r != nil {
		// Only stats reported during this clean count; the first report can
		// be left over from the last one
		if after.Stats != nil && (before.Stats == nil || *before.Stats != *after.Stats) {
			r.Area = ptr(after.Stats.Area)
		}
		if after.ErrorCode != 0 {
			r.ErrorCode = after.ErrorCode
		}
		// A dropped connection or a pause doesn't end the clean
		if after.Connected && after.Phase == "idle" {
			r.EndedAt, r.EndStatus, r.BatteryEnd = &now, after.StatusLabel, after.Battery
			finished, t.run = r, nil
		}
	}
	t.mu.Unlock()

	if finished != nil {
		log.Printf("🏁 %s: clean finished after %s (%s)", t.vacuumID,
			finished.EndedAt.Sub(finished.StartedAt).Round(time.Second), finished.EndStatus)
		t.save(*finished)
	}
}

// Current returns the clean under way, if any.
func (t *tracker) Current() *CleanRun {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.run == nil {
		return nil
	}
	r := *t.run
	return &r
}
