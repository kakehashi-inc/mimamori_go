package modules

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("time zone %s not available: %v", name, err)
	}
	return loc
}

func TestScheduleDueTimes(t *testing.T) {
	tokyo := mustLoc(t, "Asia/Tokyo")
	ny := mustLoc(t, "America/New_York")
	jst := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04:05", s, tokyo)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	utc := func(s string) time.Time {
		v, err := time.Parse("2006-01-02 15:04:05", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	daily := &CommandSchedule{Kind: ScheduleDaily, Time: "03:00"}
	weekly := &CommandSchedule{Kind: ScheduleWeekly, Weekdays: []string{"mon", "fri"}, Time: "03:00"}
	every6h := &CommandSchedule{Kind: ScheduleEveryHours, Every: 6}
	every15m := &CommandSchedule{Kind: ScheduleEveryMinutes, Every: 15}
	hourly30 := &CommandSchedule{Kind: ScheduleHourlyAt, Minute: intPtr(30)}
	once := &CommandSchedule{Kind: ScheduleOnce, At: "2026-12-01T03:00:00Z"}

	tests := []struct {
		name         string
		s            *CommandSchedule
		loc          *time.Location
		at           time.Time
		latest, next time.Time // zero: none
	}{
		{"daily before", daily, tokyo, jst("2026-11-12 02:59:59"), jst("2026-11-11 03:00:00"), jst("2026-11-12 03:00:00")},
		{"daily exactly", daily, tokyo, jst("2026-11-12 03:00:00"), jst("2026-11-12 03:00:00"), jst("2026-11-13 03:00:00")},
		{"daily across midnight", daily, tokyo, jst("2026-12-31 23:59:00"), jst("2026-12-31 03:00:00"), jst("2027-01-01 03:00:00")},
		{"weekly thursday", weekly, tokyo, jst("2026-11-12 10:00:00"), jst("2026-11-09 03:00:00"), jst("2026-11-13 03:00:00")},
		{"weekly saturday", weekly, tokyo, jst("2026-11-14 10:00:00"), jst("2026-11-13 03:00:00"), jst("2026-11-16 03:00:00")},
		{"weekly monday early", weekly, tokyo, jst("2026-11-16 02:00:00"), jst("2026-11-13 03:00:00"), jst("2026-11-16 03:00:00")},
		{"every 6 hours", every6h, tokyo, jst("2026-11-12 13:59:00"), jst("2026-11-12 12:00:00"), jst("2026-11-12 18:00:00")},
		{"every 6 hours across midnight", every6h, tokyo, jst("2026-11-12 23:30:00"), jst("2026-11-12 18:00:00"), jst("2026-11-13 00:00:00")},
		{"every 15 minutes", every15m, tokyo, jst("2026-11-12 10:44:59"), jst("2026-11-12 10:30:00"), jst("2026-11-12 10:45:00")},
		{"every 15 minutes across midnight", every15m, tokyo, jst("2026-11-12 23:50:00"), jst("2026-11-12 23:45:00"), jst("2026-11-13 00:00:00")},
		{"hourly at 30", hourly30, tokyo, jst("2026-11-12 10:29:00"), jst("2026-11-12 09:30:00"), jst("2026-11-12 10:30:00")},
		{"once before", once, tokyo, utc("2026-12-01 02:59:59"), time.Time{}, utc("2026-12-01 03:00:00")},
		{"once after", once, tokyo, utc("2026-12-01 03:00:00"), utc("2026-12-01 03:00:00"), time.Time{}},
		// New York, 2026-03-08: 02:00 EST jumps to 03:00 EDT. 02:30 does not
		// exist and becomes 03:30 EDT (07:30Z).
		{"daily nonexistent time", &CommandSchedule{Kind: ScheduleDaily, Time: "02:30"}, ny,
			utc("2026-03-08 12:00:00"), utc("2026-03-08 07:30:00"), utc("2026-03-09 06:30:00")},
		{"daily nonexistent time ahead", &CommandSchedule{Kind: ScheduleDaily, Time: "02:30"}, ny,
			utc("2026-03-08 05:00:00"), utc("2026-03-07 07:30:00"), utc("2026-03-08 07:30:00")},
		// 2026-11-01: 02:00 EDT goes back to 01:00 EST. 01:30 occurs twice
		// (05:30Z and 06:30Z); only the first is an occurrence.
		{"daily twice time", &CommandSchedule{Kind: ScheduleDaily, Time: "01:30"}, ny,
			utc("2026-11-01 06:45:00"), utc("2026-11-01 05:30:00"), utc("2026-11-02 06:30:00")},
		{"daily twice time after first", &CommandSchedule{Kind: ScheduleDaily, Time: "01:30"}, ny,
			utc("2026-11-01 05:30:00"), utc("2026-11-01 05:30:00"), utc("2026-11-02 06:30:00")},
		{"weekly across the change", &CommandSchedule{Kind: ScheduleWeekly, Weekdays: []string{"sun"}, Time: "12:00"}, ny,
			utc("2026-11-01 18:00:00"), utc("2026-11-01 17:00:00"), utc("2026-11-08 17:00:00")},
		// Clock-based kinds: the repeated hour has its occurrences twice, the
		// skipped hour none.
		{"hourly in the repeated hour", hourly30, ny, utc("2026-11-01 06:45:00"), utc("2026-11-01 06:30:00"), utc("2026-11-01 07:30:00")},
		{"hourly first pass", hourly30, ny, utc("2026-11-01 05:30:00"), utc("2026-11-01 05:30:00"), utc("2026-11-01 06:30:00")},
		{"every hour over the jump", &CommandSchedule{Kind: ScheduleEveryHours, Every: 1}, ny,
			utc("2026-03-08 06:30:00"), utc("2026-03-08 06:00:00"), utc("2026-03-08 07:00:00")},
		{"every 2 hours over the jump", &CommandSchedule{Kind: ScheduleEveryHours, Every: 2}, ny,
			utc("2026-03-08 06:30:00"), utc("2026-03-08 05:00:00"), utc("2026-03-08 08:00:00")},
		{"every 30 minutes in the repeated hour", &CommandSchedule{Kind: ScheduleEveryMinutes, Every: 30}, ny,
			utc("2026-11-01 06:10:00"), utc("2026-11-01 06:00:00"), utc("2026-11-01 06:30:00")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.s.LatestDue(tc.at, tc.loc)
			if ok != !tc.latest.IsZero() || (ok && !got.Equal(tc.latest)) {
				t.Errorf("latest: %v %v, want %v", got.UTC(), ok, tc.latest.UTC())
			}
			got, ok = tc.s.NextDue(tc.at, tc.loc)
			if ok != !tc.next.IsZero() || (ok && !got.Equal(tc.next)) {
				t.Errorf("next: %v %v, want %v", got.UTC(), ok, tc.next.UTC())
			}
		})
	}
}

// TestSchedulePlan: the entry of a scheduled set by kind: whether a passed
// occurrence that was not executed runs at once (once: always; daily: only
// today's; weekly: the latest within the last 7 days; never for hourly_at
// and the interval kinds), else the next occurrence; occurrences before the
// agent first saw the schedule (armed) are not run, except for once.
func TestSchedulePlan(t *testing.T) {
	tokyo := mustLoc(t, "Asia/Tokyo")
	ny := mustLoc(t, "America/New_York")
	// 2026-11-12 is a Thursday.
	at := func(day, hh, mm int) time.Time { return time.Date(2026, 11, day, hh, mm, 0, 0, tokyo) }
	utc := func(s string) time.Time {
		v, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	none := time.Unix(0, 0)
	once := &CommandSchedule{Kind: ScheduleOnce, At: FormatScheduleAt(at(12, 12, 0))}
	daily := &CommandSchedule{Kind: ScheduleDaily, Time: "09:00"}
	weekly := &CommandSchedule{Kind: ScheduleWeekly, Weekdays: []string{"mon", "fri"}, Time: "03:00"}
	monday := &CommandSchedule{Kind: ScheduleWeekly, Weekdays: []string{"mon"}, Time: "03:00"}
	hourly := &CommandSchedule{Kind: ScheduleHourlyAt, Minute: intPtr(30)}
	every15m := &CommandSchedule{Kind: ScheduleEveryMinutes, Every: 15}
	every6h := &CommandSchedule{Kind: ScheduleEveryHours, Every: 6}
	tests := []struct {
		name                string
		s                   *CommandSchedule
		loc                 *time.Location
		now, armed, lastDue time.Time
		due                 time.Time // zero: no entry
		atOnce              bool
	}{
		{"once ahead", once, tokyo, at(12, 11, 59), at(10, 0, 0), none, at(12, 12, 0), false},
		{"once missed while stopped", once, tokyo, at(12, 23, 0), at(10, 0, 0), none, at(12, 12, 0), true},
		{"once missed days ago", once, tokyo, at(20, 8, 0), at(10, 0, 0), none, at(12, 12, 0), true},
		{"once seen after its time", once, tokyo, at(12, 14, 0), at(12, 13, 0), none, at(12, 12, 0), true},
		{"once executed", once, tokyo, at(12, 13, 0), at(10, 0, 0), at(12, 12, 0), time.Time{}, false},
		{"daily ahead today", daily, tokyo, at(12, 8, 0), at(1, 0, 0), at(11, 9, 0), at(12, 9, 0), false},
		{"daily passed today, not executed", daily, tokyo, at(12, 10, 0), at(1, 0, 0), at(11, 9, 0), at(12, 9, 0), true},
		{"daily passed today, executed", daily, tokyo, at(12, 10, 0), at(1, 0, 0), at(12, 9, 0), at(13, 9, 0), false},
		{"daily earlier days missed, today passed", daily, tokyo, at(12, 10, 0), at(1, 0, 0), at(9, 9, 0), at(12, 9, 0), true},
		{"daily earlier days missed, today ahead", daily, tokyo, at(12, 8, 0), at(1, 0, 0), at(9, 9, 0), at(12, 9, 0), false},
		{"daily today before armed", daily, tokyo, at(12, 10, 0), at(12, 9, 30), none, at(13, 9, 0), false},
		{"weekly ahead", weekly, tokyo, at(12, 10, 0), at(1, 0, 0), at(9, 3, 0), at(13, 3, 0), false},
		{"weekly within 7 days, not executed", weekly, tokyo, at(14, 10, 0), at(1, 0, 0), at(9, 3, 0), at(13, 3, 0), true},
		{"weekly within 7 days, executed", weekly, tokyo, at(14, 10, 0), at(1, 0, 0), at(13, 3, 0), at(16, 3, 0), false},
		{"weekly latest of several within 7 days", monday, tokyo, at(16, 2, 0), at(1, 0, 0), at(2, 3, 0), at(9, 3, 0), true},
		{"weekly older than 7 days skipped", monday, tokyo, at(16, 4, 0), at(1, 0, 0), at(2, 3, 0), at(16, 3, 0), true},
		{"weekly before armed", weekly, tokyo, at(14, 10, 0), at(14, 9, 0), none, at(16, 3, 0), false},
		{"hourly_at passed not run", hourly, tokyo, at(12, 10, 45), at(1, 0, 0), at(12, 8, 30), at(12, 11, 30), false},
		{"hourly_at ahead", hourly, tokyo, at(12, 10, 15), at(1, 0, 0), at(12, 9, 30), at(12, 10, 30), false},
		{"every 15 minutes passed not run", every15m, tokyo, at(12, 10, 44), at(1, 0, 0), at(12, 9, 0), at(12, 10, 45), false},
		{"every 6 hours passed not run", every6h, tokyo, at(12, 13, 59), at(1, 0, 0), at(11, 18, 0), at(12, 18, 0), false},
		// New York, 2026-03-08: 02:30 does not exist and is 03:30 EDT
		// (07:30Z); it has passed at 08:00Z and was not executed.
		{"daily over the jump", &CommandSchedule{Kind: ScheduleDaily, Time: "02:30"}, ny,
			utc("2026-03-08 08:00"), utc("2026-03-01 00:00"), utc("2026-03-07 07:30"), utc("2026-03-08 07:30"), true},
		// 2026-11-01: 01:30 occurs twice (05:30Z and 06:30Z); the first was
		// executed, the second is not an occurrence.
		{"daily twice time", &CommandSchedule{Kind: ScheduleDaily, Time: "01:30"}, ny,
			utc("2026-11-01 06:45"), utc("2026-10-01 00:00"), utc("2026-11-01 05:30"), utc("2026-11-02 06:30"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			due, startAt, ok := tc.s.Plan(tc.now, tc.armed, tc.lastDue, tc.loc)
			if ok != !tc.due.IsZero() || (ok && !due.Equal(tc.due)) {
				t.Fatalf("due %v %v, want %v", due, ok, tc.due)
			}
			if ok && startAt.IsZero() != tc.atOnce {
				t.Fatalf("may start at %v, want at once %v", startAt, tc.atOnce)
			}
			if ok && !tc.atOnce && !startAt.Equal(due) {
				t.Fatalf("may start at %v, want at the occurrence %v", startAt, due)
			}
		})
	}
}

func TestWallTime(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	tests := []struct {
		y      int
		m      time.Month
		d      int
		hh, mm int
		want   string
	}{
		{2026, 6, 1, 12, 0, "2026-06-01T16:00:00Z"},
		{2026, 3, 8, 2, 30, "2026-03-08T07:30:00Z"},  // does not exist: 03:30 EDT
		{2026, 11, 1, 1, 30, "2026-11-01T05:30:00Z"}, // twice: the first (EDT)
		{2026, 11, 1, 2, 0, "2026-11-01T07:00:00Z"},
		{2026, 12, 32, 0, 0, "2027-01-01T05:00:00Z"}, // normalized date
	}
	for _, tc := range tests {
		if got := WallTime(tc.y, tc.m, tc.d, tc.hh, tc.mm, ny).UTC().Format(time.RFC3339); got != tc.want {
			t.Errorf("%d-%d-%d %d:%d: %s, want %s", tc.y, tc.m, tc.d, tc.hh, tc.mm, got, tc.want)
		}
	}
}

func TestScheduleSummary(t *testing.T) {
	tokyo := mustLoc(t, "Asia/Tokyo")
	tests := []struct {
		s    *CommandSchedule
		zone string
		want string
	}{
		{nil, "", "manual"},
		{&CommandSchedule{Kind: ScheduleOnce, At: "2026-11-30T18:00:00Z"}, "", "once at 2026-12-01 03:00 JST"},
		{&CommandSchedule{Kind: ScheduleWeekly, Weekdays: []string{"mon", "fri"}, Time: "03:00"}, "JST, UTC+09:00",
			"weekly on Mon, Fri at 03:00 (host time JST, UTC+09:00)"},
		{&CommandSchedule{Kind: ScheduleDaily, Time: "23:15"}, "", "daily at 23:15 (host time)"},
		{&CommandSchedule{Kind: ScheduleEveryHours, Every: 1}, "", "every hour from 00:00 (host time)"},
		{&CommandSchedule{Kind: ScheduleEveryHours, Every: 6}, "", "every 6 hours from 00:00 (host time)"},
		{&CommandSchedule{Kind: ScheduleEveryMinutes, Every: 1}, "", "every minute (host time)"},
		{&CommandSchedule{Kind: ScheduleEveryMinutes, Every: 15}, "", "every 15 minutes from minute 0 (host time)"},
		{&CommandSchedule{Kind: ScheduleHourlyAt, Minute: intPtr(5)}, "", "at minute 5 of every hour (host time)"},
	}
	for _, tc := range tests {
		if got := ScheduleSummary(tc.s, tokyo, tc.zone); got != tc.want {
			t.Errorf("%q, want %q", got, tc.want)
		}
	}
	for sec, want := range map[int]string{32400: "UTC+09:00", -16200: "UTC-04:30", 0: "UTC+00:00", 20700: "UTC+05:45"} {
		if got := FormatUTCOffset(sec); got != want {
			t.Errorf("%d: %s", sec, got)
		}
	}
}
