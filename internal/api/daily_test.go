package api

import (
	"testing"

	"folding/internal/store"
)

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

func TestRecentWindowIsBoundedByTheRecord(t *testing.T) {
	// The window is stated rather than assumed, because a ratio over four days and a
	// ratio over thirty deserve different amounts of trust — and while this service is
	// younger than a month, "the last thirty days" and "everything we have" are the
	// same range. Reporting 30 either way would overstate what was measured.
	const today = 100

	days := []store.Day{
		{Bucket: 60, Points: 1_000_000, WUs: 1000}, // 1,000/WU, far outside any window
		{Bucket: 98, Points: 800_000, WUs: 10},     // 80,000/WU
		{Bucket: 99, Points: 400_000, WUs: 10},     // 40,000/WU
	}

	// A record older than the window: thirty days, and the ancient day is excluded.
	got := recentOf(days, today, 0)
	if got.Days != 30 {
		t.Errorf("days = %d, want the full 30-day window", got.Days)
	}
	if got.WUs != 20 || got.Points != 1_200_000 {
		t.Errorf("summed %d points over %d WUs; want only the two days inside the window",
			got.Points, got.WUs)
	}
	if want := int64(60000); got.PointsPerWU != want {
		t.Errorf("points per WU = %d, want %d", got.PointsPerWU, want)
	}

	// A record younger than the window reports the record, not the window.
	if got := recentOf(days, today, 98); got.Days != 3 {
		t.Errorf("days = %d, want the 3 days actually on record", got.Days)
	}

	// Nothing produced in the window is no ratio at all. Falling back to lifetime here
	// would silently change what the field means.
	if got := recentOf([]store.Day{{Bucket: 10, Points: 5, WUs: 5}}, today, 0); got != nil {
		t.Errorf("got %+v for a window with no production, want nothing", got)
	}
	if got := recentOf(nil, today, 0); got != nil {
		t.Errorf("got %+v for an entity with no days at all, want nothing", got)
	}

	// Points without work units cannot produce a ratio, and must not divide by zero.
	if got := recentOf([]store.Day{{Bucket: 99, Points: 500, WUs: 0}}, today, 0); got != nil {
		t.Errorf("got %+v for production with no work units, want nothing", got)
	}
}
