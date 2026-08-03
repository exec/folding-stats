package rank

import (
	"math/rand"
	"sort"
	"testing"
	"time"

	"folding/internal/metrics"
	"folding/internal/model"
	"folding/internal/parse"
)

func now() time.Time { return time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC) }

func build(t *testing.T, teams []parse.TeamRow, users []parse.UserRow) (*model.State, *Table) {
	t.Helper()
	st := model.NewState()
	st.Apply(now(), teams, users)
	return st, Build(st, now(), DefaultConfig)
}

func u(name string, score int64, team int32) parse.UserRow {
	return parse.UserRow{Name: name, Score: score, WUs: score / 10, TeamID: team}
}

func TestMemberRankingIsByScoreDescending(t *testing.T) {
	st, tbl := build(t, nil, []parse.UserRow{
		u("low", 100, 1), u("high", 900, 1), u("mid", 500, 1),
	})
	var names []string
	for _, slot := range tbl.MemberOrder {
		names = append(names, st.Names.Name(st.Members[slot].NameID))
	}
	want := []string{"high", "mid", "low"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("order = %v, want %v", names, want)
		}
	}
	highSlot, _ := st.MemberSlot(st.Names.Intern("high"), 1)
	if got := tbl.MemberRank[highSlot]; got != 1 {
		t.Errorf("rank of top member = %d, want 1", got)
	}
}

func TestTiesBreakDeterministicallyBySlot(t *testing.T) {
	// Millions of donors are tied on zero. If ties shuffled between cycles, rank
	// movement would be pure noise and every rank-change column would be garbage.
	rows := make([]parse.UserRow, 500)
	for i := range rows {
		rows[i] = u(string(rune('a'+i%26))+string(rune('a'+i/26)), 0, 1)
	}
	st := model.NewState()
	st.Apply(now(), nil, rows)

	first := Build(st, now(), DefaultConfig)
	second := Build(st, now(), DefaultConfig)
	for i := range first.MemberOrder {
		if first.MemberOrder[i] != second.MemberOrder[i] {
			t.Fatalf("order differs at %d between identical builds", i)
		}
	}
	// Within a tie group, order must follow slot so it is predictable.
	for i := 1; i < len(first.MemberOrder); i++ {
		a, b := first.MemberOrder[i-1], first.MemberOrder[i]
		if st.Members[a].Score == st.Members[b].Score && a > b {
			t.Fatalf("tie at %d ordered by slot %d before %d", i, a, b)
		}
	}
}

func TestInTeamRankMatchesGlobalOrder(t *testing.T) {
	st, tbl := build(t, nil, []parse.UserRow{
		u("t1a", 900, 1), u("t2a", 800, 2), u("t1b", 700, 1),
		u("t2b", 600, 2), u("t1c", 500, 1),
	})
	check := func(name string, team int32, wantRank int32) {
		slot, _ := st.MemberSlot(st.Names.Intern(name), team)
		if got := tbl.InTeamRank[slot]; got != wantRank {
			t.Errorf("%s in-team rank = %d, want %d", name, got, wantRank)
		}
	}
	check("t1a", 1, 1)
	check("t1b", 1, 2)
	check("t1c", 1, 3)
	check("t2a", 2, 1)
	check("t2b", 2, 2)

	// Consistency: a member ranked higher globally can never rank lower inside a
	// shared team.
	for i := 1; i < len(tbl.MemberOrder); i++ {
		a, b := tbl.MemberOrder[i-1], tbl.MemberOrder[i]
		if st.Members[a].TeamID == st.Members[b].TeamID &&
			tbl.InTeamRank[a] > tbl.InTeamRank[b] {
			t.Errorf("global order and in-team rank disagree for slots %d,%d", a, b)
		}
	}
}

func TestDonorAggregatesAcrossTeams(t *testing.T) {
	// The central R1 behaviour: one name folding for three teams is one donor whose
	// total is the sum, while remaining three distinct members.
	st, tbl := build(t, nil, []parse.UserRow{
		u("DH", 100, 1), u("DH", 200, 2), u("DH", 300, 3),
		u("solo", 250, 1),
	})
	if len(st.Members) != 4 {
		t.Fatalf("members = %d, want 4", len(st.Members))
	}
	if len(tbl.Donors) != 2 {
		t.Fatalf("donors = %d, want 2", len(tbl.Donors))
	}
	dh := tbl.Donors[tbl.DonorIndex[st.Names.Intern("DH")]]
	if dh.Score != 600 {
		t.Errorf("DH aggregate = %d, want 600", dh.Score)
	}
	if dh.TeamCount != 3 {
		t.Errorf("DH TeamCount = %d, want 3", dh.TeamCount)
	}
	// Aggregated, DH outranks a member who beats any single DH row.
	if tbl.DonorRank(st.Names.Intern("DH")) != 1 {
		t.Errorf("DH donor rank = %d, want 1", tbl.DonorRank(st.Names.Intern("DH")))
	}
}

func TestDonorMembersBreakdown(t *testing.T) {
	// R9/R10: one call must return a donor's per-team breakdown, so the mapping has
	// to be a lookup rather than a scan.
	st, tbl := build(t, nil, []parse.UserRow{
		u("DH", 100, 1), u("other", 50, 1), u("DH", 200, 2),
	})
	idx := tbl.DonorIndex[st.Names.Intern("DH")]
	members := tbl.DonorMembers(idx)
	if len(members) != 2 {
		t.Fatalf("DonorMembers = %d entries, want 2", len(members))
	}
	teams := map[int32]int64{}
	for _, slot := range members {
		teams[st.Members[slot].TeamID] = st.Members[slot].Score
	}
	if teams[1] != 100 || teams[2] != 200 {
		t.Errorf("breakdown = %v, want {1:100, 2:200}", teams)
	}
}

func TestPseudoIdentityIsFlaggedNotHidden(t *testing.T) {
	// "PS3" appears on 10,426 teams upstream and "Anonymous" on 5,993. Aggregating
	// them yields a fictional donor that would otherwise top the leaderboard. The
	// points are real, so the entry stays — it is labelled, not suppressed.
	var rows []parse.UserRow
	for i := int32(0); i < 200; i++ {
		rows = append(rows, u("PS3", 1000, i+1))
	}
	rows = append(rows, u("realperson", 5000, 1))
	st, tbl := build(t, nil, rows)

	ps3 := tbl.Donors[tbl.DonorIndex[st.Names.Intern("PS3")]]
	if !ps3.LikelyNotAPerson {
		t.Error("PS3 across 200 teams not flagged")
	}
	if ps3.Score != 200_000 {
		t.Errorf("PS3 aggregate = %d, want 200000", ps3.Score)
	}
	real := tbl.Donors[tbl.DonorIndex[st.Names.Intern("realperson")]]
	if real.LikelyNotAPerson {
		t.Error("a single-team donor was flagged as a pseudo-identity")
	}
	// Still present and still ranked.
	if tbl.DonorRank(st.Names.Intern("PS3")) == 0 {
		t.Error("flagged donor was removed from the ranking")
	}
}

func TestLegitimateMultiTeamDonorNotFlagged(t *testing.T) {
	// 15% of names appear on more than one team; those are real people and must not
	// be swept up by the threshold.
	var rows []parse.UserRow
	for i := int32(0); i < 12; i++ {
		rows = append(rows, u("multi", 100, i+1))
	}
	st, tbl := build(t, nil, rows)
	d := tbl.Donors[tbl.DonorIndex[st.Names.Intern("multi")]]
	if d.LikelyNotAPerson {
		t.Errorf("donor on 12 teams flagged; threshold is %d", DefaultConfig.PseudoIdentityTeams)
	}
}

func TestTeamNamesDoNotBecomeDonors(t *testing.T) {
	// Team and donor names share one arena. A team name with no members must not
	// appear in the donor list.
	st, tbl := build(t,
		[]parse.TeamRow{{ID: 1, Name: "TeamOnly", Score: 500, WUs: 5}},
		[]parse.UserRow{u("person", 100, 1)},
	)
	if len(tbl.Donors) != 1 {
		t.Fatalf("donors = %d, want 1", len(tbl.Donors))
	}
	if got := tbl.DonorRank(st.Names.Intern("TeamOnly")); got != 0 {
		t.Errorf("team name got donor rank %d, want 0", got)
	}
}

func TestWindowClampsAtBoundaries(t *testing.T) {
	var rows []parse.UserRow
	for i := 0; i < 10; i++ {
		rows = append(rows, u(string(rune('a'+i)), int64(100-i), 1))
	}
	_, tbl := build(t, nil, rows)

	if got := len(tbl.MemberWindow(1, 4)); got != 5 {
		t.Errorf("window at rank 1 = %d entries, want 5 (clamped at the top)", got)
	}
	if got := len(tbl.MemberWindow(10, 4)); got != 5 {
		t.Errorf("window at last rank = %d entries, want 5", got)
	}
	if got := len(tbl.MemberWindow(5, 2)); got != 5 {
		t.Errorf("mid-list window = %d entries, want 5", got)
	}
	if got := tbl.MemberWindow(0, 4); got != nil {
		t.Errorf("window at rank 0 = %v, want nil", got)
	}
}

// applyCycle mirrors the service's per-cycle sequence: fold the snapshot into state,
// grow the windows to the new corpus size, then push that cycle's deltas. The order
// matters — the window records the entity count at push time, which is what tells a
// pre-existing entity apart from one that has only just appeared.
func applyCycle(st *model.State, mw, tw *metrics.Window, at time.Time,
	teams []parse.TeamRow, users []parse.UserRow) {
	c := st.Apply(at, teams, users)
	mw.Grow(len(st.Members))
	mw.Push(at, c.MemberDeltas)
	tw.Grow(len(st.Teams))
	tw.Push(at, c.TeamDeltas)
}

func slotOf(t *testing.T, st *model.State, name string, team int32) int32 {
	t.Helper()
	nameID, ok := st.Names.Lookup(name)
	if !ok {
		t.Fatalf("name %q not interned", name)
	}
	slot, ok := st.MemberSlot(nameID, team)
	if !ok {
		t.Fatalf("no member slot for (%q, %d)", name, team)
	}
	return slot
}

func TestRankChange24hReportsMovement(t *testing.T) {
	st := model.NewState()
	mw, tw := metrics.New(0), metrics.New(0)

	t0 := now()
	applyCycle(st, mw, tw, t0, nil, []parse.UserRow{u("a", 100, 1), u("b", 200, 1), u("c", 300, 1)})
	// More than 24h later, so the first cycle ages out of the rolling window and
	// becomes the baseline the comparison is made against.
	t1 := t0.Add(25 * time.Hour)
	applyCycle(st, mw, tw, t1, nil, []parse.UserRow{u("a", 1000, 1), u("b", 200, 1), u("c", 300, 1)})

	tbl := Build(st, t1, DefaultConfig)
	tbl.BuildChange24h(st, mw, tw)

	// Was 3rd on 100 points, now 1st on 1000: two places gained.
	for _, tc := range []struct {
		name string
		want int32
	}{
		{"a", 2},  // 3rd -> 1st
		{"b", -1}, // 2nd -> 3rd
		{"c", -1}, // 1st -> 2nd
	} {
		slot := slotOf(t, st, tc.name, 1)
		got, ok := tbl.MemberChange24h(slot)
		if !ok {
			t.Errorf("%s: no rank change reported, want %+d", tc.name, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: rank change = %+d, want %+d", tc.name, got, tc.want)
		}
	}

	// Each donor here folds for one team, so donor movement must match the member's.
	for _, name := range []string{"a", "b", "c"} {
		nameID, _ := st.Names.Lookup(name)
		idx := tbl.DonorIndex[nameID]
		donor, ok := tbl.DonorChange24h(idx)
		if !ok {
			t.Fatalf("%s: no donor rank change reported", name)
		}
		member, _ := tbl.MemberChange24h(slotOf(t, st, name, 1))
		if donor != member {
			t.Errorf("%s: donor change %+d, member change %+d — single-team donor should match",
				name, donor, member)
		}
	}
}

func TestRankChange24hIsAbsentForEntitiesNewerThanADay(t *testing.T) {
	st := model.NewState()
	mw, tw := metrics.New(0), metrics.New(0)

	t0 := now()
	applyCycle(st, mw, tw, t0, nil, []parse.UserRow{u("a", 100, 1), u("b", 200, 1)})
	t1 := t0.Add(25 * time.Hour)
	// "newcomer" arrives with a large lifetime total accumulated before we ever saw
	// it. Ranking it against a reconstructed past would invent a position it never
	// held, so it must report nothing at all.
	applyCycle(st, mw, tw, t1, nil, []parse.UserRow{
		u("a", 100, 1), u("b", 200, 1), u("newcomer", 9000, 1),
	})

	tbl := Build(st, t1, DefaultConfig)
	tbl.BuildChange24h(st, mw, tw)

	if got, ok := tbl.MemberChange24h(slotOf(t, st, "newcomer", 1)); ok {
		t.Errorf("newcomer reported a rank change of %+d; want none", got)
	}
	nameID, _ := st.Names.Lookup("newcomer")
	if got, ok := tbl.DonorChange24h(tbl.DonorIndex[nameID]); ok {
		t.Errorf("new donor reported a rank change of %+d; want none", got)
	}
	// The established members still report, and their movement is measured against
	// each other rather than against the entity that displaced them.
	if _, ok := tbl.MemberChange24h(slotOf(t, st, "a", 1)); !ok {
		t.Error("established member reported no rank change; want one")
	}
}

func TestRankChange24hAbsentBeforeADayHasPassed(t *testing.T) {
	st := model.NewState()
	mw, tw := metrics.New(0), metrics.New(0)

	t0 := now()
	applyCycle(st, mw, tw, t0, nil, []parse.UserRow{u("a", 100, 1), u("b", 200, 1)})
	// One hour on: the window holds both cycles, so nothing has aged out and there
	// is no earlier corpus to compare against.
	applyCycle(st, mw, tw, t0.Add(time.Hour), nil, []parse.UserRow{u("a", 500, 1), u("b", 200, 1)})

	tbl := Build(st, t0.Add(time.Hour), DefaultConfig)
	tbl.BuildChange24h(st, mw, tw)

	if got, ok := tbl.MemberChange24h(slotOf(t, st, "a", 1)); ok {
		t.Errorf("rank change %+d reported after one hour; want none until 24h is observed", got)
	}
}

func TestRankChange24hSurvivesScoreRegression(t *testing.T) {
	// A feed glitch can make an entity's deltas exceed its current total, which would
	// reconstruct a negative past score. The radix sort orders on inverted bits and
	// only holds for non-negative values, so an unclamped negative would not merely
	// misplace one entity — it would scramble the whole ranking.
	st := model.NewState()
	mw, tw := metrics.New(0), metrics.New(0)

	t0 := now()
	applyCycle(st, mw, tw, t0, nil, []parse.UserRow{u("a", 900, 1), u("b", 200, 1), u("c", 50, 1)})
	t1 := t0.Add(25 * time.Hour)
	applyCycle(st, mw, tw, t1, nil, []parse.UserRow{u("a", 10, 1), u("b", 200, 1), u("c", 50, 1)})

	tbl := Build(st, t1, DefaultConfig)
	tbl.BuildChange24h(st, mw, tw)

	// b outscores c now and did a day ago, so whatever happened to a must not have
	// disturbed their relative order.
	bChange, okB := tbl.MemberChange24h(slotOf(t, st, "b", 1))
	cChange, okC := tbl.MemberChange24h(slotOf(t, st, "c", 1))
	if !okB || !okC {
		t.Fatal("regression suppressed rank change for unaffected members")
	}
	if bChange != 1 || cChange != 1 {
		t.Errorf("b %+d, c %+d; both should have gained one place as a fell", bChange, cChange)
	}
}

func TestRadixMatchesComparisonSort(t *testing.T) {
	// The radix sort is the piece most likely to be subtly wrong, so check it
	// against the standard library over randomised inputs including the extremes.
	rng := rand.New(rand.NewSource(7))
	for _, n := range []int{0, 1, 2, 17, 1000, 5000} {
		scores := make([]int64, n)
		for i := range scores {
			switch i % 5 {
			case 0:
				scores[i] = 0
			case 1:
				scores[i] = rng.Int63n(1 << 43) // realistic magnitude
			case 2:
				scores[i] = rng.Int63n(1000)
			case 3:
				scores[i] = 1<<62 - 1 // huge, exercising the high bytes
			default:
				scores[i] = rng.Int63()
			}
		}
		var buf sortBuf
		got := sortDescByScore(scores, &buf)
		if len(got) != n {
			t.Fatalf("n=%d: got %d ids", n, len(got))
		}
		want := make([]int32, n)
		for i := range want {
			want[i] = int32(i)
		}
		sort.SliceStable(want, func(a, b int) bool {
			return scores[want[a]] > scores[want[b]]
		})
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("n=%d: position %d = id %d (score %d), want id %d (score %d)",
					n, i, got[i], scores[got[i]], want[i], scores[want[i]])
			}
		}
	}
}

func TestTeamMembersRosterIsRankOrdered(t *testing.T) {
	// Serving a team's roster by scanning the global 2.7M-entry order costs the same
	// for a two-person team as for the largest one, so it is precomputed per team.
	st, tbl := build(t, nil, []parse.UserRow{
		u("t1_mid", 500, 1), u("t2_top", 900, 2),
		u("t1_top", 800, 1), u("t2_low", 100, 2), u("t1_low", 200, 1),
	})

	for _, tc := range []struct {
		team int32
		want []string
	}{
		{1, []string{"t1_top", "t1_mid", "t1_low"}},
		{2, []string{"t2_top", "t2_low"}},
	} {
		var got []string
		for _, slot := range tbl.TeamMembers(tc.team) {
			got = append(got, st.Names.Name(st.Members[slot].NameID))
		}
		if len(got) != len(tc.want) {
			t.Fatalf("team %d roster = %v, want %v", tc.team, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("team %d roster = %v, want %v", tc.team, got, tc.want)
				break
			}
		}
	}

	// Rosters must partition the corpus: every member appears exactly once.
	seen := map[int32]int{}
	for _, team := range []int32{1, 2} {
		for _, slot := range tbl.TeamMembers(team) {
			seen[slot]++
		}
	}
	if len(seen) != len(st.Members) {
		t.Errorf("rosters cover %d members, want %d", len(seen), len(st.Members))
	}
	for slot, n := range seen {
		if n != 1 {
			t.Errorf("member slot %d appears in %d rosters", slot, n)
		}
	}
}

func TestTeamMembersUnknownTeam(t *testing.T) {
	_, tbl := build(t, nil, []parse.UserRow{u("a", 1, 5)})
	if got := tbl.TeamMembers(9999); got != nil {
		t.Errorf("unknown team roster = %v, want nil", got)
	}
	if got := tbl.TeamMembers(-1); got != nil {
		t.Errorf("negative team roster = %v, want nil", got)
	}
}
