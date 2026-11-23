package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	minYear = 1970
	maxYear = 2099

	timeZonePrefix = "CRON_TZ="
)

var (
	monthNames = map[string]int{
		"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
		"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
	}
	// Day of week counts from Sunday = 1, as in Quartz.
	dayNames = map[string]int{"SUN": 1, "MON": 2, "TUE": 3, "WED": 4, "THU": 5, "FRI": 6, "SAT": 7}
)

// values is a set of the values of one field: values[v] says whether v is in it.
type values []bool

// bounds describes the values a field accepts.
type bounds struct {
	name     string
	min, max int
	names    map[string]int
}

var (
	secondBounds     = bounds{name: "second", min: 0, max: 59}
	minuteBounds     = bounds{name: "minute", min: 0, max: 59}
	hourBounds       = bounds{name: "hour", min: 0, max: 23}
	dayOfMonthBounds = bounds{name: "day of month", min: 1, max: 31}
	monthBounds      = bounds{name: "month", min: 1, max: 12, names: monthNames}
	dayOfWeekBounds  = bounds{name: "day of week", min: 1, max: 7, names: dayNames}
	yearBounds       = bounds{name: "year", min: minYear, max: maxYear}
)

// Parse parses a startTime:
//
//	[CRON_TZ=<IANA time zone> ]<second> <minute> <hour> <day of month> <month> <day of week> <year>
//
// It reads the time zone database (ZONEINFO, the system's, or the embedded one).
func Parse(startTime string) (*Schedule, error) {
	fields := strings.Fields(startTime)
	location := time.UTC
	if len(fields) > 0 && strings.HasPrefix(fields[0], timeZonePrefix) {
		var err error
		if location, err = loadLocation(strings.TrimPrefix(fields[0], timeZonePrefix)); err != nil {
			return nil, err
		}
		fields = fields[1:]
	}
	if len(fields) != 7 {
		return nil, fmt.Errorf("startTime has %d fields, want 7 fields "+
			"(second, minute, hour, day of month, month, day of week, year) after the optional %s<time zone>",
			len(fields), timeZonePrefix)
	}

	schedule := &Schedule{location: location}
	var err error
	if schedule.second, err = secondBounds.value(fields[0]); err != nil {
		return nil, fmt.Errorf("second %q: %w (the second field is a single value)", fields[0], err)
	}
	for _, list := range []struct {
		target *values
		bounds bounds
		field  string
	}{
		{&schedule.minutes, minuteBounds, fields[1]},
		{&schedule.hours, hourBounds, fields[2]},
		{&schedule.months, monthBounds, fields[4]},
		{&schedule.years, yearBounds, fields[6]},
	} {
		*list.target = make(values, list.bounds.max+1)
		if err := list.bounds.addList(*list.target, list.field); err != nil {
			return nil, err
		}
	}
	if (fields[3] == "?") == (fields[5] == "?") {
		return nil, fmt.Errorf("exactly one of day of month (%q) and day of week (%q) must be ?", fields[3], fields[5])
	}
	if schedule.daysOfMonth, err = parseDaysOfMonth(fields[3]); err != nil {
		return nil, err
	}
	if schedule.daysOfWeek, err = parseDaysOfWeek(fields[5]); err != nil {
		return nil, err
	}
	return schedule, nil
}

// loadLocation refuses the names that time.LoadLocation accepts but that are
// not IANA time zones: "" (UTC) and "Local" (the time zone of the container).
func loadLocation(name string) (*time.Location, error) {
	if name == "" {
		return nil, fmt.Errorf("%s names no time zone", timeZonePrefix)
	}
	if name == "Local" {
		return nil, fmt.Errorf("time zone Local is not allowed: name an IANA time zone, or UTC")
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q: %w", name, err)
	}
	return location, nil
}

// value parses one value of the field: a number, or a name.
func (b bounds) value(text string) (int, error) {
	if value, ok := b.names[strings.ToUpper(text)]; ok {
		return value, nil
	}
	value, err := strconv.Atoi(text)
	if err != nil || text[0] == '+' || text[0] == '-' {
		return 0, fmt.Errorf("%q is not a value", text)
	}
	if value < b.min || value > b.max {
		return 0, fmt.Errorf("%d is not in %d-%d", value, b.min, b.max)
	}
	return value, nil
}

// addList adds the values of a comma-separated list of items to set.
func (b bounds) addList(set values, list string) error {
	for _, item := range strings.Split(list, ",") {
		if err := b.addItem(set, item); err != nil {
			return fmt.Errorf("%s %q: %w", b.name, list, err)
		}
	}
	return nil
}

// addItem adds the values of one item: *, a, a-b, each with an optional /step.
// As in Quartz, a/step goes up to the end of the field, and a range whose start
// is greater than its end overflows: 22-2 is 22, 23, 0, 1, 2.
func (b bounds) addItem(set values, item string) error {
	span, stepText, hasStep := strings.Cut(item, "/")
	step := 1
	if hasStep {
		var err error
		if step, err = strconv.Atoi(stepText); err != nil || step < 1 {
			return fmt.Errorf("increment %q is not a positive number", stepText)
		}
	}

	first, last := b.min, b.max
	if span != "*" {
		firstText, lastText, isRange := strings.Cut(span, "-")
		var err error
		if first, err = b.value(firstText); err != nil {
			return err
		}
		switch {
		case isRange:
			if last, err = b.value(lastText); err != nil {
				return err
			}
		case !hasStep:
			last = first
		}
	}

	size := b.max - b.min + 1
	count := last - first
	if count < 0 {
		count += size
	}
	for offset := 0; offset <= count; offset += step {
		set[b.min+(first-b.min+offset)%size] = true
	}
	return nil
}

// daysOfMonth is the day-of-month field.
type daysOfMonth struct {
	any bool // ?
	// days are the plain days.
	days values
	// fromLast are the days counted from the last day of the month: 0 for L, 3 for L-3.
	fromLast []int
	// nearestWeekday are the days of nW: the weekday nearest to day n.
	nearestWeekday []int
	// lastWeekday is LW.
	lastWeekday bool
}

func parseDaysOfMonth(field string) (daysOfMonth, error) {
	b := dayOfMonthBounds
	result := daysOfMonth{days: make(values, b.max+1)}
	if field == "?" {
		result.any = true
		return result, nil
	}
	for _, item := range strings.Split(field, ",") {
		upper := strings.ToUpper(item)
		var err error
		switch {
		case upper == "LW":
			result.lastWeekday = true
		case upper == "L":
			result.fromLast = append(result.fromLast, 0)
		case strings.HasPrefix(upper, "L-"):
			var offset int
			if offset, err = strconv.Atoi(upper[2:]); err != nil || offset < 1 || offset > 30 {
				err = fmt.Errorf("%q is not L-1 to L-30", item)
			}
			result.fromLast = append(result.fromLast, offset)
		case strings.HasSuffix(upper, "W"):
			var day int
			day, err = b.value(upper[:len(upper)-1])
			result.nearestWeekday = append(result.nearestWeekday, day)
		default:
			err = b.addItem(result.days, item)
		}
		if err != nil {
			return daysOfMonth{}, fmt.Errorf("%s %q: %w", b.name, field, err)
		}
	}
	return result, nil
}

// daysOfWeek is the day-of-week field. Days count from Sunday = 1.
type daysOfWeek struct {
	any bool // ?
	// days are the plain days.
	days values
	// last are the days of nL: the last such day of the month.
	last []int
	// nth are the days of n#k: the k-th such day of the month.
	nth []nthDay
}

type nthDay struct{ day, nth int }

func parseDaysOfWeek(field string) (daysOfWeek, error) {
	b := dayOfWeekBounds
	result := daysOfWeek{days: make(values, b.max+1)}
	if field == "?" {
		result.any = true
		return result, nil
	}
	for _, item := range strings.Split(field, ",") {
		upper := strings.ToUpper(item)
		dayText, nthText, isNth := strings.Cut(upper, "#")
		var err error
		switch {
		case upper == "L":
			result.days[7] = true // Saturday
		case isNth:
			var nth nthDay
			if nth.day, err = b.value(dayText); err == nil {
				if nth.nth, err = strconv.Atoi(nthText); err != nil || nth.nth < 1 || nth.nth > 5 {
					err = fmt.Errorf("%q is not #1 to #5", item)
				}
			}
			result.nth = append(result.nth, nth)
		case strings.HasSuffix(upper, "L"):
			var day int
			day, err = b.value(upper[:len(upper)-1])
			result.last = append(result.last, day)
		default:
			err = b.addItem(result.days, item)
		}
		if err != nil {
			return daysOfWeek{}, fmt.Errorf("%s %q: %w", b.name, field, err)
		}
	}
	return result, nil
}
