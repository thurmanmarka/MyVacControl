package main

import "time"

// CleanTotals adds up a set of cleans.
type CleanTotals struct {
	Cleans  int `json:"cleans"`
	Minutes int `json:"minutes"`
	Area    int `json:"area"`   // m², from the cleans that reported it
	Faults  int `json:"faults"` // cleans that ended with a fault
}

func (t *CleanTotals) add(c CleanRun) {
	t.Cleans++
	t.Minutes += int(cleanLength(c).Round(time.Minute).Minutes())
	if c.Area != nil {
		t.Area += *c.Area
	}
	if c.ErrorCode != 0 {
		t.Faults++
	}
}

// WeekTotals is one Monday-to-Sunday week.
type WeekTotals struct {
	Start string `json:"start"` // the Monday, YYYY-MM-DD
	CleanTotals
}

type CleanStats struct {
	AllTime CleanTotals  `json:"all_time"`
	Last30  CleanTotals  `json:"last_30_days"`
	Weeks   []WeekTotals `json:"weeks"`           // oldest first, ending with this week
	Since   *time.Time   `json:"since,omitempty"` // the first clean recorded
}

// cleanLength is how long a clean took, pauses included.
func cleanLength(c CleanRun) time.Duration {
	if c.EndedAt == nil {
		return 0
	}
	return c.EndedAt.Sub(c.StartedAt)
}

// weekStart is midnight on the Monday of t's week, in t's time zone.
func weekStart(t time.Time) time.Time {
	y, m, d := t.Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, t.Location())
	return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
}

// summarize totals cleans (oldest first) for all time, the last 30 days and
// each of the last n weeks, in now's time zone.
func summarize(cleans []CleanRun, now time.Time, n int) CleanStats {
	first := weekStart(now).AddDate(0, 0, -7*(n-1))
	st := CleanStats{Weeks: make([]WeekTotals, n)}
	for i := range st.Weeks {
		st.Weeks[i].Start = first.AddDate(0, 0, 7*i).Format("2006-01-02")
	}
	month := now.AddDate(0, 0, -30)
	for _, c := range cleans {
		st.AllTime.add(c)
		if !c.StartedAt.Before(month) {
			st.Last30.add(c)
		}
		start := c.StartedAt.In(now.Location())
		if start.Before(first) {
			continue
		}
		// Rounded, as a week with a clock change isn't exactly 7×24 hours
		if i := int(weekStart(start).Sub(first).Hours()/(7*24) + 0.5); i < n {
			st.Weeks[i].add(c)
		}
	}
	if len(cleans) > 0 {
		st.Since = &cleans[0].StartedAt
	}
	return st
}

// PartStatus is how worn a part is.
type PartStatus struct {
	Part
	UsedHours float64    `json:"used_hours"`
	Left      float64    `json:"left"` // fraction of its life left, 0 to 1
	ResetAt   *time.Time `json:"reset_at,omitempty"`
	ResetBy   string     `json:"reset_by,omitempty"`
}

// partStatus works out a part's wear from the cleans (and the one under
// way) since it was last replaced, or from all of them if it never was.
func partStatus(p Part, reset *PartReset, cleans []CleanRun, current *CleanRun, now time.Time) PartStatus {
	ps := PartStatus{Part: p}
	var since time.Time
	if reset != nil {
		since, ps.ResetAt, ps.ResetBy = reset.At, &reset.At, reset.By
	}
	var used time.Duration
	for _, c := range cleans {
		if !c.StartedAt.Before(since) {
			used += cleanLength(c)
		}
	}
	if current != nil && !current.StartedAt.Before(since) {
		used += now.Sub(current.StartedAt)
	}
	ps.UsedHours = used.Hours()
	ps.Left = max(0, 1-ps.UsedHours/p.Hours)
	return ps
}
