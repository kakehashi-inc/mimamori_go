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

func TestScheduleDueToRun(t *testing.T) {
	tokyo := mustLoc(t, "Asia/Tokyo")
	at := func(hh, mm int) time.Time { return time.Date(2026, 11, 12, hh, mm, 0, 0, tokyo) }
	every15m := &CommandSchedule{Kind: ScheduleEveryMinutes, Every: 15}
	once := &CommandSchedule{Kind: ScheduleOnce, At: FormatScheduleAt(at(12, 0))}
	tests := []struct {
		name                string
		s                   *CommandSchedule
		now, armed, lastRun time.Time
		want                time.Time // zero: nothing to run
	}{
		{"missed occurrences run once (the latest)", every15m, at(11, 7), at(10, 0), at(10, 15), at(11, 0)},
		{"already ran", every15m, at(11, 10), at(10, 0), at(11, 0), time.Time{}},
		{"armed after the latest occurrence", every15m, at(10, 10), at(10, 5), time.Time{}, time.Time{}},
		{"first occurrence after arming", every15m, at(10, 15), at(10, 5), time.Time{}, at(10, 15)},
		{"armed exactly at an occurrence", every15m, at(10, 0), at(10, 0), time.Time{}, time.Time{}},
		{"once due", once, at(12, 30), at(9, 0), time.Time{}, at(12, 0)},
		{"once not yet due", once, at(11, 59), at(9, 0), time.Time{}, time.Time{}},
		{"once already run", once, at(13, 0), at(9, 0), at(12, 0), time.Time{}},
		{"once noticed late", once, at(23, 0), at(9, 0), time.Time{}, at(12, 0)},
	}
	for _, tc := range tests {
		got, ok := tc.s.DueToRun(tc.now, tc.armed, tc.lastRun, tokyo)
		if ok != !tc.want.IsZero() || (ok && !got.Equal(tc.want)) {
			t.Errorf("%s: %v %v, want %v", tc.name, got, ok, tc.want)
		}
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
