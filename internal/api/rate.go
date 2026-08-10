package api

// How busy the API is, right now.
//
// A rolling sixty seconds in one-second buckets, which is the cheapest structure that
// answers "per second over the last minute" without keeping timestamps. Each request
// bumps the bucket for the current second; reading sums the ring. Sixty int64s and a
// mutex, held for the length of an increment — nothing next to the work of serving the
// request it is counting.
//
// Two things are deliberately not counted.
//
// /v1/status is excluded. It is the freshness probe, and it is what this figure is
// published on — so counting it would mean every page sitting open on the docs polling
// every ten seconds inflated the number it was displaying. A counter that measures its
// own audience is a vanity metric in the most literal sense.
//
// And this counts requests that reach the origin, not requests the world made. The CDN
// answers the nine hottest URLs from cache and those never arrive here. The figure is
// honest about what it is — work this process did — and the page says so, because
// "requests per second" with an unstated denominator is the kind of number that gets
// quoted back at you as something it never measured.

import (
	"sync"
	"time"
)

// rateWindow is how far back the rate looks. Sixty seconds is long enough that a
// single burst does not dominate it and short enough that it still reads as "now".
const rateWindow = 60

type rateCounter struct {
	mu      sync.Mutex
	buckets [rateWindow]int64
	// last is the unix second the newest bucket belongs to. Everything between it and
	// now is idle time that has to be cleared before it is counted again — the ring
	// holds a minute, so without that a quiet hour would be summed as a busy minute.
	last int64
}

// add records one request.
func (c *rateCounter) add(now time.Time) {
	sec := now.Unix()
	c.mu.Lock()
	c.advance(sec)
	c.buckets[sec%rateWindow]++
	c.mu.Unlock()
}

// advance clears the buckets for seconds that elapsed without a request. Called with
// the lock held.
func (c *rateCounter) advance(sec int64) {
	if sec == c.last {
		return
	}
	// Gone round more than once: nothing in the ring is inside the window any more.
	if sec-c.last >= rateWindow || sec < c.last {
		c.buckets = [rateWindow]int64{}
		c.last = sec
		return
	}
	for s := c.last + 1; s <= sec; s++ {
		c.buckets[s%rateWindow] = 0
	}
	c.last = sec
}

// rate returns requests per second over the window, and the count it came from.
//
// Divided by the whole window rather than by however long this process has been up:
// a service thirty seconds old would otherwise report double, and the first minute
// after every deploy would be the busiest the API has ever been.
func (c *rateCounter) rate(now time.Time) (perSec float64, total int64) {
	c.mu.Lock()
	c.advance(now.Unix())
	for _, n := range c.buckets {
		total += n
	}
	c.mu.Unlock()
	return float64(total) / rateWindow, total
}
