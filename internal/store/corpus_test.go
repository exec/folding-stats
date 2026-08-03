package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"folding/internal/model"
	"folding/internal/parse"
)

func loadCorpus(t testing.TB) ([]parse.TeamRow, []parse.UserRow) {
	t.Helper()
	dir := os.Getenv("FOLDING_CORPUS_DIR")
	if dir == "" {
		t.Skip("FOLDING_CORPUS_DIR not set")
	}
	tf, err := os.Open(dir + "/team.txt")
	if err != nil {
		t.Skip("corpus not available")
	}
	defer tf.Close()
	var tr []parse.TeamRow
	ts := parse.NewTeamScanner(tf)
	for ts.Scan() {
		tr = append(tr, ts.Row())
	}
	uf, err := os.Open(dir + "/user.txt")
	if err != nil {
		t.Skip("corpus not available")
	}
	defer uf.Close()
	var ur []parse.UserRow
	us := parse.NewUserScanner(uf)
	for us.Scan() {
		ur = append(ur, us.Row())
	}
	return tr, ur
}

// TestCorpusFirstIngest measures the cold-start write: 2.7M identity rows through a
// pure-Go SQLite driver. This happens once, but it must be minutes rather than
// hours, and the resulting database has to be a sane size.
func TestCorpusFirstIngest(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	teams, users := loadCorpus(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "history.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	st := model.NewState()
	ctx := context.Background()
	now := time.Date(2026, 8, 2, 21, 0, 0, 0, time.UTC)

	start := time.Now()
	c := st.Apply(now, teams, users)
	applied := time.Since(start)

	start = time.Now()
	if err := s.WriteCycle(ctx, st, c, CycleMeta{
		TeamSnapshotAt: now, UserSnapshotAt: now,
		TeamRows: len(teams), UserRows: len(users),
	}); err != nil {
		t.Fatalf("WriteCycle: %v", err)
	}
	wrote := time.Since(start)

	fi, _ := os.Stat(path)
	t.Logf("apply=%v  write=%v  members=%d names=%d  db=%.1f MB",
		applied.Round(time.Millisecond), wrote.Round(time.Millisecond),
		len(st.Members), st.Names.Len(), float64(fi.Size())/(1<<20))

	// Restart path: identity must come back byte-identical, at scale.
	start = time.Now()
	restored := model.NewState()
	if err := s.LoadIdentity(ctx, restored); err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	t.Logf("reload identity: %v", time.Since(start).Round(time.Millisecond))

	if len(restored.Members) != len(st.Members) {
		t.Fatalf("restored %d members, want %d", len(restored.Members), len(st.Members))
	}
	if restored.Names.Len() != st.Names.Len() {
		t.Fatalf("restored %d names, want %d", restored.Names.Len(), st.Names.Len())
	}
	// Spot-check that slots still resolve to the same donor after a round trip.
	for _, slot := range []int{0, 1, len(st.Members) / 2, len(st.Members) - 1} {
		a, b := st.Members[slot], restored.Members[slot]
		if a.NameID != b.NameID || a.TeamID != b.TeamID {
			t.Errorf("slot %d: got (%d,%d) want (%d,%d)", slot, b.NameID, b.TeamID, a.NameID, a.TeamID)
		}
		if got, want := restored.Names.Name(b.NameID), st.Names.Name(a.NameID); got != want {
			t.Errorf("slot %d name = %q, want %q", slot, got, want)
		}
	}
}

// TestCorpusSteadyStateCycle measures a normal hourly cycle, which is what
// determines whether the ingest comfortably fits inside the publish interval.
func TestCorpusSteadyStateCycle(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	teams, users := loadCorpus(t)
	s, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	st := model.NewState()
	now := time.Date(2026, 8, 2, 21, 0, 0, 0, time.UTC)
	c := st.Apply(now, teams, users)
	if err := s.WriteCycle(ctx, st, c, CycleMeta{TeamSnapshotAt: now, UserSnapshotAt: now}); err != nil {
		t.Fatal(err)
	}

	// Simulate an hour's production: 1% of members gain points, matching the
	// observed active fraction.
	next := make([]parse.UserRow, len(users))
	copy(next, users)
	for i := 0; i < len(next); i += 100 {
		next[i].Score += 50_000
		next[i].WUs += 3
	}

	start := time.Now()
	c2 := st.Apply(now.Add(time.Hour), teams, next)
	applied := time.Since(start)

	start = time.Now()
	if err := s.WriteCycle(ctx, st, c2, CycleMeta{
		TeamSnapshotAt: now.Add(time.Hour), UserSnapshotAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	wrote := time.Since(start)

	t.Logf("steady cycle: apply=%v write=%v  member_deltas=%d team_deltas=%d",
		applied.Round(time.Millisecond), wrote.Round(time.Millisecond),
		len(c2.MemberDeltas), len(c2.TeamDeltas))

	if len(c2.MemberDeltas) == 0 {
		t.Fatal("no deltas produced")
	}
	// The whole design assumes a cycle lands well inside the hourly publish
	// interval, with room for ranking and metric recomputation on top.
	if total := applied + wrote; total > 60*time.Second {
		t.Errorf("steady-state cycle took %v, want < 60s", total)
	}
}
