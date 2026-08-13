package api

import "testing"

func TestTieAtFindsTheRunAroundARank(t *testing.T) {
	// Descending, with a wide plateau — the shape of the real leaderboard, where most
	// entities sit in one enormous run of equal scores.
	scores := []int64{100, 90, 90, 90, 50, 50, 7, 7, 7, 7, 7}
	at := func(i int) int64 { return scores[i] }
	n := len(scores)
	for _, tc := range []struct{ rank, want int32 }{
		{1, 1}, // unique leader
		{2, 3}, // first of a three-way tie
		{3, 3}, // middle of it
		{4, 3}, // last of it
		{5, 2},
		{7, 5},  // first of the plateau
		{11, 5}, // last of the plateau
	} {
		if got := tieAt(n, tc.rank, at); got != tc.want {
			t.Errorf("tieAt(rank %d) = %d, want %d", tc.rank, got, tc.want)
		}
	}
	// Out of range must not panic or invent a tie.
	if got := tieAt(n, 0, at); got != 0 {
		t.Errorf("rank 0 = %d, want 0", got)
	}
	if got := tieAt(n, 99, at); got != 0 {
		t.Errorf("rank past the end = %d, want 0", got)
	}
	if got := tieAt(0, 1, func(int) int64 { return 0 }); got != 0 {
		t.Errorf("empty order = %d, want 0", got)
	}
}
