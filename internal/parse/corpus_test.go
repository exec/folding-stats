package parse

import (
	"bytes"
	"os"
	"strconv"
	"testing"
)

// Corpus tests run against a real archived snapshot when one is available. They are
// skipped otherwise so the suite still runs on a clean checkout.
//
// Set FOLDING_CORPUS_DIR to a directory containing the uncompressed feeds as
// team.txt and user.txt.
func corpusPath(t *testing.T, name string) string {
	t.Helper()
	dir := os.Getenv("FOLDING_CORPUS_DIR")
	if dir == "" {
		t.Skip("FOLDING_CORPUS_DIR not set; skipping real-corpus test")
	}
	p := dir + "/" + name
	if _, err := os.Stat(p); err != nil {
		t.Skipf("corpus file %s not available", p)
	}
	return p
}

// TestCorpusInvariants parses the real feeds and asserts the properties the whole
// backend depends on. These are the numbers established while reverse-engineering
// the format; a regression here means the parser has started losing or inventing
// records.
func TestCorpusInvariants(t *testing.T) {
	tf, err := os.Open(corpusPath(t, "team.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer tf.Close()

	teamScore := map[int32]int64{}
	ts := NewTeamScanner(tf)
	for ts.Scan() {
		r := ts.Row()
		if _, dup := teamScore[r.ID]; dup {
			t.Errorf("duplicate team id %d", r.ID)
		}
		teamScore[r.ID] = r.Score
	}
	if err := ts.Err(); err != nil {
		t.Fatalf("team scan: %v", err)
	}
	tstats := ts.Stats()

	// 129,948 teams in the 2026-08-02 snapshot. The corpus grows over time, so
	// assert a floor rather than an exact count.
	if tstats.Rows < 129_000 {
		t.Errorf("team rows = %d, want >= 129,000", tstats.Rows)
	}
	// Every physical line must resolve: the 7 "malformed" lines in this feed are
	// really names containing newlines, and the parser is supposed to repair them.
	if tstats.Malformed != 0 {
		t.Errorf("team malformed = %d, want 0 (all lines recoverable)", tstats.Malformed)
	}

	uf, err := os.Open(corpusPath(t, "user.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer uf.Close()

	summed := map[int32]int64{}
	pairs := map[string]int{}
	names := map[string]struct{}{}
	us := NewUserScanner(uf)
	for us.Scan() {
		r := us.Row()
		summed[r.TeamID] += r.Score
		pairs[r.Name+"\x00"+itoa32(r.TeamID)]++
		names[r.Name] = struct{}{}
	}
	if err := us.Err(); err != nil {
		t.Fatalf("user scan: %v", err)
	}
	ustats := us.Stats()

	if ustats.Rows < 2_700_000 {
		t.Errorf("user rows = %d, want >= 2,700,000", ustats.Rows)
	}
	if ustats.Malformed != 0 {
		t.Errorf("user malformed = %d, want 0 (all lines recoverable)", ustats.Malformed)
	}

	// Duplicate (name, team) pairs are real and must be preserved, not silently
	// deduplicated — summing them is what reconciles against team totals.
	dupes := 0
	for _, n := range pairs {
		if n > 1 {
			dupes++
		}
	}
	if dupes == 0 {
		t.Error("no duplicate (name,team) pairs found; expected ~6,984 in this corpus")
	}
	t.Logf("teams=%d users=%d distinct_names=%d dup_pairs=%d",
		tstats.Rows, ustats.Rows, len(names), dupes)

	// The load-bearing invariant: summing user rows per team must reproduce the
	// team feed's own score for the overwhelming majority of teams. A parser bug
	// that dropped or mis-split rows would show up here immediately.
	//
	// The two feeds are NOT an atomic pair: the team file publishes at :29 and the
	// user file at :30, so a point banked in that 60-second window lands in one and
	// not the other. Discrepancies therefore occur in both directions and are
	// expected — what matters is that they stay tiny.
	var exact, under, over int
	var maxOver int64
	for id, want := range teamScore {
		got, ok := summed[id]
		if !ok {
			continue
		}
		switch {
		case got == want:
			exact++
		case got < want:
			under++ // team total includes donors absent from the user feed
		default:
			over++
			if d := got - want; d > maxOver {
				maxOver = d
			}
		}
	}
	total := exact + under + over
	if total == 0 {
		t.Fatal("no teams matched between feeds")
	}
	pct := float64(exact) / float64(total) * 100
	t.Logf("checksum: %d/%d exact (%.2f%%), %d under, %d over (max overshoot %d)",
		exact, total, pct, under, over, maxOver)

	if pct < 99.5 {
		t.Errorf("checksum exact rate %.2f%%, want >= 99.5%%", pct)
	}
	// Overshoot comes only from the publish gap, so it must be vanishingly rare.
	// A parser that duplicated or mis-split rows would inflate this sharply.
	if float64(over)/float64(total) > 0.001 {
		t.Errorf("overshoot on %d/%d teams (%.3f%%), want < 0.1%%: suggests duplicated rows",
			over, total, float64(over)/float64(total)*100)
	}
}

func BenchmarkParseUserFeed(b *testing.B) {
	dir := os.Getenv("FOLDING_CORPUS_DIR")
	if dir == "" {
		b.Skip("FOLDING_CORPUS_DIR not set")
	}
	data, err := os.ReadFile(dir + "/user.txt")
	if err != nil {
		b.Skip("corpus not available")
	}
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := NewUserScanner(newBytesReader(data))
		n := 0
		for s.Scan() {
			n++
		}
		if n < 2_700_000 {
			b.Fatalf("parsed only %d rows", n)
		}
	}
}

func newBytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

func itoa32(v int32) string { return strconv.FormatInt(int64(v), 10) }
