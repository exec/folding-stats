package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"folding/internal/model"
	"folding/internal/parse"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func at(h int) time.Time {
	return time.Date(2026, 8, 2, h, 0, 0, 0, time.UTC)
}

func users(rows ...parse.UserRow) []parse.UserRow { return rows }
func teams(rows ...parse.TeamRow) []parse.TeamRow { return rows }

func writeCycle(t *testing.T, s *Store, st *model.State, when time.Time,
	tr []parse.TeamRow, ur []parse.UserRow) *model.Cycle {
	t.Helper()
	c := st.Apply(when, tr, ur)
	meta := CycleMeta{TeamSnapshotAt: when, UserSnapshotAt: when,
		TeamRows: len(tr), UserRows: len(ur)}
	if err := s.WriteCycle(context.Background(), st, c, meta); err != nil {
		t.Fatalf("WriteCycle: %v", err)
	}
	return c
}

func TestIdentitySurvivesRestart(t *testing.T) {
	// Stored deltas reference entities by dense slot number. If a restart
	// reassigned those slots differently, history would silently reattribute to the
	// wrong donor — the worst possible failure, because nothing would error.
	s := open(t)
	st := model.NewState()
	writeCycle(t, s, st, at(1),
		teams(parse.TeamRow{ID: 32, Name: "oc", Score: 100, WUs: 1}),
		users(
			parse.UserRow{Name: "DH", Score: 10, WUs: 1, TeamID: 32},
			parse.UserRow{Name: "toTOW", Score: 20, WUs: 2, TeamID: 51},
			parse.UserRow{Name: "DH", Score: 30, WUs: 3, TeamID: 51},
		))

	restored := model.NewState()
	if err := s.LoadIdentity(context.Background(), restored); err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	if len(restored.Members) != len(st.Members) {
		t.Fatalf("restored %d members, want %d", len(restored.Members), len(st.Members))
	}
	for i := range st.Members {
		a, b := st.Members[i], restored.Members[i]
		if a.NameID != b.NameID || a.TeamID != b.TeamID {
			t.Errorf("member slot %d: got (%d,%d) want (%d,%d)",
				i, b.NameID, b.TeamID, a.NameID, a.TeamID)
		}
		// Names must resolve identically, not merely share an id.
		if got, want := restored.Names.Name(b.NameID), st.Names.Name(a.NameID); got != want {
			t.Errorf("member slot %d name = %q, want %q", i, got, want)
		}
	}
	// Cumulative totals must come back too, not just identity. Restoring them by
	// replaying the newest snapshot instead would zero out any donor that had
	// dropped out of the feed, wrecking the leaderboard on restart.
	for i := range st.Members {
		if got, want := restored.Members[i].Score, st.Members[i].Score; got != want {
			t.Errorf("member slot %d score = %d after restart, want %d", i, got, want)
		}
		if got, want := restored.Members[i].WUs, st.Members[i].WUs; got != want {
			t.Errorf("member slot %d wus = %d after restart, want %d", i, got, want)
		}
	}
	for i := range st.Teams {
		if got, want := restored.Teams[i].Score, st.Teams[i].Score; got != want {
			t.Errorf("team slot %d score = %d after restart, want %d", i, got, want)
		}
	}
}

func TestTotalsTrackAcrossMultipleCycles(t *testing.T) {
	// Totals are refreshed only for entities that changed in a cycle. An entity that
	// moves, then goes idle, must keep its latest total rather than reverting.
	s := open(t)
	st := model.NewState()
	writeCycle(t, s, st, at(1), nil, users(
		parse.UserRow{Name: "mover", Score: 100, WUs: 1, TeamID: 1},
		parse.UserRow{Name: "idler", Score: 500, WUs: 5, TeamID: 1},
	))
	writeCycle(t, s, st, at(2), nil, users(
		parse.UserRow{Name: "mover", Score: 900, WUs: 9, TeamID: 1},
		parse.UserRow{Name: "idler", Score: 500, WUs: 5, TeamID: 1},
	))
	writeCycle(t, s, st, at(3), nil, users(
		parse.UserRow{Name: "mover", Score: 900, WUs: 9, TeamID: 1},
		parse.UserRow{Name: "idler", Score: 500, WUs: 5, TeamID: 1},
	))

	restored := model.NewState()
	if err := s.LoadIdentity(context.Background(), restored); err != nil {
		t.Fatal(err)
	}
	byName := map[string]int64{}
	for _, m := range restored.Members {
		byName[restored.Names.Name(m.NameID)] = m.Score
	}
	if byName["mover"] != 900 {
		t.Errorf("mover score = %d, want 900", byName["mover"])
	}
	// The idler never produced a delta after its first sighting, so its total was
	// written once at insert and never updated — it must still be right.
	if byName["idler"] != 500 {
		t.Errorf("idler score = %d, want 500", byName["idler"])
	}
}

func TestRestoreThenApplyProducesCorrectDelta(t *testing.T) {
	// The real restart path: load identity, replay the latest snapshot to rebuild
	// current totals, then ingest the next one. The replay must not emit deltas,
	// and the following cycle must produce the right ones.
	s := open(t)
	st := model.NewState()
	snapshot1 := users(parse.UserRow{Name: "DH", Score: 1000, WUs: 10, TeamID: 32})
	writeCycle(t, s, st, at(1), nil, snapshot1)

	restored := model.NewState()
	if err := s.LoadIdentity(context.Background(), restored); err != nil {
		t.Fatal(err)
	}
	replay := restored.Apply(at(1), nil, snapshot1)
	if len(replay.MemberDeltas) != 0 {
		t.Errorf("replay produced %d deltas, want 0", len(replay.MemberDeltas))
	}
	if len(replay.NewMembers) != 0 {
		t.Errorf("replay saw %d new members, want 0 (identity was restored)", len(replay.NewMembers))
	}

	next := restored.Apply(at(2), nil, users(parse.UserRow{Name: "DH", Score: 1750, WUs: 15, TeamID: 32}))
	if len(next.MemberDeltas) != 1 || next.MemberDeltas[0].DScore != 750 {
		t.Errorf("post-restore delta = %+v, want DScore=750", next.MemberDeltas)
	}
}

func TestCycleHistoryRoundTrip(t *testing.T) {
	s := open(t)
	st := model.NewState()
	u := func(score, wus int64) []parse.UserRow {
		return users(parse.UserRow{Name: "DH", Score: score, WUs: wus, TeamID: 32})
	}
	writeCycle(t, s, st, at(1), nil, u(1000, 10))
	writeCycle(t, s, st, at(2), nil, u(1750, 15))
	writeCycle(t, s, st, at(3), nil, u(1750, 15)) // idle: no delta stored
	writeCycle(t, s, st, at(4), nil, u(2000, 18))

	pts, err := s.MemberHistory(context.Background(), 0, at(0), at(9), Cycle)
	if err != nil {
		t.Fatalf("MemberHistory: %v", err)
	}
	// Three cycles produced deltas; the idle hour stores nothing at all, which is
	// what keeps the table sparse.
	if len(pts) != 2 {
		t.Fatalf("got %d points, want 2: %+v", len(pts), pts)
	}
	if pts[0].Points != 750 || pts[1].Points != 250 {
		t.Errorf("points = %d,%d want 750,250", pts[0].Points, pts[1].Points)
	}
	if !pts[0].At.Equal(at(2)) {
		t.Errorf("first point at %v, want %v", pts[0].At, at(2))
	}
}

func TestCompactRollsUpThenPrunes(t *testing.T) {
	s := open(t)
	st := model.NewState()
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	score := int64(1000)
	for i := 0; i < 6; i++ { // 6 cycles across two UTC days
		score += 100
		when := base.Add(time.Duration(i*8) * time.Hour)
		c := st.Apply(when, nil, users(parse.UserRow{Name: "DH", Score: score, WUs: 1, TeamID: 32}))
		if err := s.WriteCycle(context.Background(), st, c, CycleMeta{
			TeamSnapshotAt: when, UserSnapshotAt: when}); err != nil {
			t.Fatal(err)
		}
	}

	ctx := context.Background()
	res, err := s.Compact(ctx, CompactPolicy{RawBefore: base.Add(90 * 24 * time.Hour)})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.PrunedRaw == 0 {
		t.Error("PrunedRaw = 0, want the raw deltas removed")
	}

	// Raw is gone...
	raw, err := s.MemberHistory(ctx, 0, base.Add(-time.Hour), base.Add(72*time.Hour), Cycle)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 0 {
		t.Errorf("got %d raw points after compaction, want 0", len(raw))
	}

	// ...but the production it represented is preserved in the rollup. The first
	// cycle is a first sighting and contributes nothing, so 5 deltas of 100 remain.
	daily, err := s.MemberHistory(ctx, 0, base.Add(-24*time.Hour), base.Add(72*time.Hour), Daily)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, p := range daily {
		total += p.Points
	}
	if total != 500 {
		t.Errorf("rolled-up total = %d, want 500 (5 deltas x 100)", total)
	}
}

func TestCompactIsIdempotent(t *testing.T) {
	// A repeated or interrupted pass must converge rather than double-count.
	s := open(t)
	st := model.NewState()
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	score := int64(1000)
	for i := 0; i < 4; i++ {
		score += 100
		when := base.Add(time.Duration(i*6) * time.Hour)
		c := st.Apply(when, nil, users(parse.UserRow{Name: "DH", Score: score, WUs: 1, TeamID: 32}))
		if err := s.WriteCycle(context.Background(), st, c, CycleMeta{
			TeamSnapshotAt: when, UserSnapshotAt: when}); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	cut := base.Add(90 * 24 * time.Hour)
	policy := CompactPolicy{RawBefore: cut}

	if _, err := s.Compact(ctx, policy); err != nil {
		t.Fatal(err)
	}
	first, err := s.MemberHistory(ctx, 0, base.Add(-24*time.Hour), cut, Daily)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Compact(ctx, policy); err != nil {
		t.Fatal(err)
	}
	second, err := s.MemberHistory(ctx, 0, base.Add(-24*time.Hour), cut, Daily)
	if err != nil {
		t.Fatal(err)
	}

	if len(first) != len(second) {
		t.Fatalf("bucket count changed: %d then %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Points != second[i].Points {
			t.Errorf("bucket %d changed on second compaction: %d -> %d",
				i, first[i].Points, second[i].Points)
		}
	}
}

func TestAppliedCyclesTracksIngest(t *testing.T) {
	s := open(t)
	st := model.NewState()
	writeCycle(t, s, st, at(1), nil, users(parse.UserRow{Name: "a", Score: 1, WUs: 1, TeamID: 1}))
	writeCycle(t, s, st, at(2), nil, users(parse.UserRow{Name: "a", Score: 2, WUs: 2, TeamID: 1}))

	applied, err := s.AppliedCycles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 2 {
		t.Errorf("AppliedCycles = %d entries, want 2", len(applied))
	}
	if !applied[at(1).Unix()] || !applied[at(2).Unix()] {
		t.Errorf("AppliedCycles missing expected timestamps: %v", applied)
	}

	latest, err := s.LatestCycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !latest.Equal(at(2)) {
		t.Errorf("LatestCycle = %v, want %v", latest, at(2))
	}
}

func TestReplayingSameCycleIsIdempotent(t *testing.T) {
	// Replay must be safe to re-run: the archive is the source of truth and we will
	// re-ingest from it whenever metric logic changes.
	s := open(t)
	st := model.NewState()
	rows := users(parse.UserRow{Name: "DH", Score: 1000, WUs: 10, TeamID: 32})
	writeCycle(t, s, st, at(1), nil, rows)
	writeCycle(t, s, st, at(2), nil, users(parse.UserRow{Name: "DH", Score: 1500, WUs: 12, TeamID: 32}))

	// Re-apply the second cycle from a fresh state, as replay would.
	fresh := model.NewState()
	if err := s.LoadIdentity(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	fresh.Apply(at(1), nil, rows)
	c := fresh.Apply(at(2), nil, users(parse.UserRow{Name: "DH", Score: 1500, WUs: 12, TeamID: 32}))
	if err := s.WriteCycle(context.Background(), fresh, c, CycleMeta{
		TeamSnapshotAt: at(2), UserSnapshotAt: at(2)}); err != nil {
		t.Fatal(err)
	}

	pts, err := s.MemberHistory(context.Background(), 0, at(0), at(9), Cycle)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 || pts[0].Points != 500 {
		t.Errorf("after replay got %+v, want a single 500-point delta", pts)
	}
}

func TestMembersHistoryAggregatesInOneQuery(t *testing.T) {
	// A donor's series is the sum of their members'. Querying per member does not
	// scale: "PS3" spans 10,426 teams, which would be that many round trips for one
	// API request.
	s := open(t)
	st := model.NewState()
	rows := func(a, b int64) []parse.UserRow {
		return users(
			parse.UserRow{Name: "DH", Score: a, WUs: 1, TeamID: 1},
			parse.UserRow{Name: "DH", Score: b, WUs: 1, TeamID: 2},
			parse.UserRow{Name: "other", Score: 999, WUs: 1, TeamID: 1},
		)
	}
	writeCycle(t, s, st, at(1), nil, rows(100, 200))
	writeCycle(t, s, st, at(2), nil, rows(150, 400))
	writeCycle(t, s, st, at(3), nil, rows(150, 700))

	dhID, _ := st.Names.Lookup("DH")
	a, _ := st.MemberSlot(dhID, 1)
	b, _ := st.MemberSlot(dhID, 2)

	pts, err := s.MembersHistory(context.Background(), []int32{a, b}, at(0), at(9), Cycle)
	if err != nil {
		t.Fatalf("MembersHistory: %v", err)
	}
	if len(pts) != 2 {
		t.Fatalf("got %d buckets, want 2: %+v", len(pts), pts)
	}
	// Cycle 2: +50 on team 1 and +200 on team 2, merged into one bucket.
	if pts[0].Points != 250 {
		t.Errorf("bucket 0 = %d, want 250", pts[0].Points)
	}
	// Cycle 3: team 1 idle, team 2 +300.
	if pts[1].Points != 300 {
		t.Errorf("bucket 1 = %d, want 300", pts[1].Points)
	}
	// The unrelated member must not leak into the aggregate.
	for _, p := range pts {
		if p.Points > 300 {
			t.Errorf("aggregate %d includes a member outside the set", p.Points)
		}
	}
}

func TestMembersHistoryEmptySet(t *testing.T) {
	s := open(t)
	pts, err := s.MembersHistory(context.Background(), nil, at(0), at(9), Cycle)
	if err != nil {
		t.Errorf("empty id set returned an error: %v", err)
	}
	if len(pts) != 0 {
		t.Errorf("got %d points for an empty id set", len(pts))
	}
}

func TestCompactPrunesDailyButKeepsMonthly(t *testing.T) {
	// The retention policy collapses daily buckets to monthly after two years.
	// Without this, member_daily grows forever and every compaction pass rescans
	// all of it.
	s := open(t)
	st := model.NewState()
	ctx := context.Background()

	// Three cycles spread across two distant months.
	old := time.Date(2023, 3, 1, 6, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 5, 1, 6, 0, 0, 0, time.UTC)
	score := int64(1000)
	for _, when := range []time.Time{old, old.Add(8 * time.Hour), recent, recent.Add(8 * time.Hour)} {
		score += 100
		c := st.Apply(when, nil, users(parse.UserRow{Name: "DH", Score: score, WUs: 1, TeamID: 32}))
		if err := s.WriteCycle(ctx, st, c, CycleMeta{TeamSnapshotAt: when, UserSnapshotAt: when}); err != nil {
			t.Fatal(err)
		}
	}

	// Roll everything up, then discard daily buckets older than 2024.
	res, err := s.Compact(ctx, CompactPolicy{
		RawBefore:   time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		DailyBefore: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.PrunedDaily == 0 {
		t.Error("PrunedDaily = 0, want the 2023 buckets removed")
	}

	// The old daily detail is gone...
	daily, err := s.MemberHistory(ctx, 0, old.Add(-24*time.Hour), old.Add(48*time.Hour), Daily)
	if err != nil {
		t.Fatal(err)
	}
	if len(daily) != 0 {
		t.Errorf("got %d daily points for a pruned period, want 0", len(daily))
	}
	// ...but the month it belonged to survives, with its production intact.
	monthly, err := s.MemberHistory(ctx, 0, old.Add(-24*time.Hour), old.Add(48*time.Hour), Monthly)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, p := range monthly {
		total += p.Points
	}
	if total != 100 {
		t.Errorf("monthly total for the pruned month = %d, want 100", total)
	}
	// Recent daily detail must be untouched.
	recentDaily, err := s.MemberHistory(ctx, 0, recent.Add(-24*time.Hour), recent.Add(48*time.Hour), Daily)
	if err != nil {
		t.Fatal(err)
	}
	if len(recentDaily) == 0 {
		t.Error("recent daily buckets were pruned")
	}
}

func TestCompactAfterDailyPruneKeepsOldMonths(t *testing.T) {
	// The monthly recompute is bounded to months touched by the current pass. A
	// later pass must therefore leave already-written months alone, even though the
	// daily rows behind them no longer exist.
	s := open(t)
	st := model.NewState()
	ctx := context.Background()

	old := time.Date(2023, 3, 1, 6, 0, 0, 0, time.UTC)
	score := int64(1000)
	for i := 0; i < 2; i++ {
		score += 100
		when := old.Add(time.Duration(i*8) * time.Hour)
		c := st.Apply(when, nil, users(parse.UserRow{Name: "DH", Score: score, WUs: 1, TeamID: 32}))
		if err := s.WriteCycle(ctx, st, c, CycleMeta{TeamSnapshotAt: when, UserSnapshotAt: when}); err != nil {
			t.Fatal(err)
		}
	}
	policy := CompactPolicy{
		RawBefore:   time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		DailyBefore: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if _, err := s.Compact(ctx, policy); err != nil {
		t.Fatal(err)
	}
	before, err := s.MemberHistory(ctx, 0, old.Add(-24*time.Hour), old.Add(48*time.Hour), Monthly)
	if err != nil {
		t.Fatal(err)
	}

	// A second pass with nothing new to roll up must not zero the old month.
	if _, err := s.Compact(ctx, policy); err != nil {
		t.Fatal(err)
	}
	after, err := s.MemberHistory(ctx, 0, old.Add(-24*time.Hour), old.Add(48*time.Hour), Monthly)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("monthly bucket count changed: %d then %d", len(before), len(after))
	}
	for i := range before {
		if before[i].Points != after[i].Points {
			t.Errorf("month %v changed from %d to %d after a no-op compaction",
				before[i].At, before[i].Points, after[i].Points)
		}
	}
}

func TestHistoryRangeNarrowerThanOneBucket(t *testing.T) {
	// A bucket is a period, so it belongs in the result if it overlaps the range at
	// all. With an exclusive upper bound, asking for a single month at monthly
	// granularity returns nothing — silently, which is the worst kind of empty.
	s := open(t)
	st := model.NewState()
	ctx := context.Background()
	base := time.Date(2026, 5, 10, 6, 0, 0, 0, time.UTC)

	score := int64(1000)
	for i := 0; i < 3; i++ {
		score += 100
		when := base.Add(time.Duration(i*8) * time.Hour)
		c := st.Apply(when, nil, users(parse.UserRow{Name: "DH", Score: score, WUs: 1, TeamID: 32}))
		if err := s.WriteCycle(ctx, st, c, CycleMeta{TeamSnapshotAt: when, UserSnapshotAt: when}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Compact(ctx, CompactPolicy{RawBefore: base.Add(365 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		from, to time.Time
		gran     Granularity
	}{
		{"one month", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC), Monthly},
		{"one day", base.Truncate(24 * time.Hour), base.Add(12 * time.Hour), Daily},
		{"instant within a day", base, base.Add(time.Hour), Daily},
	} {
		pts, err := s.MemberHistory(ctx, 0, tc.from, tc.to, tc.gran)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var total int64
		for _, p := range pts {
			total += p.Points
		}
		if total != 200 {
			t.Errorf("%s: total = %d over %d buckets, want 200", tc.name, total, len(pts))
		}
	}

	// The aggregated multi-member path must agree.
	pts, err := s.MembersHistory(ctx, []int32{0},
		time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC), Monthly)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, p := range pts {
		total += p.Points
	}
	if total != 200 {
		t.Errorf("MembersHistory monthly total = %d, want 200", total)
	}
}

func TestRollupsExistImmediately(t *testing.T) {
	// Daily and monthly buckets must be queryable as soon as a cycle is ingested.
	// Deriving them only at compaction time left every entity showing "no
	// production recorded" at daily and monthly granularity for the first 90 days
	// of a deployment, with the raw data present the whole time.
	s := open(t)
	st := model.NewState()
	ctx := context.Background()

	base := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	score := int64(1000)
	for i := 0; i < 5; i++ {
		score += 100
		when := base.Add(time.Duration(i) * time.Hour)
		writeCycle(t, s, st, when, teams(parse.TeamRow{ID: 32, Name: "oc", Score: score * 2, WUs: 1}),
			users(parse.UserRow{Name: "DH", Score: score, WUs: 1, TeamID: 32}))
	}

	// Four deltas of 100 (the first cycle is a first sighting).
	for _, g := range []Granularity{Daily, Monthly} {
		pts, err := s.MemberHistory(ctx, 0, base.Add(-24*time.Hour), base.Add(48*time.Hour), g)
		if err != nil {
			t.Fatalf("%s: %v", g, err)
		}
		var total int64
		for _, p := range pts {
			total += p.Points
		}
		if total != 400 {
			t.Errorf("%s member total = %d over %d buckets, want 400", g, total, len(pts))
		}
	}
	// Teams too — they use a different id column and are easy to miss.
	pts, err := s.TeamHistory(ctx, 0, base.Add(-24*time.Hour), base.Add(48*time.Hour), Daily)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, p := range pts {
		total += p.Points
	}
	if total != 800 {
		t.Errorf("team daily total = %d, want 800", total)
	}
}

func TestRollupsAreIdempotentOnRewrite(t *testing.T) {
	// A cycle can be written more than once — an interrupted ingest, or replay
	// re-covering the same snapshot. Buckets are recomputed rather than
	// incremented, so a rewrite must converge instead of doubling.
	s := open(t)
	st := model.NewState()
	ctx := context.Background()
	base := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)

	var last *model.Cycle
	var lastAt time.Time
	score := int64(1000)
	for i := 0; i < 4; i++ {
		score += 100
		lastAt = base.Add(time.Duration(i) * time.Hour)
		last = st.Apply(lastAt, nil, users(parse.UserRow{Name: "DH", Score: score, WUs: 1, TeamID: 32}))
		if err := s.WriteCycle(ctx, st, last, CycleMeta{TeamSnapshotAt: lastAt, UserSnapshotAt: lastAt}); err != nil {
			t.Fatal(err)
		}
	}

	total := func() int64 {
		pts, err := s.MemberHistory(ctx, 0, base.Add(-24*time.Hour), base.Add(48*time.Hour), Daily)
		if err != nil {
			t.Fatal(err)
		}
		var n int64
		for _, p := range pts {
			n += p.Points
		}
		return n
	}

	before := total()
	if before != 300 {
		t.Fatalf("daily total = %d, want 300", before)
	}
	// Re-write the same cycle verbatim.
	if err := s.WriteCycle(ctx, st, last, CycleMeta{TeamSnapshotAt: lastAt, UserSnapshotAt: lastAt}); err != nil {
		t.Fatal(err)
	}
	if after := total(); after != before {
		t.Errorf("daily total changed on rewrite: %d then %d", before, after)
	}
}

func TestReplayFromScratchNeedsTotalsCleared(t *testing.T) {
	// Identity must be restored so stored deltas keep pointing at the right donors,
	// but the totals must not be — replaying cycle 1 against the final scores makes
	// the first delta hugely negative. ResetTotals is what separates replay from
	// resume.
	s := open(t)
	st := model.NewState()
	ctx := context.Background()
	base := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)

	rows := func(score int64) []parse.UserRow {
		return users(parse.UserRow{Name: "DH", Score: score, WUs: 1, TeamID: 32})
	}
	for i, sc := range []int64{1000, 1100, 1200} {
		when := base.Add(time.Duration(i) * time.Hour)
		writeCycle(t, s, st, when, nil, rows(sc))
	}

	replayed := model.NewState()
	if err := s.LoadIdentity(ctx, replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.Members[0].Score == 0 {
		t.Fatal("LoadIdentity did not restore totals; resume would be broken")
	}
	replayed.ResetTotals()
	if replayed.Members[0].Score != 0 {
		t.Fatal("ResetTotals left a total behind")
	}
	// Identity survives the reset, so ids still line up with stored deltas.
	if len(replayed.Members) != len(st.Members) {
		t.Fatalf("ResetTotals changed member count: %d vs %d", len(replayed.Members), len(st.Members))
	}

	first := replayed.Apply(base, nil, rows(1000))
	if len(first.MemberDeltas) != 0 {
		t.Errorf("replayed first cycle produced %+v, want no deltas", first.MemberDeltas)
	}
	second := replayed.Apply(base.Add(time.Hour), nil, rows(1100))
	if len(second.MemberDeltas) != 1 || second.MemberDeltas[0].DScore != 100 {
		t.Errorf("replayed second cycle = %+v, want one delta of 100", second.MemberDeltas)
	}
}

func TestCompactStillPrunesWithoutLosingRollups(t *testing.T) {
	// Compaction now only prunes. The buckets written at ingest must survive it.
	s := open(t)
	st := model.NewState()
	ctx := context.Background()
	old := time.Date(2023, 3, 1, 6, 0, 0, 0, time.UTC)

	score := int64(1000)
	for i := 0; i < 3; i++ {
		score += 100
		when := old.Add(time.Duration(i) * time.Hour)
		writeCycle(t, s, st, when, nil, users(parse.UserRow{Name: "DH", Score: score, WUs: 1, TeamID: 32}))
	}

	res, err := s.Compact(ctx, CompactPolicy{RawBefore: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if res.PrunedRaw == 0 {
		t.Error("PrunedRaw = 0, want raw deltas removed")
	}
	raw, _ := s.MemberHistory(ctx, 0, old.Add(-time.Hour), old.Add(48*time.Hour), Cycle)
	if len(raw) != 0 {
		t.Errorf("got %d raw points after compaction, want 0", len(raw))
	}
	daily, err := s.MemberHistory(ctx, 0, old.Add(-24*time.Hour), old.Add(48*time.Hour), Daily)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, p := range daily {
		total += p.Points
	}
	if total != 200 {
		t.Errorf("daily total after compaction = %d, want 200", total)
	}
}
