package api

import "testing"

func TestStreakReadsRunsOutOfActiveDays(t *testing.T) {
	// today is day 100 throughout.
	const today = 100

	for _, c := range []struct {
		name                     string
		days                     []int64
		current, longest, active int
		since                    int64 // 0 when there should be no current run
	}{{
		name: "never produced",
		days: nil,
	}, {
		name:    "produced today only",
		days:    []int64{100},
		current: 1, longest: 1, active: 1, since: 100,
	}, {
		name: "produced yesterday, nothing yet today",
		// The day is not over. Somebody who folded yesterday and has not folded in the
		// hours since midnight has broken nothing, and resetting them to zero at
		// 00:00 UTC would be the cruellest possible reading of the data.
		days:    []int64{99},
		current: 1, longest: 1, active: 1, since: 99,
	}, {
		name: "stopped two days ago",
		// Day 98 is the last, so a full day (99) passed with nothing. Broken.
		days:    []int64{96, 97, 98},
		current: 0, longest: 3, active: 3,
	}, {
		name:    "unbroken run up to today",
		days:    []int64{95, 96, 97, 98, 99, 100},
		current: 6, longest: 6, active: 6, since: 95,
	}, {
		name: "a longer run earlier, a shorter one now",
		// The best run is history; the current one is what is happening.
		days:    []int64{10, 11, 12, 13, 14, 15, 99, 100},
		current: 2, longest: 6, active: 8, since: 99,
	}, {
		name:    "single scattered days never form a run",
		days:    []int64{10, 20, 30, 40},
		current: 0, longest: 1, active: 4,
	}, {
		name: "a gap of one day breaks it",
		// 97 missing: the run before it does not join the run after it.
		days:    []int64{95, 96, 98, 99, 100},
		current: 3, longest: 3, active: 5, since: 98,
	}} {
		t.Run(c.name, func(t *testing.T) {
			got := streakOf(c.days, today)
			if got.Current != c.current {
				t.Errorf("current = %d, want %d", got.Current, c.current)
			}
			if got.Longest != c.longest {
				t.Errorf("longest = %d, want %d", got.Longest, c.longest)
			}
			if got.ActiveDays != c.active {
				t.Errorf("active days = %d, want %d", got.ActiveDays, c.active)
			}
			switch {
			case c.since == 0 && got.Since != nil:
				t.Errorf("since = %v, want none — there is no current run", got.Since)
			case c.since != 0 && got.Since == nil:
				t.Errorf("since is absent, want day %d", c.since)
			case c.since != 0 && !got.Since.Equal(dayTime(c.since)):
				t.Errorf("since = %v, want %v", got.Since, dayTime(c.since))
			}
		})
	}
}

func TestLongestStreakSurvivesEndingOnTheLastDay(t *testing.T) {
	// The final run is only compared against the best after the loop body that built
	// it, which is exactly the shape that drops a trailing maximum. An unbroken record
	// is the case where every day is the final run.
	days := make([]int64, 40)
	for i := range days {
		days[i] = int64(61 + i) // 61..100
	}
	got := streakOf(days, 100)
	if got.Longest != 40 || got.Current != 40 {
		t.Errorf("current %d, longest %d; want 40 and 40 for an unbroken record",
			got.Current, got.Longest)
	}
}
