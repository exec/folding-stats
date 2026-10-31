package api

import (
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"folding/internal/rank"
)

func TestRosterLifetimePageUsesStoredOrder(t *testing.T) {
	const n = 882_940
	slots := make([]int32, n)
	for i := range slots {
		slots[i] = int32(n - i - 1)
	}
	snap := &Snapshot{}
	for _, lo := range []int{0, n / 2, n - 100} {
		got := snap.orderRoster(slots, rank.Lifetime, false, lo, lo+100)
		if !slices.Equal(got, slots[lo:lo+100]) {
			t.Fatalf("page starting at %d has the wrong members", lo)
		}
		if &got[0] != &slots[lo] {
			t.Fatalf("page starting at %d copied the stored order", lo)
		}
	}
}

func TestEmptyRosterPageNeedsNoState(t *testing.T) {
	// Empty pages must not inspect members or populate an ordering cache.
	snap := &Snapshot{}
	slots := []int32{0, 1, 2}
	for _, key := range []rank.SortKey{
		rank.Lifetime, rank.PerDay, rank.Today, rank.ThisWeek,
		rank.ThisMonth, rank.Last24h, rank.WUs,
	} {
		for _, active := range []bool{false, true} {
			for _, lo := range []int{0, len(slots)} {
				if got := snap.orderRoster(slots, key, active, lo, lo); len(got) != 0 {
					t.Fatalf("%s active=%v empty page at %d returned %v", key, active, lo, got)
				}
			}
		}
	}
}

func TestActiveLifetimePagesReuseFilteredRoster(t *testing.T) {
	snap := rosterCacheSnapshot()
	slots := snap.Ranks.TeamMembers(32)
	var want []int32
	for _, slot := range slots {
		if snap.Members.Last7d(slot) > 0 {
			want = append(want, slot)
		}
	}
	if len(want) < 20 {
		t.Fatal("fixture needs several active pages")
	}
	key := rosterKey{team: slots[0], n: int32(len(slots)), sort: rank.Lifetime, active: true}
	for _, lo := range []int{0, len(want) / 2, len(want) - 10} {
		got := snap.orderRoster(slots, rank.Lifetime, true, lo, lo+10)
		if !slices.Equal(got, want[lo:lo+10]) {
			t.Fatalf("active page %d = %v, want %v", lo, got, want[lo:lo+10])
		}
		entry := snap.rosters[key]
		if entry == nil || len(entry.head) != len(want) || &got[0] != &entry.head[lo] {
			t.Fatal("active lifetime page did not reuse the filtered order")
		}
	}
	if got := snap.orderRoster(slots, rank.Lifetime, true, len(want), len(want)+10); len(got) != 0 {
		t.Fatal("page past the filtered order must stay empty")
	}
	// The fallback at the cache cap must still return the same page.
	snap.rosters = make(map[rosterKey]*rosterEntry, rosterCacheMax)
	for i := range rosterCacheMax {
		snap.rosters[rosterKey{team: int32(i), n: -1}] = &rosterEntry{}
	}
	if got := snap.orderRoster(slots, rank.Lifetime, true, len(want)-10, len(want)); !slices.Equal(got, want[len(want)-10:]) {
		t.Fatal("cache-cap fallback changed the active lifetime page")
	}
	// Small teams retain the direct scan.
	snap.rosters = make(map[rosterKey]*rosterEntry)
	if got := snap.orderRoster(want, rank.Lifetime, true, 0, 10); !slices.Equal(got, want[:10]) || len(snap.rosters) != 0 {
		t.Fatal("small active roster changed ordering or populated the cache")
	}
}

func BenchmarkActiveRosterLifetimePaging(b *testing.B) {
	snap := activityScaleSnapshot(882_940)
	// Lifetime leaders can be dormant; put the recent producers at the tail.
	for slot := range snap.State.Members {
		if snap.Members.Last7d(int32(slot)) == 0 {
			snap.State.Members[slot].Score += 2_000_000
		}
	}
	snap.Ranks = rank.Build(snap.State, snap.At, rank.DefaultConfig)
	slots := snap.Ranks.TeamMembers(32)
	_, active := snap.TeamMemberCounts(32)
	lo, hi := int(active)-10, int(active)
	memo := snap.rosters
	for _, cached := range []bool{false, true} {
		b.Run(map[bool]string{false: "uncached", true: "cached"}[cached], func(b *testing.B) {
			snap.rosters = nil
			if cached {
				snap.rosters = memo
			}
			snap.orderRoster(slots, rank.Lifetime, true, lo, hi)
			b.ReportAllocs()
			for b.Loop() {
				if got := snap.orderRoster(slots, rank.Lifetime, true, lo, hi); len(got) != 10 {
					b.Fatalf("got %d members, want 10", len(got))
				}
			}
		})
	}
}

func BenchmarkRosterLifetimePaging(b *testing.B) {
	const n = 882_940
	slots := make([]int32, n)
	for i := range slots {
		slots[i] = int32(i)
	}
	snap := &Snapshot{}
	for _, page := range []struct {
		name string
		lo   int
	}{{"first", 0}, {"last", n - 100}} {
		b.Run(page.name, func(b *testing.B) {
			b.ReportAllocs()
			var got []int32
			for b.Loop() {
				got = snap.orderRoster(slots, rank.Lifetime, false, page.lo, page.lo+100)
			}
			if len(got) != 100 {
				b.Fatalf("got %d members, want 100", len(got))
			}
		})
	}
}

// The roster memo must be invisible: an ordering served from it has to equal the one
// computed fresh, for every column and both filters. A cache that answers differently
// is worse than the 11.7ms scan it replaces.
func TestRosterHeadMatchesUncachedOrdering(t *testing.T) {
	snap := rosterCacheSnapshot()
	slots := snap.Ranks.TeamMembers(32)

	for _, k := range []rank.SortKey{rank.PerDay, rank.Today, rank.ThisWeek, rank.ThisMonth, rank.Last24h, rank.WUs} {
		for _, active := range []bool{false, true} {
			keep := func(slot int32) bool { return !active || snap.Members.Last7d(slot) > 0 }

			// Uncached: a snapshot with no memo takes the build path every time.
			memo := snap.rosters
			snap.rosters = nil
			want := snap.rosterHead(slots, k, active, keep)
			snap.rosters = memo
			if len(want) == 0 {
				t.Fatalf("%v active=%v: fixture has no members to rank", k, active)
			}

			// Cached, twice: the second call must come from the memo and still agree.
			got1 := snap.rosterHead(slots, k, active, keep)
			got2 := snap.rosterHead(slots, k, active, keep)

			if !slices.Equal(got1, want) || !slices.Equal(got2, want) {
				t.Fatalf("%v active=%v: cached ordering differs", k, active)
			}
			key := rosterKey{team: slots[0], n: int32(len(slots)), sort: k, active: active}
			entry := snap.rosters[key]
			if entry == nil || &entry.head[0] != &got1[0] || &got1[0] != &got2[0] {
				t.Fatalf("%v active=%v: cached slice was not reused", k, active)
			}
		}
	}
}

// Keys must not collide: the same roster under a different column or filter is a
// different answer, and serving one for the other would be silent and wrong.
func TestRosterMemoKeysDoNotCollide(t *testing.T) {
	snap := rosterCacheSnapshot()
	slots := snap.Ranks.TeamMembers(32)
	all := func(int32) bool { return true }
	onlyActive := func(slot int32) bool { return snap.Members.Last7d(slot) > 0 }

	a := snap.rosterHead(slots, rank.Last24h, false, all)
	b := snap.rosterHead(slots, rank.WUs, false, all)
	c := snap.rosterHead(slots, rank.Last24h, true, onlyActive)
	d := snap.rosterHead(slots, rank.WUs, true, onlyActive)

	if len(a) == 0 || len(b) == 0 || len(c) == 0 || len(d) == 0 {
		t.Fatal("fixture must populate every cached ordering")
	}
	if len(snap.rosters) != 4 {
		t.Fatalf("cached %d keys, want 4", len(snap.rosters))
	}
	if &a[0] == &b[0] || slices.Equal(a, b) {
		t.Error("two sort keys share one cached slice")
	}
	if &a[0] == &c[0] || slices.Equal(b, d) {
		t.Error("active-only shares the unfiltered cached slice")
	}
}

func rosterCacheSnapshot() *Snapshot {
	snap := activityScaleSnapshot(rosterCacheMin)
	snap.State.Members[0].WUs = 20
	snap.State.Members[61].WUs = 40 // An idle member distinguishes all from active-only.
	snap.MemberMonth = make([]int64, len(snap.State.Members))
	snap.MemberMonth[30], snap.MemberMonth[61] = 100, 200
	return snap
}

func TestRosterMemoBuildsOnceUnderConcurrentReads(t *testing.T) {
	snap := rosterCacheSnapshot()
	slots := snap.Ranks.TeamMembers(32)
	var scanned atomic.Int64
	keep := func(int32) bool {
		scanned.Add(1)
		return true
	}
	results := make([][]int32, 16)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() { results[i] = snap.rosterHead(slots, rank.ThisWeek, false, keep) })
	}
	wg.Wait()
	if scanned.Load() != int64(len(slots)) {
		t.Fatalf("scanned %d members, want one pass over %d", scanned.Load(), len(slots))
	}
	if len(results[0]) == 0 || len(snap.rosters) != 1 {
		t.Fatal("concurrent reads did not populate one nonempty cache entry")
	}
	for _, got := range results[1:] {
		if !slices.Equal(got, results[0]) || &got[0] != &results[0][0] {
			t.Fatal("concurrent reads did not share the cached ordering")
		}
	}
}
