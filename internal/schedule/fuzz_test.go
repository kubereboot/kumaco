package schedule

import (
	"testing"
	"time"
)

// README: #the-kumacokureddev-maintenancewindow-crd
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"CRON_TZ=Europe/Brussels 0 0 2 1 1 ? 2027",
		"CRON_TZ=America/New_York 0 30 1 ? * MON-FRI *",
		"0 0 22 ? * 3#2 *",
		"0 0/15 22-2 L-3,15W,LW JAN-MAR/2 ? 2030/10",
		"0 * * ? * 6L,L,2#5 *",
		"CRON_TZ=Local 0 0 2 * * ? *",
		"0 0 2 30 2 ? *",
	} {
		f.Add(seed, int64(1795000000))
	}
	f.Fuzz(func(t *testing.T, startTime string, unix int64) {
		schedule, err := Parse(startTime)
		if err != nil {
			return
		}
		after := time.Unix(unix%8_000_000_000, 0)
		next, ok := schedule.Next(after)
		if !ok {
			return
		}
		if !next.After(after) || next.Year() > maxYear || next.Nanosecond() != 0 {
			t.Fatalf("Next(%s) = %s, want a whole second after it, until %d", after, next, maxYear)
		}
		if again, _ := schedule.Next(next.Add(-time.Second)); !again.Equal(next) {
			t.Fatalf("Next(%s) = %s, but the occurrence after its previous second is %s", after, next, again)
		}
		if evaluation := schedule.Evaluate(time.Hour, next); !evaluation.Active || evaluation.OccurrenceStart.After(next) {
			t.Fatalf("Evaluate at the occurrence %s = %+v, want it active", next, evaluation)
		}
	})
}

// The worst case of Evaluate: the shortest duration, on the densest schedule,
// is a chain of 31 days of one-minute occurrences.
func BenchmarkEvaluateLongestChain(b *testing.B) {
	schedule, err := Parse("CRON_TZ=Europe/Brussels 0 * * * * ? *")
	if err != nil {
		b.Fatal(err)
	}
	now := time.Date(2026, 11, 18, 12, 0, 30, 0, time.UTC)
	for b.Loop() {
		schedule.Evaluate(time.Minute, now)
	}
}

func BenchmarkEvaluateDaily(b *testing.B) {
	schedule, err := Parse("CRON_TZ=Europe/Brussels 0 0 2 * * ? *")
	if err != nil {
		b.Fatal(err)
	}
	now := time.Date(2026, 11, 18, 2, 30, 0, 0, time.UTC)
	for b.Loop() {
		schedule.Evaluate(2*time.Hour, now)
	}
}
