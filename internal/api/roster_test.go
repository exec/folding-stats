package api

import (
	"testing"

	"folding/internal/rank"
)

// The roster memo must be invisible: an ordering served from it has to equal the one
// computed fresh, for every column and both filters. A cache that answers differently
// is worse than the 11.7ms scan it replaces.
func TestRosterHeadMatchesUncachedOrdering(t *testing.T) {
	snap := fixture(t).Current()
	slots := snap.Ranks.TeamMembers(32)
	if len(slots) == 0 {
		t.Skip("fixture team has no roster")
	}

	for _, k := range []rank.SortKey{rank.PerDay, rank.Today, rank.Last24h, rank.WUs} {
		for _, active := range []bool{false, true} {
			keep := func(slot int32) bool { return !active || snap.Members.Last7d(slot) > 0 }

			// Uncached: a snapshot with no memo takes the build path every time.
			bare := *snap
			bare.rosters = nil
			want := bare.rosterHead(slots, k, active, keep)

			// Cached, twice: the second call must come from the memo and still agree.
			got1 := snap.rosterHead(slots, k, active, keep)
			got2 := snap.rosterHead(slots, k, active, keep)

			for i, name := range []string{"first", "second"} {
				got := [][]int32{got1, got2}[i]
				if len(got) != len(want) {
					t.Fatalf("%v active=%v %s: %d slots, want %d", k, active, name, len(got), len(want))
				}
				for j := range want {
					if got[j] != want[j] {
						t.Fatalf("%v active=%v %s: slot %d = %d, want %d", k, active, name, j, got[j], want[j])
					}
				}
			}
		}
	}
}

// Keys must not collide: the same roster under a different column or filter is a
// different answer, and serving one for the other would be silent and wrong.
func TestRosterMemoKeysDoNotCollide(t *testing.T) {
	snap := fixture(t).Current()
	slots := snap.Ranks.TeamMembers(32)
	if len(slots) < 2 {
		t.Skip("fixture team too small")
	}
	all := func(int32) bool { return true }
	onlyActive := func(slot int32) bool { return snap.Members.Last7d(slot) > 0 }

	a := snap.rosterHead(slots, rank.Last24h, false, all)
	b := snap.rosterHead(slots, rank.WUs, false, all)
	c := snap.rosterHead(slots, rank.Last24h, true, onlyActive)

	if len(a) > 0 && len(b) > 0 && &a[0] == &b[0] {
		t.Error("two sort keys share one cached slice")
	}
	if len(a) > 0 && len(c) > 0 && &a[0] == &c[0] {
		t.Error("active-only shares the unfiltered cached slice")
	}
}
