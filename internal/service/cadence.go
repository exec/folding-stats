package service

import (
	"sort"
	"sync"
	"time"
)

// Upstream does not publish on a wall clock.
//
// Measured over consecutive publishes, the interval runs 3606–3613s — always longer
// than an hour, drifting roughly ten seconds later each cycle, so the publish time
// sweeps a full hour about every two weeks. Predicting the next publish as at+1h is
// therefore wrong by a margin that grows all day, and a countdown built on it would
// reach zero before there was anything to fetch.
//
// The fix is to stop assuming and start measuring: estimate the interval from the
// snapshots we have actually observed.
const (
	// nominalInterval is the fallback before enough cycles exist to measure, and the
	// centre of the range an estimate is allowed to occupy.
	nominalInterval = time.Hour
	// minInterval and maxInterval bound the estimate. A backfill, a clock step, or a
	// missed publish produces intervals that are not the cadence, and an unbounded
	// median would let one of them poison the prediction for hours.
	minInterval = 50 * time.Minute
	maxInterval = 75 * time.Minute
	// cadenceSamples is how many recent intervals feed the estimate. Enough to be
	// robust to one anomaly, short enough to track the drift as it moves.
	cadenceSamples = 24
)

// cadence estimates when upstream will publish next.
type cadence struct {
	mu       sync.RWMutex
	interval time.Duration
	samples  int
}

func newCadence() *cadence {
	return &cadence{interval: nominalInterval}
}

// Observe recomputes the estimate from snapshot instants, oldest first.
//
// The estimator is the median rather than the mean: a single missed publish yields
// one interval of two hours, which would drag a mean well past the truth and stay
// there for a day. The median ignores it outright.
func (c *cadence) Observe(times []time.Time) {
	gaps := make([]time.Duration, 0, len(times))
	for i := 1; i < len(times); i++ {
		g := times[i].Sub(times[i-1])
		if g >= minInterval && g <= maxInterval {
			gaps = append(gaps, g)
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.samples = len(gaps)
	if len(gaps) == 0 {
		c.interval = nominalInterval
		return
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	c.interval = gaps[len(gaps)/2]
}

// Interval is the current estimate of the upstream publish cadence.
func (c *cadence) Interval() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.interval
}

// Measured reports whether the estimate came from observation rather than the
// fallback. Clients are told, so a countdown can present itself as approximate on a
// freshly started instance instead of implying a precision it does not have.
func (c *cadence) Measured() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.samples > 0
}

// NextAfter predicts the next publish following a snapshot.
func (c *cadence) NextAfter(at time.Time) time.Time {
	return at.Add(c.Interval())
}

// PollDelay is how long to wait before the next upstream poll.
//
// A fixed ten-minute poll means the gap between upstream publishing and us capturing
// it is uniform across ten minutes — five on average. That lag is invisible in the
// data but very visible in a countdown, which would hit zero and then sit dead while
// we waited out the rest of a tick.
//
// So the poll rate follows the prediction: slow while nothing is due, fast once the
// window opens. Conditional GETs make the fast polls nearly free — an unchanged feed
// answers 304 with no body — so the cost is a handful of extra round trips per hour
// in exchange for capturing a publish within one closeInterval of it appearing.
//
// The window opens early and never closes, because the prediction is an estimate: if
// upstream is late, staying in fast mode is exactly what finds it soonest.
func (c *cadence) PollDelay(now, at time.Time, idle time.Duration) time.Duration {
	if at.IsZero() {
		return idle
	}
	// closeInterval is both how early the window opens and how often we poll inside
	// it. One minute of slack absorbs the drift many times over.
	const closeInterval = time.Minute

	next := c.NextAfter(at)
	untilWindow := next.Add(-closeInterval).Sub(now)
	if untilWindow <= 0 {
		return closeInterval
	}
	if untilWindow < idle {
		return untilWindow
	}
	return idle
}
