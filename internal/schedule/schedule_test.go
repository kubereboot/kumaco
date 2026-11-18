package schedule

import (
	"archive/zip"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, startTime string) *Schedule {
	t.Helper()
	schedule, err := Parse(startTime)
	if err != nil {
		t.Fatalf("Parse(%q): %v", startTime, err)
	}
	return schedule
}

func at(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// README: #the-kumacokureddev-maintenancewindow-crd, #time-zones
func TestParseRejectsWhatTheGrammarDoesNotAllow(t *testing.T) {
	for _, tt := range []struct {
		name, startTime, want string
	}{
		{"empty", "", "7 fields"},
		{"6 fields, as Unix cron with seconds", "0 0 2 * * ?", "7 fields"},
		{"8 fields", "0 0 2 * * ? * *", "7 fields"},
		{"an unknown prefix", "TZ=Europe/Brussels 0 0 2 * * ? *", "7 fields"},

		{"a list of seconds", "0,30 0 2 * * ? *", "second"},
		{"every second", "* 0 2 * * ? *", "second"},
		{"second 60", "60 0 2 * * ? *", "second"},
		{"minute 60", "0 60 2 * * ? *", "minute"},
		{"hour 24", "0 0 24 * * ? *", "hour"},
		{"day of month 0", "0 0 2 0 * ? *", "day of month"},
		{"day of month 32", "0 0 2 32 * ? *", "day of month"},
		{"month 0", "0 0 2 * 0 ? *", "month"},
		{"month 13", "0 0 2 * 13 ? *", "month"},
		{"an unknown month name", "0 0 2 * JANUARY ? *", "month"},
		{"day of week 0, the Unix Sunday", "0 0 2 ? * 0 *", "day of week"},
		{"day of week 8", "0 0 2 ? * 8 *", "day of week"},
		{"year 1969", "0 0 2 * * ? 1969", "year"},
		{"year 2100", "0 0 2 * * ? 2100", "year"},

		{"day of month and day of week both set", "0 0 2 1 * MON *", "exactly one"},
		{"day of month and day of week both ?", "0 0 2 ? * ? *", "exactly one"},
		{"? in the hour", "0 0 ? * * ? *", "hour"},
		{"? in a list", "0 0 2 1,? * ? *", "day of month"},

		{"L in the month", "0 0 2 * L ? *", "month"},
		{"W in the day of week", "0 0 2 ? * 2W *", "day of week"},
		{"# in the day of month", "0 0 2 3#2 * ? *", "day of month"},
		{"the sixth Tuesday", "0 0 2 ? * 3#6 *", "day of week"},
		{"the zeroth Tuesday", "0 0 2 ? * 3#0 *", "day of week"},
		{"L-31", "0 0 2 L-31 * ? *", "day of month"},
		{"32W", "0 0 2 32W * ? *", "day of month"},
		{"a zero increment", "0 0/0 2 * * ? *", "minute"},
		{"an increment without a number", "0 0/ 2 * * ? *", "minute"},
		{"an empty list item", "0 0,,5 2 * * ? *", "minute"},
		{"a negative value", "0 -5 2 * * ? *", "minute"},
		{"an open range", "0 5- 2 * * ? *", "minute"},

		{"the Local time zone", "CRON_TZ=Local 0 0 2 * * ? *", "Local"},
		{"an unknown time zone", "CRON_TZ=Mars/Olympus_Mons 0 0 2 * * ? *", "Mars/Olympus_Mons"},
		{"an empty time zone", "CRON_TZ= 0 0 2 * * ? *", "time zone"},
		{"a time zone outside of the database", "CRON_TZ=../../etc/passwd 0 0 2 * * ? *", "time zone"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			schedule, err := Parse(tt.startTime)
			if err == nil {
				t.Fatalf("Parse(%q) = %+v, want an error", tt.startTime, schedule)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse(%q) error = %q, want it to mention %q", tt.startTime, err, tt.want)
			}
		})
	}
}

// README: #the-kumacokureddev-maintenancewindow-crd, #time-zones
func TestNextFollowsTheGrammar(t *testing.T) {
	for _, tt := range []struct {
		name      string
		startTime string
		after     string
		// want lists the occurrences following after; "none" ends the schedule.
		want []string
	}{
		{
			name:      "README: once, at 02:00 Brussels time on 2027-01-01",
			startTime: "CRON_TZ=Europe/Brussels 0 0 2 1 1 ? 2027",
			after:     "2026-11-18T00:00:00Z",
			want:      []string{"2027-01-01T02:00:00+01:00", "none"},
		},
		{
			name:      "README: daily at 02:00 Brussels time",
			startTime: "CRON_TZ=Europe/Brussels 0 0 2 * * ? *",
			after:     "2026-11-18T12:00:00Z",
			want:      []string{"2026-11-19T02:00:00+01:00", "2026-11-20T02:00:00+01:00"},
		},
		{
			name:      "README: 01:30 New York time on weekdays",
			startTime: "CRON_TZ=America/New_York 0 30 1 ? * MON-FRI *",
			after:     "2026-11-20T12:00:00-05:00", // a Friday
			want:      []string{"2026-11-23T01:30:00-05:00", "2026-11-24T01:30:00-05:00"},
		},
		{
			name:      "README: 22:00 UTC on the second Tuesday of every month",
			startTime: "0 0 22 ? * 3#2 *",
			after:     "2026-11-01T00:00:00Z",
			want:      []string{"2026-11-10T22:00:00Z", "2026-12-08T22:00:00Z", "2027-01-12T22:00:00Z"},
		},
		{
			name:      "without CRON_TZ, the time zone is UTC",
			startTime: "0 0 2 * * ? *",
			after:     "2026-11-18T00:00:00Z",
			want:      []string{"2026-11-18T02:00:00Z"},
		},
		{
			name:      "the occurrence is strictly after the given time",
			startTime: "0 0 2 * * ? *",
			after:     "2026-11-18T02:00:00Z",
			want:      []string{"2026-11-19T02:00:00Z"},
		},
		{
			name:      "a fraction of a second before an occurrence",
			startTime: "0 0 2 * * ? *",
			after:     "2026-11-18T01:59:59.999999999Z",
			want:      []string{"2026-11-18T02:00:00Z"},
		},
		{
			name:      "the second field",
			startTime: "45 0 2 * * ? *",
			after:     "2026-11-18T02:00:50Z",
			want:      []string{"2026-11-19T02:00:45Z"},
		},
		{
			name:      "an increment: 0/15 is minutes 0, 15, 30 and 45",
			startTime: "0 0/15 * * * ? *",
			after:     "2026-11-18T10:10:00Z",
			want:      []string{"2026-11-18T10:15:00Z", "2026-11-18T10:30:00Z", "2026-11-18T10:45:00Z", "2026-11-18T11:00:00Z"},
		},
		{
			name:      "an increment on all values",
			startTime: "0 0 */8 * * ? *",
			after:     "2026-11-18T00:00:00Z",
			want:      []string{"2026-11-18T08:00:00Z", "2026-11-18T16:00:00Z", "2026-11-19T00:00:00Z"},
		},
		{
			name:      "an increment on a range",
			startTime: "0 10-30/10 2 * * ? *",
			after:     "2026-11-18T00:00:00Z",
			want:      []string{"2026-11-18T02:10:00Z", "2026-11-18T02:20:00Z", "2026-11-18T02:30:00Z", "2026-11-19T02:10:00Z"},
		},
		{
			name:      "a list",
			startTime: "0 0 2 ? * MON,WED,FRI *",
			after:     "2026-11-18T12:00:00Z", // a Wednesday
			want:      []string{"2026-11-20T02:00:00Z", "2026-11-23T02:00:00Z", "2026-11-25T02:00:00Z"},
		},
		{
			name:      "a range overflowing the end of the field, as in Quartz",
			startTime: "0 0 22-1 * * ? *",
			after:     "2026-11-18T21:00:00Z",
			want:      []string{"2026-11-18T22:00:00Z", "2026-11-18T23:00:00Z", "2026-11-19T00:00:00Z", "2026-11-19T01:00:00Z", "2026-11-19T22:00:00Z"},
		},
		{
			name:      "month names, in any case",
			startTime: "0 0 2 1 nov-Jan ? *",
			after:     "2026-11-18T00:00:00Z",
			want:      []string{"2026-12-01T02:00:00Z", "2027-01-01T02:00:00Z", "2027-11-01T02:00:00Z"},
		},
		{
			name:      "day of week 1 is Sunday",
			startTime: "0 0 2 ? * 1 *",
			after:     "2026-11-18T00:00:00Z",
			want:      []string{"2026-11-22T02:00:00Z"},
		},
		{
			name:      "day names, in any case",
			startTime: "0 0 2 ? * sun *",
			after:     "2026-11-18T00:00:00Z",
			want:      []string{"2026-11-22T02:00:00Z"},
		},
		{
			name:      "L in day of month is the last day of the month",
			startTime: "0 0 2 L * ? *",
			after:     "2027-01-31T12:00:00Z",
			want:      []string{"2027-02-28T02:00:00Z", "2027-03-31T02:00:00Z"},
		},
		{
			name:      "L in a leap year",
			startTime: "0 0 2 L 2 ? *",
			after:     "2027-03-01T00:00:00Z",
			want:      []string{"2028-02-29T02:00:00Z"},
		},
		{
			name:      "L-3 is the third-to-last day",
			startTime: "0 0 2 L-3 * ? *",
			after:     "2027-01-01T00:00:00Z",
			want:      []string{"2027-01-28T02:00:00Z", "2027-02-25T02:00:00Z"},
		},
		{
			name:      "L alone in day of week is Saturday",
			startTime: "0 0 2 ? * L *",
			after:     "2026-11-18T00:00:00Z",
			want:      []string{"2026-11-21T02:00:00Z", "2026-11-28T02:00:00Z"},
		},
		{
			name:      "6L is the last Friday of the month",
			startTime: "0 0 2 ? * 6L *",
			after:     "2026-11-01T00:00:00Z",
			want:      []string{"2026-11-27T02:00:00Z", "2026-12-25T02:00:00Z"},
		},
		{
			name:      "15W on a Saturday is the Friday before",
			startTime: "0 0 2 15W 8 ? 2026",
			after:     "2026-01-01T00:00:00Z",
			want:      []string{"2026-08-14T02:00:00Z", "none"},
		},
		{
			name:      "15W on a Sunday is the Monday after",
			startTime: "0 0 2 15W 11 ? 2026",
			after:     "2026-01-01T00:00:00Z",
			want:      []string{"2026-11-16T02:00:00Z", "none"},
		},
		{
			name:      "15W on a weekday is that day",
			startTime: "0 0 2 15W 12 ? 2026",
			after:     "2026-01-01T00:00:00Z",
			want:      []string{"2026-12-15T02:00:00Z", "none"},
		},
		{
			name:      "1W on a Saturday stays in the month",
			startTime: "0 0 2 1W 8 ? 2026",
			after:     "2026-01-01T00:00:00Z",
			want:      []string{"2026-08-03T02:00:00Z", "none"},
		},
		{
			name:      "31W on a Sunday stays in the month",
			startTime: "0 0 2 31W 5 ? 2026",
			after:     "2026-01-01T00:00:00Z",
			want:      []string{"2026-05-29T02:00:00Z", "none"},
		},
		{
			name:      "LW is the last weekday of the month",
			startTime: "0 0 2 LW * ? 2026",
			after:     "2026-05-01T00:00:00Z",
			want:      []string{"2026-05-29T02:00:00Z", "2026-06-30T02:00:00Z"},
		},
		{
			name:      "a day that does not exist in the month",
			startTime: "0 0 2 31 * ? 2027",
			after:     "2027-01-31T12:00:00Z",
			want:      []string{"2027-03-31T02:00:00Z", "2027-05-31T02:00:00Z"},
		},
		{
			name:      "a date that never exists",
			startTime: "0 0 2 30 2 ? *",
			after:     "2026-11-18T00:00:00Z",
			want:      []string{"none"},
		},
		{
			name:      "the year field",
			startTime: "0 0 2 1 1 ? 2030/30",
			after:     "2026-11-18T00:00:00Z",
			want:      []string{"2030-01-01T02:00:00Z", "2060-01-01T02:00:00Z", "2090-01-01T02:00:00Z", "none"},
		},
		{
			name:      "no occurrence after 2099",
			startTime: "0 0 2 * * ? *",
			after:     "2099-12-31T02:00:00Z",
			want:      []string{"none"},
		},
		{
			name:      "a year in the past",
			startTime: "0 0 2 * * ? 2020",
			after:     "2026-11-18T00:00:00Z",
			want:      []string{"none"},
		},
		{
			name:      "DST: a local time that does not exist does not start",
			startTime: "CRON_TZ=Europe/Brussels 0 30 2 * * ? *",
			after:     "2027-03-27T12:00:00+01:00", // clocks move forward on 2027-03-28 at 02:00
			want:      []string{"2027-03-29T02:30:00+02:00", "2027-03-30T02:30:00+02:00"},
		},
		{
			name:      "DST: a local time that repeats starts twice",
			startTime: "CRON_TZ=Europe/Brussels 0 30 2 * * ? *",
			after:     "2026-10-24T12:00:00+02:00", // clocks move back on 2026-10-25 at 03:00
			want:      []string{"2026-10-25T02:30:00+02:00", "2026-10-25T02:30:00+01:00", "2026-10-26T02:30:00+01:00"},
		},
		{
			name:      "DST: even a fixed-year schedule starts twice",
			startTime: "CRON_TZ=Europe/Brussels 0 30 2 25 10 ? 2026",
			after:     "2026-01-01T00:00:00Z",
			want:      []string{"2026-10-25T02:30:00+02:00", "2026-10-25T02:30:00+01:00", "none"},
		},
		{
			name:      "DST: times around the transition keep their order",
			startTime: "CRON_TZ=Europe/Brussels 0 0/30 1-3 25 10 ? 2026",
			after:     "2026-10-25T01:30:00+02:00",
			want: []string{
				"2026-10-25T02:00:00+02:00", "2026-10-25T02:30:00+02:00",
				"2026-10-25T02:00:00+01:00", "2026-10-25T02:30:00+01:00",
				"2026-10-25T03:00:00+01:00", "2026-10-25T03:30:00+01:00", "none",
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			schedule := mustParse(t, tt.startTime)
			after := at(t, tt.after)
			for i, want := range tt.want {
				next, ok := schedule.Next(after)
				if want == "none" {
					if ok {
						t.Fatalf("occurrence %d after %s = %s, want none", i, tt.after, next.Format(time.RFC3339))
					}
					return
				}
				if !ok {
					t.Fatalf("occurrence %d: none, want %s", i, want)
				}
				if !next.Equal(at(t, want)) {
					t.Fatalf("occurrence %d = %s, want %s", i, next.Format(time.RFC3339), want)
				}
				after = next
			}
		})
	}
}

// README: #the-kumacokureddev-maintenancewindow-crd, #time-zones
func TestEvaluate(t *testing.T) {
	const day = 24 * time.Hour
	for _, tt := range []struct {
		name      string
		startTime string
		duration  time.Duration
		now       string
		// wantStart is the start of the current occurrence; empty when no occurrence is active.
		wantStart string
		// wantNext is the next boundary; empty when there is none.
		wantNext string
	}{
		{
			name:      "before a one-off window",
			startTime: "0 0 2 1 1 ? 2027", duration: 2 * time.Hour,
			now:      "2026-12-31T23:00:00Z",
			wantNext: "2027-01-01T02:00:00Z",
		},
		{
			name:      "at the start: the occurrence includes its start",
			startTime: "0 0 2 1 1 ? 2027", duration: 2 * time.Hour,
			now:       "2027-01-01T02:00:00Z",
			wantStart: "2027-01-01T02:00:00Z", wantNext: "2027-01-01T04:00:00Z",
		},
		{
			name:      "during the occurrence",
			startTime: "0 0 2 1 1 ? 2027", duration: 2 * time.Hour,
			now:       "2027-01-01T03:59:59.5Z",
			wantStart: "2027-01-01T02:00:00Z", wantNext: "2027-01-01T04:00:00Z",
		},
		{
			name:      "at the end: the occurrence excludes its end, and nothing follows",
			startTime: "0 0 2 1 1 ? 2027", duration: 2 * time.Hour,
			now: "2027-01-01T04:00:00Z",
		},
		{
			name:      "between two occurrences",
			startTime: "0 0 2 * * ? *", duration: 2 * time.Hour,
			now:      "2026-11-18T04:00:00Z",
			wantNext: "2026-11-19T02:00:00Z",
		},
		{
			name:      "README: an occurrence belongs to the day it starts on",
			startTime: "CRON_TZ=Europe/Brussels 0 30 23 ? * FRI *", duration: time.Hour,
			now:       "2026-11-21T00:15:00+01:00", // a Saturday
			wantStart: "2026-11-20T23:30:00+01:00", wantNext: "2026-11-21T00:30:00+01:00",
		},
		{
			name:      "occurrences without a gap form a chain",
			startTime: "0 0 0/2 18 11 ? 2026", duration: 2 * time.Hour,
			now:       "2026-11-18T13:00:00Z",
			wantStart: "2026-11-18T00:00:00Z", wantNext: "2026-11-18T14:00:00Z",
		},
		{
			name:      "overlapping occurrences form a chain",
			startTime: "0 0 * 18 11 ? 2026", duration: 90 * time.Minute,
			now:       "2026-11-18T13:10:00Z",
			wantStart: "2026-11-18T00:00:00Z", wantNext: "2026-11-18T13:30:00Z",
		},
		{
			name:      "a gap ends the chain",
			startTime: "0 0 0-5,10-23 18 11 ? 2026", duration: time.Hour,
			now:       "2026-11-18T13:10:00Z",
			wantStart: "2026-11-18T10:00:00Z", wantNext: "2026-11-18T14:00:00Z",
		},
		{
			name:      "a chain is followed back at most 31 days",
			startTime: "0 0 2 * * ? *", duration: day,
			now:       "2026-11-18T12:00:00Z",
			wantStart: "2026-10-19T02:00:00Z", wantNext: "2026-11-19T02:00:00Z",
		},
		{
			name:      "the longest duration is not a chain by itself",
			startTime: "0 0 2 1 1 ? 2027", duration: 31 * day,
			now:       "2027-01-31T12:00:00Z",
			wantStart: "2027-01-01T02:00:00Z", wantNext: "2027-02-01T02:00:00Z",
		},
		{
			name:      "DST: the repeated occurrences overlap, and the last one ends the window",
			startTime: "CRON_TZ=Europe/Brussels 0 30 2 25 10 ? 2026", duration: 90 * time.Minute,
			now:       "2026-10-25T02:45:00+01:00",
			wantStart: "2026-10-25T02:30:00+02:00", wantNext: "2026-10-25T04:00:00+02:00",
		},
		{
			name:      "DST: the second of the repeated occurrences is still active",
			startTime: "CRON_TZ=Europe/Brussels 0 30 2 25 10 ? 2026", duration: 90 * time.Minute,
			now:       "2026-10-25T03:45:00+01:00",
			wantStart: "2026-10-25T02:30:00+02:00", wantNext: "2026-10-25T04:00:00+01:00",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := mustParse(t, tt.startTime).Evaluate(tt.duration, at(t, tt.now))

			if active := tt.wantStart != ""; got.Active != active {
				t.Fatalf("Active = %v, want %v", got.Active, active)
			}
			if tt.wantStart != "" && !got.OccurrenceStart.Equal(at(t, tt.wantStart)) {
				t.Errorf("OccurrenceStart = %s, want %s", got.OccurrenceStart.Format(time.RFC3339), tt.wantStart)
			}
			switch {
			case tt.wantNext == "" && !got.NextBoundary.IsZero():
				t.Errorf("NextBoundary = %s, want none", got.NextBoundary.Format(time.RFC3339))
			case tt.wantNext != "" && !got.NextBoundary.Equal(at(t, tt.wantNext)):
				t.Errorf("NextBoundary = %s, want %s", got.NextBoundary.Format(time.RFC3339), tt.wantNext)
			}
		})
	}
}

// ZONEINFO names a time zone database that replaces the one kumaco embeds: a
// time zone that only exists in that database is known. Go reads ZONEINFO once
// per process, so the test runs itself again with the variable set.
//
// README: #time-zones
func TestParseReadsTheTimeZoneDatabaseNamedByZONEINFO(t *testing.T) {
	const startTime = "CRON_TZ=Kumaco/Test 0 0 2 * * ? *"
	if os.Getenv("ZONEINFO") != "" {
		next, _ := mustParse(t, startTime).Next(at(t, "2026-11-18T12:00:00Z"))
		if want := at(t, "2026-11-19T02:00:00+01:00"); !next.Equal(want) {
			t.Errorf("Next = %s, want %s: the rules of Europe/Brussels", next.Format(time.RFC3339), want.Format(time.RFC3339))
		}
		return
	}
	if _, err := Parse(startTime); err == nil {
		t.Fatal("Kumaco/Test is a known time zone without ZONEINFO")
	}

	archive, err := zip.OpenReader(filepath.Join(runtime.GOROOT(), "lib", "time", "zoneinfo.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	brussels, err := fs.ReadFile(archive, "Europe/Brussels")
	if err != nil {
		t.Fatal(err)
	}
	database := t.TempDir()
	if err := os.Mkdir(filepath.Join(database, "Kumaco"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(database, "Kumaco", "Test"), brussels, 0o644); err != nil {
		t.Fatal(err)
	}

	again := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	again.Env = append(os.Environ(), "ZONEINFO="+database)
	if output, err := again.CombinedOutput(); err != nil || !strings.Contains(string(output), "--- PASS: "+t.Name()) {
		t.Errorf("with ZONEINFO=%s: %v\n%s", database, err, output)
	}
}
