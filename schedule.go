package main

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
)

var hhmm = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// Validate checks a schedule against the vacuum it's for.
func (sc *Schedule) Validate(v *Vacuum) error {
	sc.Name = strings.TrimSpace(sc.Name)
	if sc.Name == "" {
		sc.Name = "Clean"
	}
	if len(sc.Name) > 64 {
		return fmt.Errorf("name must be at most 64 characters")
	}
	if sc.Days <= 0 || sc.Days > 127 {
		return fmt.Errorf("pick at least one day")
	}
	if !hhmm.MatchString(sc.At) {
		return fmt.Errorf("time must be HH:MM (24-hour)")
	}
	if !contains(v.Modes, sc.Mode) {
		return fmt.Errorf("unknown mode %q", sc.Mode)
	}
	if sc.Suction != "" && !contains(v.Suction, sc.Suction) {
		return fmt.Errorf("unknown suction level %q", sc.Suction)
	}
	return nil
}

// Due reports whether the schedule should run in the minute that contains now.
func (sc *Schedule) Due(now time.Time) bool {
	if !sc.Enabled || sc.Days&(1<<int(now.Weekday())) == 0 || now.Format("15:04") != sc.At {
		return false
	}
	minute := now.Truncate(time.Minute)
	return sc.LastRun == nil || sc.LastRun.Before(minute)
}

// runScheduler checks the schedules every 20 seconds, in the Pi's local time.
func runScheduler(stop <-chan struct{}) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			scheds, err := db.Schedules("")
			if err != nil {
				log.Printf("⚠️  could not read schedules: %v", err)
				continue
			}
			for _, sc := range scheds {
				if sc.Due(now) {
					runSchedule(sc, now)
				}
			}
		}
	}
}

func runSchedule(sc Schedule, now time.Time) {
	// Marked first so a failure isn't retried every tick for the rest of the minute
	if err := db.MarkScheduleRun(sc.ID, now); err != nil {
		log.Printf("⚠️  could not mark schedule %d as run: %v", sc.ID, err)
		return
	}
	vac, ok := vacuums[sc.VacuumID]
	if !ok {
		log.Printf("⚠️  schedule %q is for unknown vacuum %q", sc.Name, sc.VacuumID)
		return
	}
	by := "schedule: " + sc.Name
	if st := vac.dev.State(); st.Phase == "cleaning" || st.Phase == "paused" {
		log.Printf("⏭️  %s: skipped schedule %q, already cleaning", sc.VacuumID, sc.Name)
		db.RecordCommand(CommandEntry{User: by, VacuumID: sc.VacuumID, Action: "start", Detail: "Skipped: already cleaning", OK: true})
		return
	}
	if sc.Suction != "" {
		if err := vac.run(by, "suction", sc.Suction); err != nil {
			return
		}
	}
	if err := vac.run(by, "start", sc.Mode); err == nil {
		log.Printf("⏰ %s: started schedule %q (%s)", sc.VacuumID, sc.Name, sc.Mode)
	}
}
