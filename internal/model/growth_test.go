package model

import "testing"

func TestScratchGrowthReusesCapacity(t *testing.T) {
	ints32, ints64, flags := grow32(nil, 100), grow64(nil, 100), growBool(nil, 100)
	ints64[0], flags[0] = 42, true
	p32, p64, pb := &ints32[0], &ints64[0], &flags[0]
	for n := 101; n <= 112; n++ {
		ints32, ints64, flags = grow32(ints32, n), grow64(ints64, n), growBool(flags, n)
		if &ints32[0] != p32 || &ints64[0] != p64 || &flags[0] != pb {
			t.Fatalf("scratch buffers reallocated at %d entities", n)
		}
		if ints64[0] != 42 || !flags[0] || ints64[n-1] != 0 || flags[n-1] {
			t.Fatal("growth lost existing contents or exposed nonzero new entries")
		}
	}
}

// One day's hourly growth, gaining one member per cycle. The first allocation
// is included; following cycles should reuse its headroom.
func BenchmarkScratchGrowth(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		var rows []int32
		var score, wus []int64
		var touched []bool
		for n := 2_700_000; n < 2_700_024; n++ {
			rows = grow32(rows, n)
			score = grow64(score, n)
			wus = grow64(wus, n)
			touched = growBool(touched, n)
		}
	}
}
