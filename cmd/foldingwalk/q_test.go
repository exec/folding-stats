package main

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestQuantiles(t *testing.T) {
	w := &walker{lat: make([]atomic.Int64, latN+1)}
	// 100 samples: 1..100 ms.
	for i := 1; i <= 100; i++ {
		w.observe(time.Duration(i) * time.Millisecond)
	}
	q := w.quantiles(0.5, 0.9, 0.99)
	for k, want := range []time.Duration{50 * time.Millisecond, 90 * time.Millisecond, 99 * time.Millisecond} {
		// Bucket edges round up by at most one bucket.
		if q[k] < want || q[k] > want+latBucket {
			t.Errorf("q[%d] = %v, want ~%v", k, q[k], want)
		}
	}
	// Empty histogram must not panic or claim a number.
	e := (&walker{lat: make([]atomic.Int64, latN+1)}).quantiles(0.5)
	if e[0] != 0 {
		t.Errorf("empty = %v, want 0", e[0])
	}
	// Anything past the ceiling lands in the overflow bucket, not out of bounds.
	w2 := &walker{lat: make([]atomic.Int64, latN+1)}
	w2.observe(time.Hour)
	if got := w2.quantiles(0.5)[0]; got < latMax {
		t.Errorf("overflow = %v, want >= %v", got, latMax)
	}
}
