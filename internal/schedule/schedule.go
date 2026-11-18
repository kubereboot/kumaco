// Package schedule parses the startTime of a MaintenanceWindow and evaluates
// its occurrences. See README.md, "The kumaco.kured.dev MaintenanceWindow CRD"
// and "Time zones".
package schedule

import (
	"time"
)

// MaxChain is how far back a chain of occurrences is followed. It is also the
// longest duration of a window.
const MaxChain = 31 * 24 * time.Hour

// maxOffsetChange is more than two UTC offsets of a time zone can differ by.
const maxOffsetChange = 26 * time.Hour

// Schedule is a parsed startTime: the occurrences of a window.
type Schedule struct {
	location    *time.Location
	second      int
	minutes     values
	hours       values
	daysOfMonth daysOfMonth
	months      values
	daysOfWeek  daysOfWeek
	years       values
}

// Next returns the first occurrence strictly after the given time.
//
// It goes over the periods during which the time zone keeps one UTC offset.
// Within such a period, local times and instants are in the same order, so the
// first matching local time is the first occurrence. This gives the README's
// DST rules without special cases: a local time that does not exist is in no
// period, and a local time that repeats is in two of them.
func (s *Schedule) Next(after time.Time) (time.Time, bool) {
	from := after.Truncate(time.Second).Add(time.Second).In(s.location)
	for from.Year() <= maxYear {
		_, periodEnd := from.ZoneBounds()
		_, offsetSeconds := from.Zone()
		offset := time.Duration(offsetSeconds) * time.Second

		// A local time is represented by the UTC time with the same fields.
		local, ok := s.nextLocal(from.UTC().Add(offset))
		if !ok {
			// No local time matches from here on. A later instant can only
			// have an earlier local time shortly after a change of offset.
			if periodEnd.IsZero() || periodEnd.Sub(from) > maxOffsetChange {
				return time.Time{}, false
			}
			from = periodEnd
			continue
		}
		occurrence := local.Add(-offset)
		if periodEnd.IsZero() || occurrence.Before(periodEnd) {
			return occurrence.In(s.location), true
		}
		// No occurrence in this period. In the later ones, an instant whose
		// local time is at least `local` cannot be much before `occurrence`.
		from = periodEnd
		if jump := occurrence.Add(-maxOffsetChange); jump.After(from) {
			from = jump.In(s.location)
		}
	}
	return time.Time{}, false
}

// nextLocal returns the first local time matching the fields that is not before
// from. Local times are UTC times with the same fields: no time zone here.
func (s *Schedule) nextLocal(from time.Time) (time.Time, bool) {
	t := from
	for t.Year() <= maxYear {
		year, month, day := t.Date()
		switch {
		case year < minYear:
			t = time.Date(minYear, time.January, 1, 0, 0, 0, 0, time.UTC)
		case !s.years[year]:
			t = time.Date(year+1, time.January, 1, 0, 0, 0, 0, time.UTC)
		case !s.months[month]:
			t = time.Date(year, month+1, 1, 0, 0, 0, 0, time.UTC)
		case !s.dayMatches(year, month, day):
			t = time.Date(year, month, day+1, 0, 0, 0, 0, time.UTC)
		case !s.hours[t.Hour()]:
			t = t.Truncate(time.Hour).Add(time.Hour)
		case !s.minutes[t.Minute()] || t.Second() > s.second:
			t = t.Truncate(time.Minute).Add(time.Minute)
		default:
			return time.Date(year, month, day, t.Hour(), t.Minute(), s.second, 0, time.UTC), true
		}
	}
	return time.Time{}, false
}

func (s *Schedule) dayMatches(year int, month time.Month, day int) bool {
	date := monthDay{
		day:     day,
		last:    time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day(),
		weekday: int(time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Weekday()) + 1,
	}
	if s.daysOfMonth.any {
		return s.daysOfWeek.match(date)
	}
	return s.daysOfMonth.match(date)
}

// monthDay is a day within its month.
type monthDay struct {
	day     int
	last    int // the last day of the month
	weekday int // of day, from Sunday = 1
}

// weekdayOf returns the day of week of another day of the same month.
func (d monthDay) weekdayOf(day int) int {
	return ((d.weekday-1+day-d.day)%7+7)%7 + 1
}

// nearestWeekday returns the weekday (Monday to Friday) nearest to the given
// day, within the month.
func (d monthDay) nearestWeekday(day int) int {
	const sunday, saturday = 1, 7
	switch weekday := d.weekdayOf(day); {
	case weekday == saturday && day == 1:
		return 3
	case weekday == saturday:
		return day - 1
	case weekday == sunday && day == d.last:
		return day - 2
	case weekday == sunday:
		return day + 1
	}
	return day
}

func (f daysOfMonth) match(date monthDay) bool {
	if f.days[date.day] {
		return true
	}
	for _, offset := range f.fromLast {
		if date.day == date.last-offset {
			return true
		}
	}
	// A day that the month does not have has no nearest weekday in it.
	for _, day := range f.nearestWeekday {
		if day <= date.last && date.day == date.nearestWeekday(day) {
			return true
		}
	}
	return f.lastWeekday && date.day == date.nearestWeekday(date.last)
}

func (f daysOfWeek) match(date monthDay) bool {
	if f.days[date.weekday] {
		return true
	}
	for _, day := range f.last {
		if date.weekday == day && date.day+7 > date.last {
			return true
		}
	}
	for _, nth := range f.nth {
		if date.weekday == nth.day && (date.day-1)/7+1 == nth.nth {
			return true
		}
	}
	return false
}

// Evaluation is the situation of a window's schedule at one point in time.
type Evaluation struct {
	// Active says whether the time falls in [start, start+duration) for an occurrence.
	Active bool
	// OccurrenceStart is, when Active, the start of the current occurrence: the
	// earliest occurrence of the chain of occurrences that overlap or follow
	// each other without a gap, followed back at most MaxChain.
	OccurrenceStart time.Time
	// NextBoundary is when the schedule must be evaluated again: the end of an
	// active occurrence when Active, the next start otherwise. It is zero when
	// no occurrence is active and none will start.
	NextBoundary time.Time
}

// Evaluate says whether an occurrence of the given duration is active at now.
// Only the occurrences starting in (now-duration, now] can be active.
func (s *Schedule) Evaluate(duration time.Duration, now time.Time) Evaluation {
	start, found := s.Next(now.Add(-duration))
	if !found || start.After(now) {
		next, _ := s.Next(now)
		return Evaluation{NextBoundary: next}
	}
	evaluation := Evaluation{Active: true, NextBoundary: start.Add(duration)}

	// Follow the chain back: the previous occurrences of the chain started at
	// most one duration before the earliest one known. Two steps go back by
	// more than one duration, which bounds the search.
	limit := now.Add(-MaxChain)
	for {
		floor := start.Add(-duration)
		if floor.Before(limit) {
			floor = limit
		}
		earlier, _ := s.Next(floor.Add(-time.Nanosecond))
		if !earlier.Before(start) {
			break
		}
		start = earlier
	}
	evaluation.OccurrenceStart = start
	return evaluation
}
