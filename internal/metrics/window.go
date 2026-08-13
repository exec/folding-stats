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
	"math"
	"sort"
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

	// count is how many entities existed once this cycle had been applied. Slots are
	// assigned densely in first-seen order and never reused, so "id < count" is
	// exactly "this entity already existed at this cycle" — which is what separates a
	// genuine rank movement from an entity that simply was not there before.
	count int32
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
//
// A little headroom is kept because replay calls this once per cycle, following the
// corpus as it was at the time. Sizing exactly to n would reallocate five arrays on
// every one of 168 cycles — tens of gigabytes of copying at corpus scale — while
// doubling would leave 108 MB of member windows sitting in 217 MB of allocation
// forever. An eighth covers a week of replay and months of hourly growth (the corpus
// gains a few thousand members a day against 2.7M) for ~13 MB.
func (w *Window) Grow(n int) {
	if n <= len(w.last24) {
		return
	}
	if n <= cap(w.last24) {
		// Nothing ever writes past len, so the newly exposed tail is still the zero
		// it was allocated as.
		w.last24 = w.last24[:n]
		w.last7d = w.last7d[:n]
		w.today = w.today[:n]
		w.week = w.week[:n]
		w.update = w.update[:n]
		return
	}
	c := n + n/8
	grow := func(s []int64) []int64 {
		out := make([]int64, n, c)
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
	// Callers Grow to the current entity count before pushing, so the window's own
	// length is that count.
	w.cycles = append(w.cycles, cycleDeltas{at: at, deltas: deltas, count: int32(len(w.last24))})
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

// Baseline returns the entity count as of the cycle the 24-hour window is measured
// against — the newest cycle at or before at-24h — and whether such a cycle exists.
//
// This is the companion to Last24h. Subtracting Last24h from a current total gives
// that entity's total as of exactly this cycle, so the two together reconstruct the
// corpus as it stood a day ago. An entity whose id is at or above the returned count
// did not exist then, and any movement computed for it would be invented rather than
// measured.
//
// ok is false until a full 24 hours has been observed, because until then nothing has
// aged out of the window and there is no earlier state to compare against.
//
// Replay grows the window cycle by cycle from the stored first-sighting counts, so
// these counts are the historical ones across a restart rather than today's stamped
// on every cycle.
func (w *Window) Baseline() (int32, bool) {
	if w.idx24 == 0 {
		return 0, false
	}
	return w.cycles[w.idx24-1].count, true
}

// At returns the timestamp of the newest cycle.
func (w *Window) At() time.Time { return w.at }

// Cycles reports how many cycles are currently retained.
func (w *Window) Cycles() int { return len(w.cycles) }

// PreviousAt is the publish time of the cycle before the newest one, or the zero time
// when only one has been seen.
//
// It is the denominator for LastUpdate. That figure is production "in the most recent
// cycle", and a cycle is not reliably an hour: the measured interval drifts a few
// seconds either way and stretches when upstream publishes late. Anyone deriving a rate
// from it was dividing by an assumption; with both ends published they can divide by
// what actually happened.
func (w *Window) PreviousAt() time.Time {
	if len(w.cycles) < 2 {
		return time.Time{}
	}
	return w.cycles[len(w.cycles)-2].at
}

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

// avgInterval is the mean gap between retained cycles, used to account for the
// production the oldest retained delta covers but that no retained cycle precedes.
func (w *Window) avgInterval() time.Duration {
	if len(w.cycles) < 2 {
		return 0
	}
	return w.at.Sub(w.cycles[0].at) / time.Duration(len(w.cycles)-1)
}

// CoveredSpan is the period the retained deltas actually cover, and the denominator
// for anything that has existed throughout the window — the project as a whole.
//
// Two cases, and conflating them is a factor-of-two error on a young window. Once
// cycles have started ageing out, the oldest retained one describes production over
// the interval *before* it, which no retained cycle marks the start of — so one mean
// interval is added, without which a full 168-cycle window measures 167 hours. But
// while the oldest retained cycle is still the first ever observed, it contributed no
// deltas at all (every entity in it was a first sighting), so observation genuinely
// begins there and adding an interval would invent one.
func (w *Window) CoveredSpan() time.Duration {
	if len(w.cycles) == 0 {
		return 0
	}
	span := w.at.Sub(w.cycles[0].at)
	if !w.cycles[0].at.Equal(w.firstAt) {
		span += w.avgInterval()
	}
	if span > weekSpan {
		span = weekSpan
	}
	return span
}

// ObservedSpan is how long id has been under observation within the window.
//
// This is the denominator its rates are honestly divisible by. For an entity that
// predates the window it is the whole window; for one that appeared inside it, the
// period since it appeared — never longer, because we cannot claim to have watched
// something before we had ever seen it.
//
// Slots are dense and assigned in first-seen order, and each cycle records the corpus
// size at the time, so the cycle an entity appeared at is a binary search over a
// non-decreasing sequence rather than a scan.
func (w *Window) ObservedSpan(id int32) time.Duration {
	if len(w.cycles) == 0 || id < 0 {
		return 0
	}
	i := sort.Search(len(w.cycles), func(i int) bool { return w.cycles[i].count > id })
	if i == len(w.cycles) {
		return 0 // never present at any retained cycle
	}
	if i == 0 {
		return w.CoveredSpan()
	}
	// It first appeared at cycle i, so its earliest possible delta is the next one,
	// covering the interval that starts here. No mean-interval adjustment: this
	// boundary is exact.
	return w.at.Sub(w.cycles[i].at)
}

// PointsPerDay is the 7-day moving average expressed as points per day.
//
// This is the figure EOC displays as "24hr Avg", which is a misnomer: their FAQ
// states plainly that "a 24hr average is simply the past 7 days total divided by
// 7". It is the input to every projection on their site, and naming it honestly
// here is deliberate — see R7.
//
// The divisor is the period this entity has actually been observed over, not a flat
// seven days. Dividing a two-day-old donor's output by seven averages in five days
// during which they did not exist, so a new arrival would surface at a seventh of
// their real rate and creep up over a week to the figure that was true on day one.
// Days they existed for and produced nothing on are real zeros and still count —
// only days before we had ever seen them are excluded.
func (w *Window) PointsPerDay(id int32) int64 {
	return PerDay(w.get(w.last7d, id), w.ObservedSpan(id))
}

// PointsPerDay24h is production in the rolling day, expressed as points per day.
//
// The same question as PointsPerDay over a window a seventh as long, which makes it a
// far livelier number: a machine switched on this morning shows up here today and takes
// most of a week to move the seven-day figure. That is the point, and so is the cost —
// one good night reads as a permanent rate, and a projection built on it will say so.
// Both are published so the two can be read against each other; where they disagree by
// a lot, something changed recently.
//
// Points in the last twenty-four hours are already a daily rate, so there is nothing to
// divide in the ordinary case. The span still matters at the edge: an entity first seen
// three hours ago has three hours of production in this window, and reporting that as a
// day's work would understate it by eight times. Only the period actually observed
// counts, capped at the window itself.
func (w *Window) PointsPerDay24h(id int32) int64 {
	span := w.ObservedSpan(id)
	if span > day {
		span = day
	}
	return PerDay(w.get(w.last24, id), span)
}

// PerDay divides production by the period it was observed over, as points per day.
//
// A full window keeps exact integer arithmetic, because that path is checked against
// three independently captured EOC pages: truncation reproduces none of them and
// rounding to nearest reproduces all three (51,364,842/7 = 7,337,834.57, published as
// 7,337,835). Matching exactly keeps these figures reconcilable with the site donors
// already compare against.
//
// A zero or negative span means nothing has been observed yet, which is not the same
// as a rate of zero — but with no production recorded either, zero is the only figure
// that does not invent one.
func PerDay(points int64, span time.Duration) int64 {
	if span >= weekSpan {
		if points < 0 {
			return (points - avgWindow/2) / avgWindow
		}
		return (points + avgWindow/2) / avgWindow
	}
	if span <= 0 {
		return 0
	}
	return int64(math.Round(float64(points) / (float64(span) / float64(day))))
}

func startOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// startOfWeek returns 00:00 UTC on the Sunday of t's week.
//
// This was ISO (Monday) on the reasoning that we are not obliged to reproduce EOC's
// calendar. That was the wrong call for one reason: donors reconcile these figures
// against EOC, which buckets on Sunday, and a weekly total that silently covers a
// different seven days than the site being compared against is worse than an opinion
// about which weekday is correct. The boundary is still UTC (R14) — only the weekday
// moved.
//
// One definition of "week" serves the whole system: points_this_week_utc, the weekly
// history buckets and the Weekly leaderboard all start here. Two definitions would let
// the same response disagree with itself about a period it names.
func startOfWeek(t time.Time) time.Time {
	d := startOfDay(t)
	return d.AddDate(0, 0, -int(d.Weekday())) // time.Sunday == 0
}
