package routine

import (
	"testing"
	"time"
)

func TestNext(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	// Friday 2026-11-06 10:30 in Berlin.
	now := time.Date(2026, 11, 6, 10, 30, 0, 0, berlin)
	cases := []struct {
		name string
		s    Schedule
		want time.Time
	}{
		{"daily later today", Schedule{Kind: Daily, At: "17:00", TZ: "Europe/Berlin"}, time.Date(2026, 11, 6, 17, 0, 0, 0, berlin)},
		{"daily tomorrow", Schedule{Kind: Daily, At: "07:00", TZ: "Europe/Berlin"}, time.Date(2026, 11, 7, 7, 0, 0, 0, berlin)},
		{"weekdays skip weekend", Schedule{Kind: Weekdays, At: "07:00", TZ: "Europe/Berlin"}, time.Date(2026, 11, 9, 7, 0, 0, 0, berlin)},
		{"weekly", Schedule{Kind: Weekly, Days: []int{2, 4}, At: "08:20", TZ: "Europe/Berlin"}, time.Date(2026, 11, 10, 8, 20, 0, 0, berlin)},
		{"hourly", Schedule{Kind: Hourly, At: "00:15", TZ: "Europe/Berlin"}, time.Date(2026, 11, 6, 11, 15, 0, 0, berlin)},
		{"every 4 hours", Schedule{Kind: EveryN, Every: 4, At: "00:00", TZ: "Europe/Berlin"}, time.Date(2026, 11, 6, 12, 0, 0, 0, berlin)},
		{"monthly late day clamps", Schedule{Kind: Monthly, Day: 31, At: "09:00", TZ: "Europe/Berlin"}, time.Date(2026, 11, 30, 9, 0, 0, 0, berlin)},
		{"yearly", Schedule{Kind: Yearly, Month: 1, Day: 1, At: "00:00", TZ: "Europe/Berlin"}, time.Date(2027, 1, 1, 0, 0, 0, 0, berlin)},
		{"once future", Schedule{Kind: Once, Date: "2026-11-08", At: "12:00", TZ: "Europe/Berlin"}, time.Date(2026, 11, 8, 12, 0, 0, 0, berlin)},
		{"once past", Schedule{Kind: Once, Date: "2026-11-01", At: "12:00", TZ: "Europe/Berlin"}, time.Time{}},
		{"cron every 15", Schedule{Kind: Cron, Cron: "*/15 * * * *", TZ: "Europe/Berlin"}, time.Date(2026, 11, 6, 10, 45, 0, 0, berlin)},
		{"cron weekdays 9", Schedule{Kind: Cron, Cron: "0 9 * * 1-5", TZ: "Europe/Berlin"}, time.Date(2026, 11, 9, 9, 0, 0, 0, berlin)},
		{"utc default", Schedule{Kind: Daily, At: "12:00"}, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		if err := c.s.Validate(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := c.s.Next(now)
		if !got.Equal(c.want) {
			t.Errorf("%s: Next = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDST(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	// The night clocks go back (2026-10-25): 07:00 local stays 07:00.
	s := Schedule{Kind: Daily, At: "07:00", TZ: "Europe/Berlin"}
	got := s.Next(time.Date(2026, 10, 24, 8, 0, 0, 0, berlin))
	if want := time.Date(2026, 10, 25, 7, 0, 0, 0, berlin); !got.Equal(want) || got.UTC().Hour() != 6 {
		t.Fatalf("Next across DST = %v (%v UTC)", got, got.UTC())
	}
}

func TestValidate(t *testing.T) {
	for _, s := range []Schedule{
		{Kind: "sometimes"},
		{Kind: Weekly},
		{Kind: Daily, At: "25:00"},
		{Kind: Cron, Cron: "* * *"},
		{Kind: Cron, Cron: "61 * * * *"},
		{Kind: Daily, TZ: "Mars/Olympus"},
		{Kind: EveryN, Every: 0},
		{Kind: Once, Date: "tomorrow"},
	} {
		if s.Validate() == nil {
			t.Errorf("%+v validated", s)
		}
	}
}

func TestRunsAndDescribe(t *testing.T) {
	s := Schedule{Kind: Weekdays, At: "07:00"}
	from := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC) // Monday
	runs := s.Runs(from, from.AddDate(0, 0, 7), 50)
	if len(runs) != 5 {
		t.Fatalf("a week of weekday runs = %d", len(runs))
	}
	if d := s.Describe(); d != "Weekdays at 07:00" {
		t.Fatalf("Describe = %q", d)
	}
}
