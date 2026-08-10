package api

import (
	"sync"
	"testing"
	"time"
)

func TestRateCounterMeasuresTheLastMinuteOnly(t *testing.T) {
	base := time.Unix(1786000000, 0)
	var c rateCounter

	// Thirty requests spread over thirty seconds is half a request per second, and
	// stays that whether they are read from the same second or a later one.
	for i := range 30 {
		c.add(base.Add(time.Duration(i) * time.Second))
	}
	if got, total := c.rate(base.Add(29 * time.Second)); got != 0.5 || total != 30 {
		t.Errorf("rate = %v over %d, want 0.5 over 30", got, total)
	}

	// A quiet minute empties it. Without clearing skipped buckets the ring would
	// still be holding those thirty and reporting an hour-old burst as current.
	if got, total := c.rate(base.Add(10 * time.Minute)); got != 0 || total != 0 {
		t.Errorf("after ten idle minutes rate = %v over %d, want 0", got, total)
	}

	// Requests aging out one at a time, not all at once.
	var d rateCounter
	for i := range 60 {
		d.add(base.Add(time.Duration(i) * time.Second))
	}
	if _, total := d.rate(base.Add(59 * time.Second)); total != 60 {
		t.Errorf("full window holds %d, want 60", total)
	}
	// Thirty seconds later, the first thirty have expired and nothing was added.
	if _, total := d.rate(base.Add(89 * time.Second)); total != 30 {
		t.Errorf("half a window later %d remain, want 30", total)
	}

	// The divisor is the window, never the uptime — otherwise the first seconds after
	// a deploy are always the busiest the service has ever been.
	var e rateCounter
	e.add(base)
	if got, _ := e.rate(base); got != 1.0/60 {
		t.Errorf("one request reads as %v/s, want 1/60", got)
	}
}

func TestRateCounterSurvivesConcurrentUse(t *testing.T) {
	// It sits on the request path, so it is hit from every serving goroutine at once.
	var c rateCounter
	now := time.Unix(1786000000, 0)
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				c.add(now)
			}
		}()
	}
	wg.Wait()
	if _, total := c.rate(now); total != 10000 {
		t.Errorf("counted %d of 10000 — increments were lost", total)
	}
}

func TestRateCounterIgnoresAClockGoingBackwards(t *testing.T) {
	// NTP steps happen. Going backwards must not leave stale buckets to be counted
	// again as the clock catches up.
	var c rateCounter
	base := time.Unix(1786000000, 0)
	for i := range 60 {
		c.add(base.Add(time.Duration(i) * time.Second))
	}
	if _, total := c.rate(base.Add(-5 * time.Minute)); total != 0 {
		t.Errorf("a backwards clock left %d counted, want the window cleared", total)
	}
}
