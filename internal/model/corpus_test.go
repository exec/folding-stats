package model

import (
	"os"
	"runtime"
	"testing"
	"time"

	"folding/internal/parse"
)

func loadCorpus(t testing.TB) ([]parse.TeamRow, []parse.UserRow) {
	t.Helper()
	dir := os.Getenv("FOLDING_CORPUS_DIR")
	if dir == "" {
		t.Skip("FOLDING_CORPUS_DIR not set; skipping real-corpus test")
	}
	tf, err := os.Open(dir + "/team.txt")
	if err != nil {
		t.Skip("corpus not available")
	}
	defer tf.Close()
	var teams []parse.TeamRow
	ts := parse.NewTeamScanner(tf)
	for ts.Scan() {
		teams = append(teams, ts.Row())
	}

	uf, err := os.Open(dir + "/user.txt")
	if err != nil {
		t.Skip("corpus not available")
	}
	defer uf.Close()
	var us []parse.UserRow
	usc := parse.NewUserScanner(uf)
	for usc.Scan() {
		us = append(us, usc.Row())
	}
	return teams, us
}

// TestCorpusMemoryBudget applies the real corpus and reports live heap. The whole
// architecture assumes the hot set fits comfortably on a 4 GB box, so this is the
// number that decides whether the in-memory design holds.
func TestCorpusMemoryBudget(t *testing.T) {
	teams, users := loadCorpus(t)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	s := NewState()
	start := time.Now()
	c := s.Apply(time.Now().UTC(), teams, users)
	elapsed := time.Since(start)

	// Drop the parsed input so we measure only retained state.
	teams, users = nil, nil
	_ = teams
	_ = users
	runtime.GC()
	runtime.ReadMemStats(&after)

	heapMB := float64(after.HeapAlloc) / (1 << 20)
	t.Logf("apply: %v   members=%d teams=%d names=%d",
		elapsed.Round(time.Millisecond), len(s.Members), len(s.Teams), s.Names.Len())
	t.Logf("first cycle: new_members=%d new_teams=%d deltas=%d (expected 0 deltas)",
		len(c.NewMembers), len(c.NewTeams), len(c.MemberDeltas))
	t.Logf("live heap after apply: %.1f MB", heapMB)

	if len(s.Members) < 2_700_000 {
		t.Errorf("members = %d, want >= 2.7M", len(s.Members))
	}
	if len(c.MemberDeltas) != 0 {
		t.Errorf("first cycle produced %d deltas, want 0", len(c.MemberDeltas))
	}
	// Budget is ~455 MB by design; fail well before the 4 GB box is at risk so a
	// regression in the arena or index shows up here rather than in production.
	if heapMB > 900 {
		t.Errorf("live heap %.1f MB exceeds 900 MB budget", heapMB)
	}
}

// TestCorpusSecondCycleIsSparse verifies the sparsity assumption underpinning the
// storage and sliding-window budgets: re-applying an identical snapshot must
// produce no deltas at all.
func TestCorpusSecondCycleIsSparse(t *testing.T) {
	teams, users := loadCorpus(t)
	s := NewState()
	now := time.Now().UTC()
	s.Apply(now, teams, users)
	c := s.Apply(now.Add(time.Hour), teams, users)

	if len(c.MemberDeltas) != 0 || len(c.TeamDeltas) != 0 {
		t.Errorf("identical snapshot produced %d member and %d team deltas, want 0",
			len(c.MemberDeltas), len(c.TeamDeltas))
	}
	if len(c.NewMembers) != 0 {
		t.Errorf("NewMembers = %d on repeat apply, want 0", len(c.NewMembers))
	}
	if c.Regressions != 0 {
		t.Errorf("Regressions = %d, want 0", c.Regressions)
	}
}
