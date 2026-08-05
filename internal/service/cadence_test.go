package service

import (
	"testing"
	"time"
)

// series builds snapshot instants from consecutive gaps, in seconds.
func series(start time.Time, gaps ...int) []time.Time {
	out := []time.Time{start}
	for _, g := range gaps {
		out = append(out, out[len(out)-1].Add(time.Duration(g)*time.Second))
	}
	return out
}

var base = time.Date(2026, 8, 2, 22, 29, 7, 0, time.UTC)

func TestEstimateTracksMeasuredDrift(t *testing.T) {
	// The real intervals, from five consecutive upstream publishes.
	c := newCadence()
	c.Observe(series(base, 3606, 3612, 3612, 3609))

	if !c.Measured() {
		t.Error("Measured() = false after four observed intervals")
	}
	if got := c.Interval(); got != 3612*time.Second {
		t.Errorf("Interval() = %v, want 3612s (median of 3606,3609,3612,3612)", got)
	}
	// The point of measuring: the estimate must beat the hardcoded hour it replaced.
	if got := c.Interval(); got <= time.Hour {
		t.Errorf("Interval() = %v, expected longer than the nominal hour", got)
	}
}

func TestFallsBackBeforeAnythingIsObserved(t *testing.T) {
	c := newCadence()
	if got := c.Interval(); got != nominalInterval {
		t.Errorf("fresh Interval() = %v, want %v", got, nominalInterval)
	}
	if c.Measured() {
		t.Error("Measured() = true with no observations")
	}
	// One snapshot is zero intervals, so still nothing to measure.
	c.Observe(series(base))
	if c.Measured() {
		t.Error("Measured() = true from a single snapshot")
	}
}

func TestMissedPublishDoesNotPoisonTheEstimate(t *testing.T) {
	// A skipped publish shows up as one interval of two hours. A mean would be
	// dragged past the truth and stay there; the median must ignore it.
	c := newCadence()
	c.Observe(series(base, 3610, 3611, 7220, 3609, 3612))

	if got := c.Interval(); got < 3605*time.Second || got > 3615*time.Second {
		t.Errorf("Interval() = %v, want ~3610s despite the gap", got)
	}
}

func TestAbsurdGapsAreRejectedEntirely(t *testing.T) {
	// A backfill replays months of archived snapshots back to back. None of those
	// intervals describe the live cadence.
	c := newCadence()
	c.Observe(series(base, 2, 3, 2, 4))
	if got := c.Interval(); got != nominalInterval {
		t.Errorf("Interval() = %v after a backfill, want the %v fallback", got, nominalInterval)
	}
	if c.Measured() {
		t.Error("Measured() = true from intervals outside the plausible range")
	}
}

func TestPollSlowsWhenNothingIsDueAndSpeedsUpNearThePublish(t *testing.T) {
	c := newCadence()
	c.Observe(series(base, 3610, 3610, 3610))
	at := base
	idle := 10 * time.Minute

	cases := []struct {
		name string
		now  time.Time
		want time.Duration
	}{
		{"just ingested", at.Add(time.Second), idle},
		{"half an hour out", at.Add(30 * time.Minute), idle},
		// Inside the last idle interval, wait exactly until the window opens rather
		// than overshooting it by most of a tick.
		{"eight minutes out", at.Add(52 * time.Minute), 7*time.Minute + 10*time.Second},
		{"window open", at.Add(60 * time.Minute), time.Minute},
		{"upstream is late", at.Add(90 * time.Minute), time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.PollDelay(tc.now, at, idle); got != tc.want {
				t.Errorf("PollDelay = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPollFallsBackWithNoSnapshotYet(t *testing.T) {
	c := newCadence()
	if got := c.PollDelay(base, time.Time{}, 10*time.Minute); got != 10*time.Minute {
		t.Errorf("PollDelay with no snapshot = %v, want the idle interval", got)
	}
}

func TestCaptureLagIsBoundedByTheCloseInterval(t *testing.T) {
	// The property the countdown depends on: however the poll schedule falls, a
	// publish is captured within about a minute of appearing — not the five-minute
	// average a fixed ten-minute tick produced.
	c := newCadence()
	c.Observe(series(base, 3610, 3610, 3610))

	at := base
	publish := at.Add(3610 * time.Second)
	idle := 10 * time.Minute

	now := at
	for i := 0; i < 200 && now.Before(publish); i++ {
		now = now.Add(c.PollDelay(now, at, idle))
	}
	if lag := now.Sub(publish); lag > time.Minute {
		t.Errorf("captured %v after publish, want within a minute", lag)
	}
}

// TestRestartAtAnyMomentStillCatchesThePublish encodes the property a deploy needs:
// restarting at any point, including in the middle of an update, must not delay the
// next cycle beyond the schedule that would have applied anyway.
//
// The scenario is the one that happened. Upstream publishes the team feed at 06:35:04
// and the user feed at 06:36:11 — a cycle needs both, and a restart's fetch landing
// between them archives half a pair. What decides whether that costs a minute or ten
// is entirely this function: state.At is still the last *applied* cycle, so the
// window it opens around the predicted publish stays open until a cycle actually
// forms, however many restarts happen in between.
func TestRestartAtAnyMomentStillCatchesThePublish(t *testing.T) {
	c := newCadence()
	// Measured cadence, as production reports it.
	base := time.Date(2026, 8, 5, 4, 35, 51, 0, time.UTC)
	var times []time.Time
	for i := range 8 {
		times = append(times, base.Add(time.Duration(i)*(time.Hour+7*time.Second)))
	}
	c.Observe(times)

	const idle = 10 * time.Minute
	applied := time.Date(2026, 8, 5, 5, 35, 58, 0, time.UTC) // last complete cycle
	predicted := c.NextAfter(applied)

	for _, tc := range []struct {
		name    string
		restart time.Time
		wantMax time.Duration
	}{
		// Long before the publish: nothing is due, so waiting is correct — but never
		// past the moment the window opens.
		{"half an hour early", predicted.Add(-30 * time.Minute), idle},
		// Inside the window, which is where a deploy is most likely to hurt.
		{"a minute early", predicted.Add(-time.Minute), time.Minute},
		{"seconds early", predicted.Add(-11 * time.Second), time.Minute},
		{"seconds late", predicted.Add(11 * time.Second), time.Minute},
		// The case that cost ten minutes: the team half is archived, the user half is
		// not, and no cycle has formed — so state.At has not moved and the window is
		// still open however long ago the restart was.
		{"ten minutes late, half a pair on disk", predicted.Add(10 * time.Minute), time.Minute},
		{"an hour late", predicted.Add(time.Hour), time.Minute},
	} {
		got := c.PollDelay(tc.restart, applied, idle)
		if got > tc.wantMax {
			t.Errorf("%s: delay %v exceeds %v — a restart here would sit on a half-formed cycle",
				tc.name, got, tc.wantMax)
		}
		// Landing inside the window must never mean waiting out the idle interval.
		if tc.wantMax < idle && got >= idle {
			t.Errorf("%s: delay %v fell back to the idle interval", tc.name, got)
		}
	}

	// And the early case must still be bounded by when the window opens, not by idle:
	// a restart 11 minutes before the publish should wake at the window, not 10
	// minutes later and 1 minute after it.
	early := predicted.Add(-11 * time.Minute)
	if got := c.PollDelay(early, applied, idle); got > 10*time.Minute {
		t.Errorf("restart 11 min before the publish waits %v; the window opens sooner", got)
	}
}
