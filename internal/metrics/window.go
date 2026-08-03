// Package metrics maintains the rolling production windows that every rate on the
// site is derived from.
//
// The obvious implementation — keep N past snapshots and subtract — does not fit:
// 24 hourly snapshots of 2.7M int64 scores is 522 MB for a single field. Instead we
// exploit sparsity. Only ~1% of donors produce anything in a given hour, so each
// cycle is stored as a short list of (entity, delta) pairs and the windows are
// maintained incrementally: add the entering cycle, subtract the ones that have
// aged out. Seven days of history costs ~73 MB and each update is O(changed), not
// O(members).
//
// # Window semantics
//
// Two different notions of "recent" coexist, and conflating them is the single
// easiest way to get these numbers wrong:
//
//   - Last24h and Last7d are *rolling* windows measured backwards from the newest
//     cycle.
//   - Today and ThisWeek are *calendar* buckets that reset at UTC midnight and at
//     00:00 UTC Monday respectively.
//
// So Today can be far smaller than Last24h just after midnight, and equal to
// ThisWeek all day Monday. Both are correct; they answer different questions.
//
// Production is attributed in full to the cycle in which it was observed. A cycle
// published at 00:30 covers work submitted from 23:30 onward, but we do not attempt
// to split it across the midnight boundary — upstream gives us no way to know when
// within the interval the points were banked.
package metrics

import (
	"time"

	"folding/internal/model"
)

const (
	day       = 24 * time.Hour
	weekSpan  = 7 * day
	avgWindow = 7 // Last7d / 7 == the "points per day" figure
)

// cycleDeltas is one cycle's sparse production.
type cycleDeltas struct {
	at     time.Time
	deltas []model.Delta
}

// Window tracks rolling and calendar production for a dense set of entity ids.
//
// It is not safe for concurrent use; the ingest loop owns it and publishes results
// by swapping an immutable snapshot.
type Window struct {
	cycles []cycleDeltas // ordered oldest to newest, spanning at most 7 days
	idx24  int           // index of the oldest cycle still inside the 24h window

	last24 []int64
	last7d []int64
	today  []int64
	week   []int64
	update []int64

	// prev is the newest cycle's delta list, retained so the per-cycle "update"
	// figures can be cleared sparsely instead of zeroing 2.7M entries each time.
	prev []model.Delta

	at        time.Time
	firstAt   time.Time
	dayStart  time.Time
	weekStart time.Time
}

// New returns a Window sized for n entities.
func New(n int) *Window {
	return &Window{
		last24: make([]int64, n),
		last7d: make([]int64, n),
		today:  make([]int64, n),
		week:   make([]int64, n),
		update: make([]int64, n),
	}
}

// Grow extends the window to cover at least n entities. New entities start at zero,
// which is correct: an entity first seen this cycle has no observed production.
func (w *Window) Grow(n int) {
	if n <= len(w.last24) {
		return
	}
	grow := func(s []int64) []int64 {
		out := make([]int64, n)
		copy(out, s)
		return out
	}
	w.last24 = grow(w.last24)
	w.last7d = grow(w.last7d)
	w.today = grow(w.today)
	w.week = grow(w.week)
	w.update = grow(w.update)
}

// Push folds one cycle's deltas into the windows.
//
// Cycles must arrive in ascending time order; out-of-order pushes are ignored,
// because applying them would corrupt every window in ways no later cycle could
// repair. Replay therefore has to run forwards, which it does naturally.
//
// Push retains deltas. The caller must not mutate the slice afterwards.
func (w *Window) Push(at time.Time, deltas []model.Delta) {
	at = at.UTC()
	if !w.at.IsZero() && !at.After(w.at) {
		return
	}
	if w.firstAt.IsZero() {
		w.firstAt = at
	}

	// Calendar buckets reset on their boundary, before this cycle is counted.
	if ds := startOfDay(at); !ds.Equal(w.dayStart) {
		clear(w.today)
		w.dayStart = ds
	}
	if ws := startOfWeek(at); !ws.Equal(w.weekStart) {
		clear(w.week)
		w.weekStart = ws
	}

	// "Points this update" describes only the newest cycle, so clear the previous
	// one's entries rather than the whole array.
	for _, d := range w.prev {
		w.update[d.ID] = 0
	}

	for _, d := range deltas {
		id := d.ID
		w.last24[id] += d.DScore
		w.last7d[id] += d.DScore
		w.today[id] += d.DScore
		w.week[id] += d.DScore
		w.update[id] = d.DScore
	}
	w.cycles = append(w.cycles, cycleDeltas{at: at, deltas: deltas})
	w.prev = deltas
	w.at = at

	w.expire(at)
}

// expire advances both window boundaries. The 24h boundary is advanced first so the
// index stays valid when older cycles are dropped from the front of the slice.
func (w *Window) expire(now time.Time) {
	cut24 := now.Add(-day)
	for w.idx24 < len(w.cycles) && !w.cycles[w.idx24].at.After(cut24) {
		for _, d := range w.cycles[w.idx24].deltas {
			w.last24[d.ID] -= d.DScore
		}
		w.idx24++
	}

	cut7 := now.Add(-weekSpan)
	drop := 0
	for drop < len(w.cycles) && !w.cycles[drop].at.After(cut7) {
		for _, d := range w.cycles[drop].deltas {
			w.last7d[d.ID] -= d.DScore
		}
		drop++
	}
	if drop > 0 {
		// Retain the tail rather than re-slicing in place so the dropped cycles'
		// delta slices become collectable.
		w.cycles = append([]cycleDeltas(nil), w.cycles[drop:]...)
		w.idx24 -= drop
		if w.idx24 < 0 {
			w.idx24 = 0
		}
	}
}

// At returns the timestamp of the newest cycle.
func (w *Window) At() time.Time { return w.at }

// Cycles reports how many cycles are currently retained.
func (w *Window) Cycles() int { return len(w.cycles) }

// Span is how much history the window actually covers. Before a full week has been
// observed this is less than 7 days, which makes the average unrepresentative.
func (w *Window) Span() time.Duration {
	if w.firstAt.IsZero() {
		return 0
	}
	return w.at.Sub(w.firstAt)
}

// Complete reports whether a full 7 days has been observed. Until it is true the
// per-day average is computed over a partial window and the API should say so
// rather than quietly presenting a number that is too low.
func (w *Window) Complete() bool { return w.Span() >= weekSpan }

func (w *Window) get(s []int64, id int32) int64 {
	if id < 0 || int(id) >= len(s) {
		return 0
	}
	return s[id]
}

// LastUpdate is production in the most recent cycle.
func (w *Window) LastUpdate(id int32) int64 { return w.get(w.update, id) }

// Last24h is production in the rolling 24 hours ending at the newest cycle.
func (w *Window) Last24h(id int32) int64 { return w.get(w.last24, id) }

// Last7d is production in the rolling 7 days ending at the newest cycle.
func (w *Window) Last7d(id int32) int64 { return w.get(w.last7d, id) }

// Today is production since 00:00 UTC.
func (w *Window) Today(id int32) int64 { return w.get(w.today, id) }

// ThisWeek is production since 00:00 UTC Monday.
func (w *Window) ThisWeek(id int32) int64 { return w.get(w.week, id) }

// PointsPerDay is the 7-day moving average expressed as points per day.
//
// This is the figure EOC displays as "24hr Avg", which is a misnomer: their FAQ
// states plainly that "a 24hr average is simply the past 7 days total divided by
// 7". It is the input to every projection on their site, and naming it honestly
// here is deliberate — see R7.
//
// The division rounds to nearest rather than truncating. That is not cosmetic:
// checked against three independently captured EOC pages, truncation reproduces
// none of them and rounding reproduces all three (e.g. 51,364,842/7 = 7,337,834.57,
// published as 7,337,835). Matching exactly keeps our figures reconcilable with the
// site donors already compare against.
func (w *Window) PointsPerDay(id int32) int64 {
	v := w.get(w.last7d, id)
	if v < 0 {
		return (v - avgWindow/2) / avgWindow
	}
	return (v + avgWindow/2) / avgWindow
}

func startOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// startOfWeek returns 00:00 UTC on the Monday of t's week. ISO weeks start Monday;
// EOC buckets on Sunday, but we are not reproducing their calendar (see R14 on
// serving UTC rather than their hard-coded Central time).
func startOfWeek(t time.Time) time.Time {
	d := startOfDay(t)
	offset := (int(d.Weekday()) + 6) % 7 // Monday == 0
	return d.AddDate(0, 0, -offset)
}
