package metrics

import (
	"runtime"
	"testing"
	"time"

	"folding/internal/model"
)

// TestWindowMemoryAtCorpusScale exercises the window with a realistic entity count
// and active fraction. This design exists purely to avoid the 522 MB that keeping
// full snapshots would cost, so the memory figure is the point of the exercise.
func TestWindowMemoryAtCorpusScale(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	const (
		entities = 2_710_000
		active   = 27_000 // ~1%, matching the observed rate
		cycles   = 24 * 7 // a full week at hourly publication
	)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	w := New(entities)
	start := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	rng := uint64(99)

	pushStart := time.Now()
	for i := 0; i < cycles; i++ {
		ds := make([]model.Delta, active)
		for j := range ds {
			rng = rng*6364136223846793005 + 1442695040888963407
			ds[j] = model.Delta{ID: int32((rng >> 33) % entities), DScore: 1000}
		}
		w.Push(start.Add(time.Duration(i)*time.Hour), ds)
	}
	pushElapsed := time.Since(pushStart)

	runtime.GC()
	runtime.ReadMemStats(&after)
	heapMB := float64(after.HeapAlloc) / (1 << 20)

	t.Logf("entities=%d cycles_retained=%d  push total=%v (%.2f ms/cycle)  heap=%.1f MB",
		entities, w.Cycles(), pushElapsed.Round(time.Millisecond),
		float64(pushElapsed.Microseconds())/float64(cycles)/1000, heapMB)

	if w.Cycles() > cycles+1 {
		t.Errorf("retained %d cycles, want <= %d", w.Cycles(), cycles+1)
	}
	// Keeping 24 full snapshots of int64 scores would be 522 MB for one field
	// alone; the sparse ring plus five accumulator arrays must stay far below that.
	if heapMB > 350 {
		t.Errorf("heap %.1f MB exceeds 350 MB budget", heapMB)
	}
}
