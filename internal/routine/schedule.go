// Package routine computes when scheduled routines run: the schedule
// kinds the UI offers (once, hourly, every n hours, daily, weekdays,
// weekly, monthly, yearly) and five-field cron expressions, in an IANA time
// zone.
package routine

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the hub image has no zoneinfo
)

// Schedule kinds.
const (
	Once     = "once"
	Hourly   = "hourly"
	EveryN   = "every_n_hours"
	Daily    = "daily"
	Weekdays = "weekdays"
	Weekly   = "weekly"
	Monthly  = "monthly"
	Yearly   = "yearly"
	Cron     = "cron"
)

// Schedule says when a routine runs.
type Schedule struct {
	Kind string `json:"kind"`
	// At is the time of day, "HH:MM" (hourly and every_n_hours: only the
	// minute counts).
	At string `json:"at,omitempty"`
	// Days are weekdays for weekly, 0 = Sunday.
	Days []int `json:"days,omitempty"`
	// Day is the day of the month (monthly, yearly); a month without it
	// runs on its last day.
	Day int `json:"day,omitempty"`
	// Month is 1-12 (yearly).
	Month int `json:"month,omitempty"`
	// Every is the number of hours (every_n_hours), counted from midnight.
	Every int `json:"every,omitempty"`
	// Date is "YYYY-MM-DD" (once).
	Date string `json:"date,omitempty"`
	// Cron is a five-field expression (cron).
	Cron string `json:"cron,omitempty"`
	// TZ is an IANA zone; empty means UTC.
	TZ string `json:"tz,omitempty"`
}

// Validate checks the schedule.
func (s Schedule) Validate() error {
	if _, err := s.location(); err != nil {
		return err
	}
	switch s.Kind {
	case Once:
		if _, err := time.Parse("2006-01-02", s.Date); err != nil {
			return errors.New("a one-time routine needs a date (YYYY-MM-DD)")
		}
	case Hourly, Daily, Weekdays:
	case EveryN:
		if s.Every < 1 || s.Every > 23 {
			return errors.New("every n hours: n is 1 to 23")
		}
	case Weekly:
		if len(s.Days) == 0 {
			return errors.New("a weekly routine needs at least one day")
		}
		for _, d := range s.Days {
			if d < 0 || d > 6 {
				return errors.New("weekdays are 0 (Sunday) to 6")
			}
		}
	case Monthly:
		if s.Day < 1 || s.Day > 31 {
			return errors.New("a monthly routine needs a day of the month (1-31)")
		}
	case Yearly:
		if s.Month < 1 || s.Month > 12 || s.Day < 1 || s.Day > 31 {
			return errors.New("a yearly routine needs a month and a day")
		}
	case Cron:
		if _, err := parseCron(s.Cron); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("unknown schedule kind %q", s.Kind)
	}
	if _, _, err := s.clock(); err != nil {
		return err
	}
	return nil
}

func (s Schedule) location() (*time.Location, error) {
	if s.TZ == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(s.TZ)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q", s.TZ)
	}
	return loc, nil
}

// clock is At as hour and minute ("" is 09:00, hourly kinds use only the
// minute).
func (s Schedule) clock() (int, int, error) {
	at := s.At
	if at == "" {
		at = "09:00"
	}
	t, err := time.Parse("15:04", at)
	if err != nil {
		return 0, 0, errors.New("the time is HH:MM")
	}
	return t.Hour(), t.Minute(), nil
}

// Next is the first run strictly after after; the zero time when there is
// none (a one-time routine that has passed).
func (s Schedule) Next(after time.Time) time.Time {
	loc, err := s.location()
	if err != nil {
		return time.Time{}
	}
	if s.Kind == Cron {
		c, err := parseCron(s.Cron)
		if err != nil {
			return time.Time{}
		}
		return c.next(after.In(loc))
	}
	h, m, err := s.clock()
	if err != nil {
		return time.Time{}
	}
	t := after.In(loc)
	switch s.Kind {
	case Once:
		d, err := time.ParseInLocation("2006-01-02", s.Date, loc)
		if err != nil {
			return time.Time{}
		}
		run := time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, loc)
		if run.After(after) {
			return run
		}
		return time.Time{}
	case Hourly:
		run := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), m, 0, 0, loc)
		for !run.After(after) {
			run = run.Add(time.Hour)
		}
		return run
	case EveryN:
		for day := 0; day < 3; day++ {
			base := time.Date(t.Year(), t.Month(), t.Day()+day, 0, m, 0, 0, loc)
			for hour := 0; hour < 24; hour += s.Every {
				run := time.Date(base.Year(), base.Month(), base.Day(), hour, m, 0, 0, loc)
				if run.After(after) {
					return run
				}
			}
		}
		return time.Time{}
	}
	// Daily kinds: look day by day.
	for day := 0; day < 800; day++ {
		d := time.Date(t.Year(), t.Month(), t.Day()+day, 0, 0, 0, 0, loc)
		if !s.matchesDay(d) {
			continue
		}
		run := time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, loc)
		if run.After(after) {
			return run
		}
	}
	return time.Time{}
}

func (s Schedule) matchesDay(d time.Time) bool {
	switch s.Kind {
	case Daily:
		return true
	case Weekdays:
		return d.Weekday() >= time.Monday && d.Weekday() <= time.Friday
	case Weekly:
		for _, wd := range s.Days {
			if int(d.Weekday()) == wd {
				return true
			}
		}
		return false
	case Monthly:
		return d.Day() == min(s.Day, daysIn(d.Year(), d.Month()))
	case Yearly:
		return int(d.Month()) == s.Month && d.Day() == min(s.Day, daysIn(d.Year(), d.Month()))
	}
	return false
}

func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// Runs lists the runs in [from, to), at most limit.
func (s Schedule) Runs(from, to time.Time, limit int) []time.Time {
	var out []time.Time
	t := from.Add(-time.Nanosecond)
	for len(out) < limit {
		t = s.Next(t)
		if t.IsZero() || !t.Before(to) {
			break
		}
		out = append(out, t)
	}
	return out
}

// Describe is the schedule in words: "Weekdays at 07:00".
func (s Schedule) Describe() string {
	h, m, _ := s.clock()
	at := fmt.Sprintf("%02d:%02d", h, m)
	days := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	switch s.Kind {
	case Once:
		return "Once on " + s.Date + " at " + at
	case Hourly:
		return fmt.Sprintf("Every hour at :%02d", m)
	case EveryN:
		return fmt.Sprintf("Every %d hours at :%02d", s.Every, m)
	case Daily:
		return "Every day at " + at
	case Weekdays:
		return "Weekdays at " + at
	case Weekly:
		var names []string
		for _, d := range s.Days {
			if d >= 0 && d < 7 {
				names = append(names, days[d])
			}
		}
		return "Every " + strings.Join(names, ", ") + " at " + at
	case Monthly:
		return fmt.Sprintf("Monthly on day %d at %s", s.Day, at)
	case Yearly:
		return fmt.Sprintf("Yearly on %s %d at %s", time.Month(s.Month), s.Day, at)
	case Cron:
		return "Cron " + s.Cron
	}
	return s.Kind
}

// ---- cron ----

type cronSpec struct {
	minute, hour, dom, month, dow [64]bool
	domStar, dowStar              bool
}

func parseCron(expr string) (*cronSpec, error) {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, errors.New("a cron expression has five fields: minute hour day-of-month month day-of-week")
	}
	c := &cronSpec{domStar: f[2] == "*", dowStar: f[4] == "*"}
	for i, spec := range []struct {
		set      *[64]bool
		min, max int
	}{{&c.minute, 0, 59}, {&c.hour, 0, 23}, {&c.dom, 1, 31}, {&c.month, 1, 12}, {&c.dow, 0, 7}} {
		if err := cronField(f[i], spec.min, spec.max, spec.set); err != nil {
			return nil, fmt.Errorf("cron field %d (%q): %v", i+1, f[i], err)
		}
	}
	if c.dow[7] {
		c.dow[0] = true
	}
	return c, nil
}

func cronField(field string, lo, hi int, set *[64]bool) error {
	for _, part := range strings.Split(field, ",") {
		step := 1
		if base, s, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 {
				return errors.New("bad step")
			}
			step, part = n, base
		}
		from, to := lo, hi
		switch {
		case part == "*":
		case strings.Contains(part, "-"):
			a, b, _ := strings.Cut(part, "-")
			x, err1 := strconv.Atoi(a)
			y, err2 := strconv.Atoi(b)
			if err1 != nil || err2 != nil {
				return errors.New("bad range")
			}
			from, to = x, y
		default:
			x, err := strconv.Atoi(part)
			if err != nil {
				return errors.New("not a number")
			}
			from, to = x, x
			if step > 1 {
				to = hi
			}
		}
		if from < lo || to > hi || from > to {
			return fmt.Errorf("out of range %d-%d", lo, hi)
		}
		for v := from; v <= to; v += step {
			set[v] = true
		}
	}
	return nil
}

// next walks minute by minute, skipping whole hours and days that cannot
// match; at most a few years ahead.
func (c *cronSpec) next(after time.Time) time.Time {
	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := after.AddDate(5, 0, 0)
	for t.Before(limit) {
		if !c.month[int(t.Month())] {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !c.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !c.hour[t.Hour()] {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
			continue
		}
		if !c.minute[t.Minute()] {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

// dayMatches follows cron: with both day fields restricted, either may
// match.
func (c *cronSpec) dayMatches(t time.Time) bool {
	dom, dow := c.dom[t.Day()], c.dow[int(t.Weekday())]
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dow
	case c.dowStar:
		return dom
	}
	return dom || dow
}
