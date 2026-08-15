package api

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// REST and MCP answer the same three questions, and a model quoting one while a bot
// quotes the other is the failure this project can least afford. They share a resolver
// and the goal arithmetic now; these hold them to it.
//
// Formatting is deliberately not compared. MCP writes prose with separators and REST
// emits integers, and that difference is the point of having two surfaces — what must
// match is the number underneath.

// num pulls one figure out of MCP's prose, with the separators removed.
func num(t *testing.T, text, pattern string) int64 {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no match for %q in:\n%s", pattern, text)
	}
	v, err := strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
	if err != nil {
		t.Fatalf("%q from %q: %v", m[1], pattern, err)
	}
	return v
}

func TestCompareAgreesAcrossRestAndMcp(t *testing.T) {
	srv := fixture(t)
	_, env := get(t, srv, "/v1/compare?kind=team&a=32&b=51")
	rest := decode[Comparison](t, env.Data)

	text, err := srv.Current().mcpCompare("teams", "32", "51")
	if err != nil {
		t.Fatal(err)
	}
	if gap := num(t, text, `ahead by ([\d,]+) points`); gap != rest.PointsGap {
		t.Errorf("points gap: REST %d, MCP %d", rest.PointsGap, gap)
	}
	// Both sides' figures must come from the same resolver, not just the difference.
	for _, want := range []int64{rest.A.PointsTotal, rest.B.PointsTotal} {
		if !strings.Contains(text, fmtInt(want)) {
			t.Errorf("MCP does not report %s points, which REST does:\n%s", fmtInt(want), text)
		}
	}
}

func TestGoalAgreesAcrossRestAndMcp(t *testing.T) {
	srv := fixture(t)
	_, env := get(t, srv, "/v1/goals?kind=donor&who=solo&overtake=DH&by=2026-08-04")
	rest := decode[Goal](t, env.Data)
	if rest.RequiredBy == nil {
		t.Fatal("REST returned no required rate")
	}

	text, err := srv.Current().mcpGoal("donors", "solo", 0, 0, "DH", "2026-08-04")
	if err != nil {
		t.Fatal(err)
	}
	// The moving-target correction is the whole reason this arithmetic is shared: a
	// naive gap ÷ days understates it, sometimes by more than the gap itself.
	if got := num(t, text, `([\d,]+)\s*(?:points\s*)?(?:a|per)\s*day`); got != *rest.RequiredBy {
		t.Errorf("required rate: REST %d, MCP %d", *rest.RequiredBy, got)
	}
}

// An entity resolved through one surface must be the same entity through the other,
// including for a rank lookup, which had its own copy until they were merged.
func TestEntityResolutionAgrees(t *testing.T) {
	snap := fixture(t).Current()

	rest, err := snap.insightEntity("team", "32")
	if err != nil {
		t.Fatal(err)
	}
	mcp, err := snap.mcpEntity("teams", "32")
	if err != nil {
		t.Fatal(err)
	}
	if mcp.score != rest.PointsTotal || mcp.rate != rest.PointsPerDay24hAvg || mcp.rank != rest.Rank {
		t.Errorf("team 32: REST %+v, MCP %+v", rest, mcp)
	}
	// MCP decorates the name with the number; REST carries it as a field. That is the
	// only difference either is allowed to have.
	if !strings.HasPrefix(mcp.name, rest.Name) {
		t.Errorf("MCP name %q does not start with REST name %q", mcp.name, rest.Name)
	}

	restAt, err := snap.insightAtRank("donor", 1)
	if err != nil {
		t.Fatal(err)
	}
	mcpAt, err := snap.atRank("donors", 1)
	if err != nil {
		t.Fatal(err)
	}
	if mcpAt.score != restAt.PointsTotal || mcpAt.rank != restAt.Rank {
		t.Errorf("rank 1: REST %+v, MCP %+v", restAt, mcpAt)
	}
}

// display must carry the rolling-day rate, which is the one every projection on both
// surfaces is built on.
//
// Asserted on a constructed entity rather than through the fixture: in a fixture whose
// cycles all sit inside both windows the 7-day and 24-hour rates are identical, so
// swapping them is invisible. That is exactly how this would regress unnoticed.
func TestDisplayCarriesTheRollingDayRate(t *testing.T) {
	id := int32(32)
	e := InsightEntity{Name: "Team", TeamID: &id, Rank: 3, PointsTotal: 10,
		PointsPerDay24hAvg: 24, PointsPerDay7dAvg: 7}
	got := e.display()
	if got.rate != 24 {
		t.Errorf("rate = %d, want the 24h figure 24 (7d is 7)", got.rate)
	}
	if got.score != 10 || got.rank != 3 {
		t.Errorf("display dropped a figure: %+v", got)
	}
	if got.name != "Team (team 32)" {
		t.Errorf("name = %q, want the number folded in", got.name)
	}
	// A donor has no team number, so nothing should be appended.
	if d := (InsightEntity{Name: "solo"}).display(); d.name != "solo" {
		t.Errorf("donor name = %q, want %q", d.name, "solo")
	}
}

// Both spellings of a kind must reach the same place, since that split is what let the
// two resolvers exist separately.
func TestKindSpellingsAgree(t *testing.T) {
	for _, pair := range [][2]string{{"team", "teams"}, {"donor", "donors"}} {
		a, err := insightKind(pair[0])
		if err != nil {
			t.Fatalf("%q: %v", pair[0], err)
		}
		b, err := insightKind(pair[1])
		if err != nil {
			t.Fatalf("%q: %v", pair[1], err)
		}
		if a != b {
			t.Errorf("%q resolved to %q but %q to %q", pair[0], a, pair[1], b)
		}
	}
	if _, err := insightKind("teamz"); err == nil {
		t.Error("teamz was accepted")
	}
}
