package modules

import (
	"fmt"
	"strings"
	"time"
)

// Schedule arithmetic of command sets. Every function takes the time zone
// location explicitly: recurring kinds are interpreted in the local time of
// the monitored host, a once schedule is an absolute time.
//
// Daylight-saving changes:
//   - weekly and daily name a wall-clock time per day. When that time does
//     not exist on a day (the clock jumps over it), the occurrence is the
//     same length of time after the last moment before the jump (02:30
//     becomes 03:30 when 02:00 jumps to 03:00). When it occurs twice (the
//     clock goes back), only the first one is an occurrence.
//   - every_hours, every_minutes and hourly_at follow the clock: an instant
//     is an occurrence when the local clock shows a matching time. Hours the
//     clock skips have no occurrence; an hour the clock repeats has its
//     occurrences twice, so every real hour keeps its runs.

// clockSearchLimit bounds the minute-by-minute search of the clock-based
// kinds (the largest gap is 12 hours of every_hours plus a shift, or a day
// when the zone skips the only matching hour).
const clockSearchLimit = 27 * 60

// LatestDue returns the latest due time not after t. ok is false when there
// is none (a once time after t).
func (s *CommandSchedule) LatestDue(t time.Time, loc *time.Location) (time.Time, bool) {
	switch s.Kind {
	case ScheduleOnce:
		at := s.AtTime()
		if at.IsZero() || at.After(t) {
			return time.Time{}, false
		}
		return at, true
	case ScheduleWeekly, ScheduleDaily:
		lt := t.In(loc)
		for i := 0; i <= 8; i++ {
			c, ok := s.dayOccurrence(lt.Year(), lt.Month(), lt.Day()-i, loc)
			if ok && !c.After(t) {
				return c, true
			}
		}
		return time.Time{}, false
	case ScheduleEveryHours, ScheduleEveryMinutes, ScheduleHourlyAt:
		c := minuteFloor(t, loc)
		for i := 0; i < clockSearchLimit; i++ {
			if s.clockMatches(c.In(loc)) {
				return c, true
			}
			c = minuteFloor(c.Add(-time.Minute), loc)
		}
	}
	return time.Time{}, false
}

// NextDue returns the first due time after t. ok is false when there is
// none (a once time not after t).
func (s *CommandSchedule) NextDue(t time.Time, loc *time.Location) (time.Time, bool) {
	switch s.Kind {
	case ScheduleOnce:
		at := s.AtTime()
		if at.IsZero() || !at.After(t) {
			return time.Time{}, false
		}
		return at, true
	case ScheduleWeekly, ScheduleDaily:
		lt := t.In(loc)
		for i := -1; i <= 8; i++ {
			c, ok := s.dayOccurrence(lt.Year(), lt.Month(), lt.Day()+i, loc)
			if ok && c.After(t) {
				return c, true
			}
		}
		return time.Time{}, false
	case ScheduleEveryHours, ScheduleEveryMinutes, ScheduleHourlyAt:
		c := minuteFloor(t, loc)
		for i := 0; i < clockSearchLimit; i++ {
			c = minuteFloor(c.Add(time.Minute), loc)
			if c.After(t) && s.clockMatches(c.In(loc)) {
				return c, true
			}
		}
	}
	return time.Time{}, false
}

// DueToRun returns the occurrence a schedule should run at now: its latest
// due time not after now, when that is later than both armedAt (when the
// agent first saw this schedule definition) and lastRun (the latest
// occurrence already run). However many occurrences were missed, only the
// latest one is returned.
func (s *CommandSchedule) DueToRun(now, armedAt, lastRun time.Time, loc *time.Location) (time.Time, bool) {
	due, ok := s.LatestDue(now, loc)
	if !ok || !due.After(armedAt) || !due.After(lastRun) {
		return time.Time{}, false
	}
	return due, true
}

// dayOccurrence returns the occurrence of a weekly or daily schedule on the
// local date (y, m, d); d may be out of range and is normalized. ok is false
// when a weekly schedule does not include that weekday.
func (s *CommandSchedule) dayOccurrence(y int, m time.Month, d int, loc *time.Location) (time.Time, bool) {
	date := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	if s.Kind == ScheduleWeekly && s.WeekdayMask()&(1<<int(date.Weekday())) == 0 {
		return time.Time{}, false
	}
	tod := s.TimeOfDay()
	return WallTime(date.Year(), date.Month(), date.Day(), tod/60, tod%60, loc), true
}

// WallTime returns the instant at which the clock of loc shows the given
// local date and time. When that time occurs twice, the earlier instant is
// returned; when it does not exist, the instant the same length of time
// after the last moment before the jump.
func WallTime(y int, m time.Month, d, hh, mm int, loc *time.Location) time.Time {
	wall := time.Date(y, m, d, hh, mm, 0, 0, time.UTC)
	var best time.Time
	for _, probe := range []time.Duration{-36 * time.Hour, 0, 36 * time.Hour} {
		_, off := wall.Add(probe).In(loc).Zone()
		c := wall.Add(-time.Duration(off) * time.Second)
		lc := c.In(loc)
		if lc.Year() != y || lc.Month() != m || lc.Day() != d || lc.Hour() != hh || lc.Minute() != mm || lc.Second() != 0 {
			continue
		}
		if best.IsZero() || c.Before(best) {
			best = c
		}
	}
	if !best.IsZero() {
		return best
	}
	// The time does not exist: use the offset in effect before the jump.
	_, off := wall.Add(-36 * time.Hour).In(loc).Zone()
	return wall.Add(-time.Duration(off) * time.Second)
}

// minuteFloor returns the latest instant not after t at which the clock of
// loc shows a whole minute.
func minuteFloor(t time.Time, loc *time.Location) time.Time {
	lt := t.In(loc)
	return t.Add(-time.Duration(lt.Second())*time.Second - time.Duration(lt.Nanosecond()))
}

// clockMatches reports whether a local time (a whole minute) is an
// occurrence of a clock-based kind.
func (s *CommandSchedule) clockMatches(lt time.Time) bool {
	if lt.Second() != 0 || lt.Nanosecond() != 0 {
		return false
	}
	switch s.Kind {
	case ScheduleEveryHours:
		return s.Every > 0 && lt.Minute() == 0 && lt.Hour()%s.Every == 0
	case ScheduleEveryMinutes:
		return s.Every > 0 && lt.Minute()%s.Every == 0
	case ScheduleHourlyAt:
		return lt.Minute() == s.MinuteValue()
	}
	return false
}

// FormatUTCOffset renders an offset in seconds as "UTC+09:00".
func FormatUTCOffset(sec int) string {
	sign := "+"
	if sec < 0 {
		sign, sec = "-", -sec
	}
	return fmt.Sprintf("UTC%s%02d:%02d", sign, sec/3600, sec%3600/60)
}

// ScheduleSummary returns a one-line English summary of a schedule for the
// CLI ("manual" for none). A once time is shown in loc; recurring kinds name
// the host's time (hostZone, such as "JST, UTC+09:00", when known).
func ScheduleSummary(s *CommandSchedule, loc *time.Location, hostZone string) string {
	if s == nil {
		return "manual"
	}
	host := "host time"
	if hostZone != "" {
		host = "host time " + hostZone
	}
	switch s.Kind {
	case ScheduleOnce:
		at := s.AtTime()
		if at.IsZero() {
			return "once"
		}
		return "once at " + at.In(loc).Format("2006-01-02 15:04 MST")
	case ScheduleWeekly:
		days := make([]string, 0, len(s.Weekdays))
		for _, w := range s.Weekdays {
			if w != "" {
				days = append(days, strings.ToUpper(w[:1])+w[1:])
			}
		}
		return fmt.Sprintf("weekly on %s at %s (%s)", strings.Join(days, ", "), s.Time, host)
	case ScheduleDaily:
		return fmt.Sprintf("daily at %s (%s)", s.Time, host)
	case ScheduleEveryHours:
		if s.Every == 1 {
			return fmt.Sprintf("every hour from 00:00 (%s)", host)
		}
		return fmt.Sprintf("every %d hours from 00:00 (%s)", s.Every, host)
	case ScheduleEveryMinutes:
		if s.Every == 1 {
			return fmt.Sprintf("every minute (%s)", host)
		}
		return fmt.Sprintf("every %d minutes from minute 0 (%s)", s.Every, host)
	case ScheduleHourlyAt:
		return fmt.Sprintf("at minute %d of every hour (%s)", s.MinuteValue(), host)
	}
	return s.Kind
}
