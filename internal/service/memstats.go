package service

import (
	"log/slog"
	"runtime"
)

// Memory reporting exists because resident size cannot answer the question we have.
//
// RSS conflates two different things: the bytes actually holding live data, and the
// bytes Go has taken from the OS and not given back. A process whose live set is flat
// at 700 MB and one that is leaking both show a rising RSS, and from outside they are
// indistinguishable. Three days of sampling `/proc` told us the resident set had grown
// from 660 MB to over 1 GB and peaked at 2 GB, and could not tell us whether that
// mattered.
//
// So the figures logged here are chosen to separate those:
//
//   - live is HeapAlloc: reachable objects. This is the number that grows if
//     something is genuinely accumulating.
//   - heap is HeapSys minus HeapReleased: what Go currently holds from the OS. It
//     tracks the high-water mark of live and comes down only slowly, which is
//     ordinary behaviour and not a fault.
//   - The gap between them is headroom Go is sitting on, and explains most of the
//     difference between live data and what `ps` reports.
//
// ReadMemStats briefly stops the world. At four calls an hour against a publish that
// already takes seconds, that is not a cost worth avoiding.
type memSample struct {
	liveMB     uint64 // HeapAlloc: reachable right now
	heapMB     uint64 // HeapSys - HeapReleased: held from the OS
	releasedMB uint64
	numGC      uint32
}

func readMem() memSample {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return memSample{
		liveMB:     m.HeapAlloc >> 20,
		heapMB:     (m.HeapSys - m.HeapReleased) >> 20,
		releasedMB: m.HeapReleased >> 20,
		numGC:      m.NumGC,
	}
}

// logMemory reports the publish's memory profile.
//
// The three points bracket the moment that matters. A new ranked table is built while
// the old one is still being served, so both are reachable at once — `built` is the
// only place that doubling is visible, and it is where a peak near the memory limit
// would come from. Sampling once after the fact would miss it entirely and report a
// process that looks comfortable.
func logMemory(log *slog.Logger, before, built, after memSample, donors, members int) {
	log.Info("memory",
		"live_before_mb", before.liveMB,
		"live_at_peak_mb", built.liveMB,
		"live_after_mb", after.liveMB,
		"table_cost_mb", int64(built.liveMB)-int64(before.liveMB),
		"heap_held_mb", after.heapMB,
		"released_mb", after.releasedMB,
		"gc_runs", after.numGC-before.numGC,
		"donors", donors,
		"members", members,
	)
}
