package model

import (
	"fmt"
	"math"
	"testing"
	"time"

	"folding/internal/parse"
)

func t0() time.Time { return time.Date(2026, 8, 2, 21, 0, 0, 0, time.UTC) }

func users(rows ...parse.UserRow) []parse.UserRow { return rows }
func teams(rows ...parse.TeamRow) []parse.TeamRow { return rows }

func TestFirstSightingIsNotProduction(t *testing.T) {
	s := NewState()
	c := s.Apply(t0(),
		teams(parse.TeamRow{ID: 32, Name: "overclockers", Score: 1_000_000, WUs: 10}),
		users(parse.UserRow{Name: "DH", Score: 2_290_929_677, WUs: 2571, TeamID: 32}),
	)
	// A donor's first appearance carries a lifetime total accumulated before we
	// were watching. Counting it as production would fabricate a huge spike on
	// whatever day we happened to start.
	if len(c.MemberDeltas) != 0 {
		t.Errorf("MemberDeltas = %+v, want none on first sighting", c.MemberDeltas)
	}
	if len(c.TeamDeltas) != 0 {
		t.Errorf("TeamDeltas = %+v, want none on first sighting", c.TeamDeltas)
	}
	if len(c.NewMembers) != 1 || len(c.NewTeams) != 1 {
		t.Errorf("NewMembers=%d NewTeams=%d, want 1 and 1", len(c.NewMembers), len(c.NewTeams))
	}
	// The totals must still be recorded, or the next cycle's delta would be wrong.
	if s.Members[0].Score != 2_290_929_677 {
		t.Errorf("member score = %d, want 2290929677", s.Members[0].Score)
	}
}

func TestOverflowingSnapshotIsRejectedBeforeMutation(t *testing.T) {
	s := NewState()
	rows := users(
		parse.UserRow{Name: "a", Score: math.MaxInt64, WUs: 1, TeamID: 1},
		parse.UserRow{Name: "b", Score: 1, WUs: 1, TeamID: 1},
	)
	if err := ValidateSnapshot(nil, rows); err == nil {
		t.Fatal("overflowing aggregate was accepted")
	}
	if cycle := s.Apply(t0(), nil, rows); cycle != nil {
		t.Fatalf("Apply returned a cycle for unsafe input: %+v", cycle)
	}
	if len(s.Members) != 0 || !s.At.IsZero() {
		t.Fatalf("state mutated after rejected input: members=%d at=%v", len(s.Members), s.At)
	}
}

func TestSecondCycleProducesDelta(t *testing.T) {
	s := NewState()
	s.Apply(t0(),
		teams(parse.TeamRow{ID: 32, Name: "oc", Score: 1_000_000, WUs: 10}),
		users(parse.UserRow{Name: "DH", Score: 1000, WUs: 2, TeamID: 32}),
	)
	c := s.Apply(t0().Add(time.Hour),
		teams(parse.TeamRow{ID: 32, Name: "oc", Score: 1_050_000, WUs: 15}),
		users(parse.UserRow{Name: "DH", Score: 1750, WUs: 5, TeamID: 32}),
	)
	if len(c.MemberDeltas) != 1 {
		t.Fatalf("MemberDeltas = %+v, want 1", c.MemberDeltas)
	}
	if got := c.MemberDeltas[0]; got.DScore != 750 || got.DWUs != 3 {
		t.Errorf("member delta = %+v, want DScore=750 DWUs=3", got)
	}
	if len(c.TeamDeltas) != 1 || c.TeamDeltas[0].DScore != 50_000 {
		t.Errorf("team deltas = %+v, want one with DScore=50000", c.TeamDeltas)
	}
}

func TestIdleMembersProduceNoDelta(t *testing.T) {
	s := NewState()
	in := users(
		parse.UserRow{Name: "busy", Score: 100, WUs: 1, TeamID: 1},
		parse.UserRow{Name: "idle", Score: 500, WUs: 5, TeamID: 1},
	)
	s.Apply(t0(), nil, in)
	c := s.Apply(t0().Add(time.Hour), nil, users(
		parse.UserRow{Name: "busy", Score: 300, WUs: 3, TeamID: 1},
		parse.UserRow{Name: "idle", Score: 500, WUs: 5, TeamID: 1},
	))
	// Sparsity is the whole basis of the storage and sliding-window budgets: at any
	// moment ~99% of donors are idle and must cost nothing.
	if len(c.MemberDeltas) != 1 {
		t.Fatalf("MemberDeltas = %+v, want only the active member", c.MemberDeltas)
	}
	if c.MemberDeltas[0].DScore != 200 {
		t.Errorf("delta = %+v, want DScore=200", c.MemberDeltas[0])
	}
}

func TestDuplicatePairsAreSummedNotDeduplicated(t *testing.T) {
	// 6,984 duplicate (name, team) pairs exist upstream: F@H filters names that
	// look like private info, collapsing distinct donors onto one name. Summing is
	// what reconciles against the authoritative team total; dropping rows would
	// silently lose production.
	s := NewState()
	s.Apply(t0(), nil, users(
		parse.UserRow{Name: "dupe", Score: 100, WUs: 1, TeamID: 7},
		parse.UserRow{Name: "dupe", Score: 200, WUs: 2, TeamID: 7},
	))
	if len(s.Members) != 1 {
		t.Fatalf("got %d members, want 1 (pairs collapse to one slot)", len(s.Members))
	}
	if s.Members[0].Score != 300 || s.Members[0].WUs != 3 {
		t.Errorf("member = %+v, want summed Score=300 WUs=3", s.Members[0])
	}

	c := s.Apply(t0().Add(time.Hour), nil, users(
		parse.UserRow{Name: "dupe", Score: 150, WUs: 1, TeamID: 7},
		parse.UserRow{Name: "dupe", Score: 250, WUs: 3, TeamID: 7},
	))
	if len(c.MemberDeltas) != 1 || c.MemberDeltas[0].DScore != 100 {
		t.Errorf("delta = %+v, want DScore=100 (400-300)", c.MemberDeltas)
	}
}

func TestSameNameOnDifferentTeamsIsDistinct(t *testing.T) {
	// The storage grain is (name, team). 15% of names appear on more than one team,
	// and merging them here would make the two levels of rollup impossible.
	s := NewState()
	s.Apply(t0(), nil, users(
		parse.UserRow{Name: "DH", Score: 100, WUs: 1, TeamID: 1},
		parse.UserRow{Name: "DH", Score: 900, WUs: 9, TeamID: 2},
	))
	if len(s.Members) != 2 {
		t.Fatalf("got %d members, want 2", len(s.Members))
	}
	if s.Names.Len() != 1 {
		t.Errorf("interned %d names, want 1 shared name", s.Names.Len())
	}
	a, _ := s.MemberSlot(0, 1)
	b, _ := s.MemberSlot(0, 2)
	if s.Members[a].Score != 100 || s.Members[b].Score != 900 {
		t.Errorf("scores not kept separate: %+v", s.Members)
	}
}

func TestScoreRegressionIsCounted(t *testing.T) {
	// Cumulative totals must never fall. If they do it means a feed glitch or a
	// name collision, and we surface it rather than clamping silently.
	s := NewState()
	s.Apply(t0(), nil, users(parse.UserRow{Name: "x", Score: 500, WUs: 5, TeamID: 1}))
	c := s.Apply(t0().Add(time.Hour), nil, users(parse.UserRow{Name: "x", Score: 400, WUs: 4, TeamID: 1}))
	if c.Regressions != 1 {
		t.Errorf("Regressions = %d, want 1", c.Regressions)
	}
	if len(c.MemberDeltas) != 1 || c.MemberDeltas[0].DScore != -100 {
		t.Errorf("delta = %+v, want the negative delta preserved", c.MemberDeltas)
	}
	// The count says something happened; the sample says who, which is the whole
	// difference between an alarm that can be acted on and one that cannot.
	if len(c.RegressedMembers) != 1 || c.RegressedMembers[0].DScore != -100 {
		t.Errorf("RegressedMembers = %+v, want the offending member sampled", c.RegressedMembers)
	}
}

func TestRegressionSampleIsCapped(t *testing.T) {
	// A systemic upstream fault must not log the corpus. The count keeps the scale.
	s := NewState()
	var first, second []parse.UserRow
	for i := range maxRegressionSample + 3 {
		first = append(first, parse.UserRow{Name: fmt.Sprintf("u%d", i), Score: 500, WUs: 5, TeamID: 1})
		second = append(second, parse.UserRow{Name: fmt.Sprintf("u%d", i), Score: 400, WUs: 4, TeamID: 1})
	}
	s.Apply(t0(), nil, users(first...))
	c := s.Apply(t0().Add(time.Hour), nil, users(second...))

	if c.Regressions != maxRegressionSample+3 {
		t.Errorf("Regressions = %d, want %d", c.Regressions, maxRegressionSample+3)
	}
	if len(c.RegressedMembers) != maxRegressionSample {
		t.Errorf("sampled %d, want the cap of %d", len(c.RegressedMembers), maxRegressionSample)
	}
}

func TestTeamRenameKeepsIdentityAndTotals(t *testing.T) {
	s := NewState()
	s.Apply(t0(), teams(parse.TeamRow{ID: 32, Name: "old", Score: 100, WUs: 1}), nil)
	c := s.Apply(t0().Add(time.Hour), teams(parse.TeamRow{ID: 32, Name: "new", Score: 150, WUs: 2}), nil)

	if len(s.Teams) != 1 {
		t.Fatalf("got %d teams, want 1 (rename must not fork identity)", len(s.Teams))
	}
	if got := s.Names.Name(s.Teams[0].NameID); got != "new" {
		t.Errorf("team name = %q, want %q", got, "new")
	}
	if len(c.TeamDeltas) != 1 || c.TeamDeltas[0].DScore != 50 {
		t.Errorf("team delta = %+v, want DScore=50 across the rename", c.TeamDeltas)
	}
}

func TestMemberJoiningSecondTeamStartsFresh(t *testing.T) {
	s := NewState()
	s.Apply(t0(), nil, users(parse.UserRow{Name: "DH", Score: 1000, WUs: 10, TeamID: 1}))
	c := s.Apply(t0().Add(time.Hour), nil, users(
		parse.UserRow{Name: "DH", Score: 1100, WUs: 11, TeamID: 1},
		parse.UserRow{Name: "DH", Score: 5000, WUs: 50, TeamID: 2},
	))
	// The new (name, team) pair is a first sighting even though the name is known,
	// so its 5000 lifetime points must not register as an hour's production.
	if len(c.MemberDeltas) != 1 {
		t.Fatalf("MemberDeltas = %+v, want only the existing pair", c.MemberDeltas)
	}
	if c.MemberDeltas[0].DScore != 100 {
		t.Errorf("delta = %+v, want DScore=100", c.MemberDeltas[0])
	}
	if len(c.NewMembers) != 1 {
		t.Errorf("NewMembers = %d, want 1", len(c.NewMembers))
	}
}
