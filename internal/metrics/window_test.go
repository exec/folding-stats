package metrics

import (
	"testing"
	"time"

	"folding/internal/model"
)

// Monday 2026-08-03 00:00 UTC, so week boundaries are easy to reason about.
func mon(h int) time.Time {
	return time.Date(2026, 8, 3, h, 0, 0, 0, time.UTC)
}

func d(id int32, score int64) model.Delta { return model.Delta{ID: id, DScore: score} }

func deltas(ds ...model.Delta) []model.Delta { return ds }

func TestSingleCyclePopulatesEveryWindow(t *testing.T) {
	w := New(4)
	w.Push(mon(6), deltas(d(0, 500)))

	for _, tc := range []struct {
		name string
		got  int64
		want int64
	}{
		{"LastUpdate", w.LastUpdate(0), 500},
		{"Last24h", w.Last24h(0), 500},
		{"Last7d", w.Last7d(0), 500},
		{"Today", w.Today(0), 500},
		{"ThisWeek", w.ThisWeek(0), 500},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
	// An entity that produced nothing must read zero everywhere.
	if w.Last7d(1) != 0 || w.Today(1) != 0 {
		t.Error("idle entity has non-zero windows")
	}
}

func TestUpdateReflectsOnlyNewestCycle(t *testing.T) {
	w := New(2)
	w.Push(mon(1), deltas(d(0, 100)))
	w.Push(mon(2), deltas(d(0, 250)))

	if got := w.LastUpdate(0); got != 250 {
		t.Errorf("LastUpdate = %d, want 250", got)
	}
	if got := w.Last24h(0); got != 350 {
		t.Errorf("Last24h = %d, want 350 (cumulative)", got)
	}
}

func TestUpdateClearsForNewlyIdleEntity(t *testing.T) {
	// An entity that produced last cycle but not this one must report zero for the
	// update, not its stale previous figure. The clear is sparse, so this is
	// exactly the path that would silently rot.
	w := New(2)
	w.Push(mon(1), deltas(d(0, 100), d(1, 200)))
	w.Push(mon(2), deltas(d(1, 300)))

	if got := w.LastUpdate(0); got != 0 {
		t.Errorf("LastUpdate for newly idle entity = %d, want 0", got)
	}
	if got := w.LastUpdate(1); got != 300 {
		t.Errorf("LastUpdate = %d, want 300", got)
	}
	if got := w.Last24h(0); got != 100 {
		t.Errorf("Last24h = %d, want 100 (still inside the window)", got)
	}
}

func TestRolling24hEvictsButKeeps7d(t *testing.T) {
	// The two windows share one cycle list; an eviction from 24h must not touch
	// the 7-day totals.
	w := New(2)
	w.Push(mon(0), deltas(d(0, 100)))
	w.Push(mon(10), deltas(d(0, 200)))
	w.Push(mon(30), deltas(d(0, 400))) // mon(0) is now >24h old

	if got := w.Last24h(0); got != 600 {
		t.Errorf("Last24h = %d, want 600 (the 100 aged out)", got)
	}
	if got := w.Last7d(0); got != 700 {
		t.Errorf("Last7d = %d, want 700 (nothing aged out yet)", got)
	}
}

func TestRolling7dEviction(t *testing.T) {
	w := New(2)
	w.Push(mon(0), deltas(d(0, 100)))
	w.Push(mon(24), deltas(d(0, 200)))
	w.Push(mon(24*8), deltas(d(0, 400))) // both earlier cycles now >7d old

	if got := w.Last7d(0); got != 400 {
		t.Errorf("Last7d = %d, want 400", got)
	}
	if got := w.Last24h(0); got != 400 {
		t.Errorf("Last24h = %d, want 400", got)
	}
	if w.Cycles() != 1 {
		t.Errorf("retained %d cycles, want 1", w.Cycles())
	}
}

func TestTodayResetsAtUTCMidnight(t *testing.T) {
	w := New(2)
	w.Push(mon(22), deltas(d(0, 100)))
	w.Push(mon(23), deltas(d(0, 200)))
	if got := w.Today(0); got != 300 {
		t.Fatalf("Today before midnight = %d, want 300", got)
	}

	w.Push(mon(25), deltas(d(0, 50))) // 01:00 the next day
	if got := w.Today(0); got != 50 {
		t.Errorf("Today after midnight = %d, want 50", got)
	}
	// The rolling window must NOT reset — this is the distinction that makes
	// Today and Last24h different numbers.
	if got := w.Last24h(0); got != 350 {
		t.Errorf("Last24h across midnight = %d, want 350", got)
	}
}

// sun is 2026-08-02, the Sunday before mon's Monday.
func sun(h int) time.Time {
	return time.Date(2026, 8, 2, h, 0, 0, 0, time.UTC)
}

func TestThisWeekResetsOnSunday(t *testing.T) {
	w := New(2)
	// Saturday: the last day of the preceding week.
	sat := sun(0).Add(-6 * time.Hour)
	w.Push(sat, deltas(d(0, 900)))
	if got := w.ThisWeek(0); got != 900 {
		t.Fatalf("ThisWeek on Saturday = %d, want 900", got)
	}

	w.Push(sun(1), deltas(d(0, 40)))
	if got := w.ThisWeek(0); got != 40 {
		t.Errorf("ThisWeek after Sunday rollover = %d, want 40", got)
	}
	// On the first day of the week, Today and ThisWeek necessarily agree.
	if w.Today(0) != w.ThisWeek(0) {
		t.Errorf("Today=%d ThisWeek=%d, want equal on Sunday", w.Today(0), w.ThisWeek(0))
	}

	// Monday now continues the week that Sunday opened. This is the boundary that
	// moved off ISO, and it is the assertion that fails if it ever moves back.
	w.Push(mon(1), deltas(d(0, 5)))
	if got := w.ThisWeek(0); got != 45 {
		t.Errorf("ThisWeek on the Monday after = %d, want 45 (same week as Sunday)", got)
	}
}

func TestPointsPerDayIsSevenDayAverage(t *testing.T) {
	// The load-bearing formula: EOC's "24hr Avg" is Last7d/7, confirmed verbatim
	// by their FAQ and by arithmetic on three separate captured pages.
	// Every case is a real (Last7days, published average) pair captured from EOC.
	// They pin down the rounding mode too: truncation reproduces none of them.
	for _, tc := range []struct {
		name   string
		last7d int64
		want   int64
	}{
		{"Wisconsin team", 51_364_842, 7_337_835},
		{"DH user", 49_559_068, 7_079_867},
		{"site aggregate", 123_079_584_757, 17_582_797_822},
	} {
		w := New(2)
		w.Push(mon(1), deltas(d(0, tc.last7d)))
		if got := w.PointsPerDay(0); got != tc.want {
			t.Errorf("%s: PointsPerDay = %d, want %d (EOC published value)", tc.name, got, tc.want)
		}
	}
}

func TestPointsPerDayRoundsToNearest(t *testing.T) {
	for _, tc := range []struct{ last7d, want int64 }{
		{7, 1},
		{10, 1}, // 1.43 -> 1
		{11, 2}, // 1.57 -> 2
		{0, 0},
		{3, 0}, // 0.43 -> 0
		{4, 1}, // 0.57 -> 1
	} {
		w := New(2)
		w.Push(mon(1), deltas(d(0, tc.last7d)))
		if got := w.PointsPerDay(0); got != tc.want {
			t.Errorf("PointsPerDay(last7d=%d) = %d, want %d", tc.last7d, got, tc.want)
		}
	}
}

func TestPointsPerDayIsNotLast24h(t *testing.T) {
	// A donor can be spiky: DH showed Last24h 34,539,445 against a 7-day average of
	// 7,079,867 — nearly 5x. Anything treating the average as a 24-hour figure
	// would be badly wrong.
	w := New(2)
	w.Push(mon(1), deltas(d(0, 34_539_445)))
	if w.PointsPerDay(0) == w.Last24h(0) {
		t.Error("PointsPerDay equals Last24h; the average must span 7 days")
	}
	if got := w.PointsPerDay(0); got != 34_539_445/7 {
		t.Errorf("PointsPerDay = %d, want %d", got, int64(34_539_445/7))
	}
}

func TestOutOfOrderCycleIsIgnored(t *testing.T) {
	// Applying a stale cycle would corrupt every window irreparably, so it is
	// refused rather than merged.
	w := New(2)
	w.Push(mon(5), deltas(d(0, 100)))
	w.Push(mon(3), deltas(d(0, 999)))

	if got := w.Last7d(0); got != 100 {
		t.Errorf("Last7d = %d, want 100 (stale cycle ignored)", got)
	}
	if !w.At().Equal(mon(5)) {
		t.Errorf("At = %v, want %v", w.At(), mon(5))
	}
}

func TestDuplicateTimestampIsIgnored(t *testing.T) {
	w := New(2)
	w.Push(mon(5), deltas(d(0, 100)))
	w.Push(mon(5), deltas(d(0, 100)))
	if got := w.Last7d(0); got != 100 {
		t.Errorf("Last7d = %d, want 100 (duplicate cycle ignored)", got)
	}
}

func TestCompletenessReporting(t *testing.T) {
	// Until a full week is observed the average is computed over a partial window
	// and reads low. The API has to be able to say so.
	w := New(2)
	w.Push(mon(0), deltas(d(0, 100)))
	if w.Complete() {
		t.Error("Complete on the first cycle")
	}
	w.Push(mon(24*3), deltas(d(0, 100)))
	if w.Complete() {
		t.Errorf("Complete after %v, want false", w.Span())
	}
	w.Push(mon(24*7), deltas(d(0, 100)))
	if !w.Complete() {
		t.Errorf("not Complete after %v, want true", w.Span())
	}
}

func TestGrowPreservesExistingTotals(t *testing.T) {
	// New members appear every cycle; growing the arrays must not disturb the
	// entities already being tracked.
	w := New(2)
	w.Push(mon(1), deltas(d(0, 100), d(1, 200)))
	w.Grow(5)
	w.Push(mon(2), deltas(d(4, 50)))

	if got := w.Last7d(0); got != 100 {
		t.Errorf("Last7d(0) after Grow = %d, want 100", got)
	}
	if got := w.Last7d(1); got != 200 {
		t.Errorf("Last7d(1) after Grow = %d, want 200", got)
	}
	if got := w.Last7d(4); got != 50 {
		t.Errorf("Last7d(4) = %d, want 50", got)
	}
}

func TestOutOfRangeIdIsZeroNotPanic(t *testing.T) {
	w := New(2)
	w.Push(mon(1), deltas(d(0, 100)))
	if got := w.Last7d(99); got != 0 {
		t.Errorf("Last7d(99) = %d, want 0", got)
	}
	if got := w.Last7d(-1); got != 0 {
		t.Errorf("Last7d(-1) = %d, want 0", got)
	}
}

func TestGapInCyclesDoesNotBreakWindows(t *testing.T) {
	// Upstream feed outages are, per EOC's FAQ, "a common occurrence". Windows are
	// time-based rather than cycle-counted precisely so a gap does not stretch them.
	w := New(2)
	w.Push(mon(0), deltas(d(0, 100)))
	// 30-hour gap: the first cycle must age out of 24h purely on time.
	w.Push(mon(30), deltas(d(0, 200)))

	if got := w.Last24h(0); got != 200 {
		t.Errorf("Last24h after gap = %d, want 200", got)
	}
	if got := w.Last7d(0); got != 300 {
		t.Errorf("Last7d after gap = %d, want 300", got)
	}
}

// TestWindowMatchesBruteForce cross-checks the incremental arithmetic against a
// naive recomputation over the full cycle history. The incremental path is the one
// that runs in production; the brute-force path is obviously correct. They must
// agree at every step, including across eviction and calendar boundaries.
func TestWindowMatchesBruteForce(t *testing.T) {
	const entities = 8
	w := New(entities)

	type record struct {
		at time.Time
		ds []model.Delta
	}
	var history []record

	start := mon(0).Add(-3 * day) // start mid-week so boundaries are crossed
	rng := uint64(12345)
	next := func(n int) int {
		rng = rng*6364136223846793005 + 1442695040888963407
		return int((rng >> 33) % uint64(n))
	}

	for i := 0; i < 400; i++ { // ~16 days at 1h steps
		at := start.Add(time.Duration(i) * time.Hour)
		var ds []model.Delta
		for e := 0; e < entities; e++ {
			if next(3) == 0 { // ~1/3 of entities active per cycle
				ds = append(ds, model.Delta{ID: int32(e), DScore: int64(next(1000) + 1)})
			}
		}
		w.Push(at, ds)
		history = append(history, record{at, ds})

		for e := int32(0); e < entities; e++ {
			var b24, b7, bToday, bWeek int64
			for _, h := range history {
				if h.at.After(at.Add(-day)) {
					b24 += sumFor(h.ds, e)
				}
				if h.at.After(at.Add(-weekSpan)) {
					b7 += sumFor(h.ds, e)
				}
				if !h.at.Before(startOfDay(at)) {
					bToday += sumFor(h.ds, e)
				}
				if !h.at.Before(startOfWeek(at)) {
					bWeek += sumFor(h.ds, e)
				}
			}
			if got := w.Last24h(e); got != b24 {
				t.Fatalf("cycle %d entity %d: Last24h = %d, brute force %d", i, e, got, b24)
			}
			if got := w.Last7d(e); got != b7 {
				t.Fatalf("cycle %d entity %d: Last7d = %d, brute force %d", i, e, got, b7)
			}
			if got := w.Today(e); got != bToday {
				t.Fatalf("cycle %d entity %d: Today = %d, brute force %d", i, e, got, bToday)
			}
			if got := w.ThisWeek(e); got != bWeek {
				t.Fatalf("cycle %d entity %d: ThisWeek = %d, brute force %d", i, e, got, bWeek)
			}
		}
	}
}

func sumFor(ds []model.Delta, id int32) int64 {
	var n int64
	for _, d := range ds {
		if d.ID == id {
			n += d.DScore
		}
	}
	return n
}

func TestRetainedCyclesStayBounded(t *testing.T) {
	// Memory is the reason this design exists; the ring must not grow without
	// bound no matter how long it runs.
	w := New(4)
	for i := 0; i < 24*30; i++ { // 30 days of hourly cycles
		w.Push(mon(0).Add(time.Duration(i)*time.Hour), deltas(d(0, 10)))
	}
	if w.Cycles() > 24*7+2 {
		t.Errorf("retained %d cycles, want ~168 (7 days)", w.Cycles())
	}
	if got := w.Last7d(0); got != 24*7*10 {
		t.Errorf("Last7d = %d, want %d", got, 24*7*10)
	}
}
