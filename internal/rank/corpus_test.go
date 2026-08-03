package rank

import (
	"os"
	"runtime"
	"testing"
	"time"

	"folding/internal/model"
	"folding/internal/parse"
)

func loadState(t testing.TB) *model.State {
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
	var users []parse.UserRow
	us := parse.NewUserScanner(uf)
	for us.Scan() {
		users = append(users, us.Row())
	}
	st := model.NewState()
	st.Apply(time.Now().UTC(), teams, users)
	return st
}

func TestCorpusRanking(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	st := loadState(t)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	start := time.Now()
	tbl := Build(st, time.Now().UTC(), DefaultConfig)
	elapsed := time.Since(start)

	runtime.GC()
	runtime.ReadMemStats(&after)

	t.Logf("build=%v  members=%d teams=%d donors=%d  heap=%.1f MB",
		elapsed.Round(time.Millisecond), len(tbl.MemberOrder), len(tbl.TeamOrder),
		len(tbl.Donors), float64(after.HeapAlloc)/(1<<20))

	// Ranking runs once per hourly cycle alongside ingest; it must stay a small
	// fraction of the budget.
	if elapsed > 10*time.Second {
		t.Errorf("Build took %v, want < 10s", elapsed)
	}

	// Ordering must be monotonically non-increasing by score.
	for i := 1; i < len(tbl.MemberOrder); i++ {
		a, b := tbl.MemberOrder[i-1], tbl.MemberOrder[i]
		if st.Members[a].Score < st.Members[b].Score {
			t.Fatalf("member order not descending at %d: %d < %d",
				i, st.Members[a].Score, st.Members[b].Score)
		}
	}
	for i := 1; i < len(tbl.TeamOrder); i++ {
		a, b := tbl.TeamOrder[i-1], tbl.TeamOrder[i]
		if st.Teams[a].Score < st.Teams[b].Score {
			t.Fatalf("team order not descending at %d", i)
		}
	}
	// Ranks are a permutation: every member gets exactly one, 1..N.
	seen := make([]bool, len(st.Members)+1)
	for _, r := range tbl.MemberRank {
		if r < 1 || int(r) > len(st.Members) || seen[r] {
			t.Fatalf("rank %d is duplicated or out of range", r)
		}
		seen[r] = true
	}

	// Donor aggregation must conserve points exactly.
	var memberTotal, donorTotal int64
	for _, m := range st.Members {
		memberTotal += m.Score
	}
	for _, d := range tbl.Donors {
		donorTotal += d.Score
	}
	if memberTotal != donorTotal {
		t.Errorf("donor aggregation lost points: members=%d donors=%d", memberTotal, donorTotal)
	}

	// The pseudo-identity flag should catch the handful of shared default names and
	// nothing else.
	var flagged int
	var worst Donor
	for _, d := range tbl.Donors {
		if d.LikelyNotAPerson {
			flagged++
			if d.TeamCount > worst.TeamCount {
				worst = d
			}
		}
	}
	t.Logf("flagged pseudo-identities: %d (worst: %q on %d teams, %d points)",
		flagged, st.Names.Name(worst.NameID), worst.TeamCount, worst.Score)
	if flagged == 0 {
		t.Error("no pseudo-identities flagged; expected PS3/Anonymous and similar")
	}
	if pct := float64(flagged) / float64(len(tbl.Donors)) * 100; pct > 0.5 {
		t.Errorf("flagged %.2f%% of donors, want < 0.5%% — threshold too aggressive", pct)
	}
}
