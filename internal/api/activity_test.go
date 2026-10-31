package api

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"folding/internal/metrics"
	"folding/internal/model"
	"folding/internal/rank"
)

func activityScaleSnapshot(n int) *Snapshot {
	st := model.NewState()
	st.AppendTeam(32, st.Names.Intern("activity team"))
	for i := 0; i < n; i++ {
		slot := st.AppendMember(st.Names.Intern(fmt.Sprintf("member%06d", i)), 32)
		st.Members[slot].Score = int64(n - i + 1000)
	}
	mw, tw := metrics.New(n), metrics.New(1)
	mw.Push(at(0), nil)
	tw.Push(at(0), nil)
	var stopped, surging []model.Delta
	for i := 0; i < 30; i++ {
		points := int64((i + 1) * 1000)
		stopped = append(stopped, model.Delta{ID: int32(i), DScore: points})
		surging = append(surging, model.Delta{ID: int32(i + 30), DScore: points})
		st.Members[i].Score += points
		st.Members[i+30].Score += points
	}
	mw.Push(at(30), stopped)
	tw.Push(at(30), nil)
	for i := 0; i < 30; i++ {
		slot := st.AppendMember(st.Names.Intern(fmt.Sprintf("new%06d", i)), 32)
		st.Members[slot].Score = int64((i + 1) * 1000)
	}
	mw.Grow(len(st.Members))
	mw.Push(at(60), surging)
	tw.Push(at(60), nil)
	tbl := rank.Build(st, at(60), rank.DefaultConfig)
	tbl.BuildChange24h(st, mw, tw)
	return Build(st, mw, tw, tbl, nil, at(60), at(61), "activity")
}

func TestLargeTeamActivityCache(t *testing.T) {
	snap := activityScaleSnapshot(rosterCacheMin)
	id := int32(32)
	memo := snap.activities
	snap.activities = nil
	want := make(map[int]string)
	for _, limit := range []int{1, 10, 25} {
		var err error
		want[limit], err = snap.mcpTeamActivity(&id, limit)
		if err != nil {
			t.Fatal(err)
		}
	}
	snap.activities = memo

	// Cold concurrent calls must share one cached selection across requested limits.
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			for limit, expected := range want {
				got, err := snap.mcpTeamActivity(&id, limit)
				if err != nil || got != expected {
					t.Errorf("limit %d: cached response differs: %v", limit, err)
				}
			}
		})
	}
	wg.Wait()
	if len(snap.activities) != 1 {
		t.Fatalf("cached %d entries for one team", len(snap.activities))
	}
	summary := snap.teamActivity(id)
	if summary != snap.teamActivity(id) || !summary.newKnown {
		t.Fatal("selection was not reused or lost its arrival baseline")
	}
	for _, list := range []activityList{summary.stopped, summary.surging, summary.joined} {
		if list.total != 30 || len(list.members) != maxActivityMembers || cap(list.members) != maxActivityMembers {
			t.Fatalf("selection lost its full count or retained excess members: %+v", list)
		}
	}
	if summary.stopped.members[0] != 29 || summary.surging.members[0] != 59 || summary.joined.members[0] != rosterCacheMin+29 {
		t.Fatal("largest changes did not lead each section")
	}

	snap.StaleAfter = time.Now().Add(-time.Hour)
	stale, err := snap.mcpTeamActivity(&id, 10)
	if err != nil || !strings.Contains(stale, "The expected update has not arrived") || strings.Contains(want[10], "The expected update has not arrived") {
		t.Fatalf("cached activity did not update its freshness footer: %v", err)
	}

	// A new snapshot owns a fresh cache and reclassifies yesterday's joiners and surges.
	snap.Members.Push(at(85), nil)
	snap.Teams.Push(at(85), nil)
	tbl := rank.Build(snap.State, at(85), rank.DefaultConfig)
	tbl.BuildChange24h(snap.State, snap.Members, snap.Teams)
	next := Build(snap.State, snap.Members, snap.Teams, tbl, nil, at(85), at(86), "next")
	if len(next.activities) != 0 {
		t.Fatal("new snapshot inherited the activity cache")
	}
	updated := next.teamActivity(id)
	if updated == summary || updated.joined.total != 0 || updated.surging.total != 0 || updated.stopped.total != 60 {
		t.Fatalf("new snapshot reused old classifications: %+v", updated)
	}

	for i := int32(0); len(next.activities) < rosterCacheMax; i++ {
		next.activities[-i-1] = &activityEntry{}
	}
	delete(next.activities, id)
	next.activities[-1000] = &activityEntry{}
	if got := next.teamActivity(id); got == updated || got.stopped.total != 60 || len(next.activities) != rosterCacheMax {
		t.Fatal("full cache did not use the uncached fallback")
	}
}

func TestSmallTeamActivityIsNotCached(t *testing.T) {
	snap := activityFixture(t).Current()
	id := int32(32)
	if _, err := snap.mcpTeamActivity(&id, 10); err != nil {
		t.Fatal(err)
	}
	if len(snap.activities) != 0 {
		t.Fatal("small roster populated the activity cache")
	}
}

func BenchmarkLargeTeamActivity(b *testing.B) {
	snap := activityScaleSnapshot(882_940)
	id := int32(32)
	memo := snap.activities
	for _, cached := range []bool{false, true} {
		b.Run(fmt.Sprintf("cached=%v", cached), func(b *testing.B) {
			snap.activities = nil
			if cached {
				snap.activities = memo
			}
			if _, err := snap.mcpTeamActivity(&id, 10); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := snap.mcpTeamActivity(&id, 10); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
