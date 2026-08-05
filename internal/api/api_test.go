package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"folding/internal/metrics"
	"folding/internal/model"
	"folding/internal/parse"
	"folding/internal/rank"
	"folding/internal/store"
)

func at(h int) time.Time { return time.Date(2026, 8, 3, h, 0, 0, 0, time.UTC) }

func u(name string, score int64, team int32) parse.UserRow {
	return parse.UserRow{Name: name, Score: score, WUs: score / 10, TeamID: team}
}

func tr(id int32, name string, score int64) parse.TeamRow {
	return parse.TeamRow{ID: id, Name: name, Score: score, WUs: score / 10}
}

// fixture builds a two-cycle world so rate windows are populated rather than all
// zero (a single cycle is all first sightings and produces no deltas).
func fixture(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	state := model.NewState()
	memberWin := metrics.New(0)
	teamWin := metrics.New(0)
	ctx := context.Background()

	cycles := []struct {
		when  time.Time
		teams []parse.TeamRow
		users []parse.UserRow
	}{
		{at(1),
			[]parse.TeamRow{tr(32, "overclockers", 1000), tr(51, "Alliance", 500)},
			[]parse.UserRow{u("DH", 600, 32), u("toTOW", 400, 32), u("DH", 300, 51), u("solo", 200, 51)},
		},
		{at(2),
			[]parse.TeamRow{tr(32, "overclockers", 1900), tr(51, "Alliance", 700)},
			[]parse.UserRow{u("DH", 1000, 32), u("toTOW", 900, 32), u("DH", 400, 51), u("solo", 300, 51)},
		},
	}
	for _, c := range cycles {
		cy := state.Apply(c.when, c.teams, c.users)
		if err := st.WriteCycle(ctx, state, cy, store.CycleMeta{
			TeamSnapshotAt: c.when, UserSnapshotAt: c.when}); err != nil {
			t.Fatal(err)
		}
		memberWin.Grow(len(state.Members))
		memberWin.Push(c.when, cy.MemberDeltas)
		teamWin.Grow(len(state.Teams))
		teamWin.Push(c.when, cy.TeamDeltas)
	}

	tbl := rank.Build(state, at(2), rank.DefaultConfig)
	// Mirror the service: it builds the period orderings on every publish, so a
	// fixture without them would let a sort bug pass unnoticed here.
	tbl.BuildOrders(state, memberWin, teamWin, nil, nil)
	snap := Build(state, memberWin, teamWin, tbl, st, at(2), at(3), "test-etag")

	srv := NewServer()
	srv.Publish(snap)
	return srv
}

func get(t *testing.T, srv *Server, path string) (*httptest.ResponseRecorder, Envelope) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var env Envelope
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: decoding body: %v\n%s", path, err, rec.Body.String())
		}
	}
	return rec, env
}

func decode[T any](t *testing.T, v any) T {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestUnpublishedServerReports503(t *testing.T) {
	srv := NewServer()
	rec, _ := get(t, srv, "/v1/summary")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 before any snapshot exists", rec.Code)
	}
}

func TestEveryResponseCarriesSnapshotMetadata(t *testing.T) {
	// Freshness travels with the data so a client never needs a second request to
	// know how old it is.
	srv := fixture(t)
	for _, path := range []string{"/v1/summary", "/v1/teams", "/v1/donors", "/v1/status"} {
		rec, env := get(t, srv, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		if !env.Snapshot.At.Equal(at(2)) {
			t.Errorf("%s: snapshot.at = %v, want %v", path, env.Snapshot.At, at(2))
		}
		if !env.Snapshot.NextExpectedAt.Equal(at(3)) {
			t.Errorf("%s: next_expected_at = %v, want %v", path, env.Snapshot.NextExpectedAt, at(3))
		}
		// Two cycles an hour apart is nowhere near a week, so the average is over a
		// partial window and the API must say so.
		if env.Snapshot.WarmingUp == nil || env.Snapshot.WarmingUp.HistorySpanSec == 0 {
			t.Errorf("%s: no warming_up block after 1 hour of history", path)
		}
	}
}

func TestConditionalRequestReturns304(t *testing.T) {
	// Data is immutable between cycles, so repeat traffic should never reach the
	// handlers at all.
	srv := fixture(t)
	rec, _ := get(t, srv, "/v1/summary")
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on response")
	}
	if cc := rec.Header().Get("Cache-Control"); cc == "" {
		t.Error("no Cache-Control on response")
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/summary", nil)
	req.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 carried a %d-byte body", rec2.Body.Len())
	}
}

func TestSummaryTotalsComeFromTeamFeed(t *testing.T) {
	srv := fixture(t)
	_, env := get(t, srv, "/v1/summary")
	got := decode[Summary](t, env.Data)

	// Team totals are authoritative: upstream publishes the two feeds a minute
	// apart, so summing member rows would disagree slightly.
	if got.PointsTotal != 1900+700 {
		t.Errorf("points_total = %d, want 2600", got.PointsTotal)
	}
	if got.TeamsTotal != 2 {
		t.Errorf("teams_total = %d, want 2", got.TeamsTotal)
	}
	// Three distinct names across four (name, team) pairs.
	if got.DonorsTotal != 3 {
		t.Errorf("donors_total = %d, want 3", got.DonorsTotal)
	}
	if got.MembersTotal != 4 {
		t.Errorf("members_total = %d, want 4", got.MembersTotal)
	}
}

func TestDonorAggregatesAcrossTeamsInOneRequest(t *testing.T) {
	// R9/R10: a donor's page must not need one request per team.
	srv := fixture(t)
	rec, env := get(t, srv, "/v1/donors/DH")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	got := decode[Donor](t, env.Data)

	if got.Name != "DH" {
		t.Errorf("name = %q", got.Name)
	}
	if got.PointsTotal != 1400 { // 1000 on team 32 + 400 on team 51
		t.Errorf("points_total = %d, want 1400", got.PointsTotal)
	}
	if got.TeamCount != 2 {
		t.Errorf("team_count = %d, want 2", got.TeamCount)
	}
	if len(got.Teams) != 2 {
		t.Fatalf("teams breakdown has %d entries, want 2", len(got.Teams))
	}
	byTeam := map[int32]Member{}
	for _, m := range got.Teams {
		byTeam[m.TeamID] = m
	}
	if byTeam[32].PointsTotal != 1000 || byTeam[51].PointsTotal != 400 {
		t.Errorf("breakdown = %+v", byTeam)
	}
	// The breakdown identifies each row by team name, which is the point of it.
	if byTeam[32].TeamName != "overclockers" {
		t.Errorf("team_name = %q, want overclockers", byTeam[32].TeamName)
	}
	// Rate windows must aggregate too: DH gained 400 on team 32 and 100 on 51.
	if got.PointsLast24h != 500 {
		t.Errorf("points_last_24h = %d, want 500", got.PointsLast24h)
	}
}

func TestCollectionListingIsPaginatedAndRanked(t *testing.T) {
	srv := fixture(t)
	rec, env := get(t, srv, "/v1/teams?page=1&per_page=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	teams := decode[[]Team](t, env.Data)
	if len(teams) != 1 {
		t.Fatalf("got %d teams, want 1", len(teams))
	}
	if teams[0].TeamID != 32 { // highest score
		t.Errorf("first team = %d, want 32", teams[0].TeamID)
	}
	if env.Page == nil {
		t.Fatal("no page info on a collection")
	}
	if env.Page.TotalItems != 2 || env.Page.TotalPages != 2 {
		t.Errorf("page = %+v, want 2 items over 2 pages", env.Page)
	}
}

func TestTeamMembersActiveFilter(t *testing.T) {
	srv := fixture(t)
	_, env := get(t, srv, "/v1/teams/32/members")
	all := decode[[]Member](t, env.Data)
	if len(all) != 2 {
		t.Fatalf("got %d members, want 2", len(all))
	}
	// Members arrive in rank order without a per-request sort.
	if all[0].PointsTotal < all[1].PointsTotal {
		t.Errorf("members not in rank order: %+v", all)
	}
	if all[0].RankInTeam != 1 {
		t.Errorf("top member rank_in_team = %d, want 1", all[0].RankInTeam)
	}

	_, env = get(t, srv, "/v1/teams/32/members?active_only=true")
	active := decode[[]Member](t, env.Data)
	if len(active) != 2 {
		t.Errorf("got %d active members, want 2 (both produced)", len(active))
	}
}

func TestSearchRanksExactMatchFirst(t *testing.T) {
	// Live results as you type make prefix matching useful rather than noisy — but
	// an exact hit must still lead and be identifiable, so "DH" never buries the
	// actual DH under everything starting with those letters.
	srv := fixture(t)

	_, env := get(t, srv, "/v1/search?q=DH")
	res := decode[SearchResults](t, env.Data)
	if len(res.Donors) == 0 {
		t.Fatal("exact 2-character search returned nothing")
	}
	if res.Donors[0].Name != "DH" {
		t.Errorf("first donor = %q, want the exact match DH", res.Donors[0].Name)
	}
	if !res.ExactDonor {
		t.Error("exact_donor = false for an exact match")
	}
}

func TestSearchFindsPrefixMatches(t *testing.T) {
	// Case-insensitive prefix matching is what makes the live dropdown useful.
	srv := fixture(t)

	_, env := get(t, srv, "/v1/search?q=to")
	res := decode[SearchResults](t, env.Data)
	found := false
	for _, d := range res.Donors {
		if d.Name == "toTOW" {
			found = true
		}
	}
	if !found {
		t.Errorf("prefix search for %q did not find toTOW: %+v", "to", res.Donors)
	}
	// A prefix-only result set must say so, or the UI cannot distinguish "your
	// donor" from "these look similar".
	if res.ExactDonor {
		t.Error("exact_donor = true for a prefix-only match")
	}

	// Case-insensitive.
	_, env = get(t, srv, "/v1/search?q=TOT")
	res = decode[SearchResults](t, env.Data)
	if len(res.Donors) == 0 {
		t.Error("case-insensitive prefix search returned nothing")
	}
}

func TestSearchTeamsByIDNameAndPrefix(t *testing.T) {
	srv := fixture(t)

	_, env := get(t, srv, "/v1/search?q=32&type=team")
	res := decode[SearchResults](t, env.Data)
	if len(res.Teams) == 0 || res.Teams[0].TeamID != 32 {
		t.Errorf("team id search = %+v", res.Teams)
	}
	if !res.ExactTeam {
		t.Error("exact_team = false for a numeric id hit")
	}

	_, env = get(t, srv, "/v1/search?q=Alliance&type=team")
	res = decode[SearchResults](t, env.Data)
	if len(res.Teams) == 0 || res.Teams[0].TeamID != 51 {
		t.Errorf("team name search = %+v", res.Teams)
	}

	// Prefix, lowercase.
	_, env = get(t, srv, "/v1/search?q=over&type=team")
	res = decode[SearchResults](t, env.Data)
	found := false
	for _, tm := range res.Teams {
		if tm.TeamID == 32 {
			found = true
		}
	}
	if !found {
		t.Errorf("team prefix search did not find overclockers: %+v", res.Teams)
	}
}

func TestSearchRespectsLimit(t *testing.T) {
	srv := fixture(t)
	_, env := get(t, srv, "/v1/search?q=&type=donor")
	_ = env
	rec, env := get(t, srv, "/v1/search?q=t&limit=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	res := decode[SearchResults](t, env.Data)
	if len(res.Donors) > 1 || len(res.Teams) > 1 {
		t.Errorf("limit=1 returned %d donors and %d teams", len(res.Donors), len(res.Teams))
	}

	rec, _ = get(t, srv, "/v1/search?q=t&limit=999")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized limit: status %d, want 400", rec.Code)
	}
}

func TestHistoryRoundTrip(t *testing.T) {
	srv := fixture(t)
	_, env := get(t, srv,
		"/v1/teams/32/history?granularity=cycle&from=2026-08-03T00:00:00Z&to=2026-08-03T23:00:00Z")
	h := decode[History](t, env.Data)
	if h.Granularity != "hourly" {
		t.Errorf("granularity = %q, want hourly", h.Granularity)
	}
	if len(h.Points) != 1 {
		t.Fatalf("got %d points, want 1 (only the second cycle has a delta)", len(h.Points))
	}
	if h.Points[0].Points != 900 {
		t.Errorf("delta = %d, want 900", h.Points[0].Points)
	}
}

func TestDonorHistoryMergesTeamsAndFilters(t *testing.T) {
	srv := fixture(t)
	q := "granularity=cycle&from=2026-08-03T00:00:00Z&to=2026-08-03T23:00:00Z"

	_, env := get(t, srv, "/v1/donors/DH/history?"+q)
	h := decode[History](t, env.Data)
	var total int64
	for _, p := range h.Points {
		total += p.Points
	}
	if total != 500 { // 400 on team 32 + 100 on team 51
		t.Errorf("merged history total = %d, want 500", total)
	}

	// Restricting to one team is what makes per-team tabs work.
	_, env = get(t, srv, "/v1/donors/DH/history?team_id=51&"+q)
	h = decode[History](t, env.Data)
	total = 0
	for _, p := range h.Points {
		total += p.Points
	}
	if total != 100 {
		t.Errorf("team-filtered history = %d, want 100", total)
	}
}

func TestErrorsAreStructured(t *testing.T) {
	srv := fixture(t)
	for _, tc := range []struct {
		path string
		code int
	}{
		{"/v1/teams/999999", http.StatusNotFound},
		{"/v1/teams/notanumber", http.StatusBadRequest},
		{"/v1/donors/nobody", http.StatusNotFound},
		{"/v1/search", http.StatusBadRequest},
		{"/v1/teams?page=0", http.StatusBadRequest},
		{"/v1/teams?per_page=99999", http.StatusBadRequest},
		{"/v1/teams/32/history?granularity=fortnightly", http.StatusBadRequest},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != tc.code {
			t.Errorf("%s: status = %d, want %d", tc.path, rec.Code, tc.code)
		}
		var e APIError
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
			t.Errorf("%s: error body not JSON: %s", tc.path, rec.Body.String())
			continue
		}
		if e.Error == "" || e.Message == "" {
			t.Errorf("%s: incomplete error body %+v", tc.path, e)
		}
	}
}

func TestPointsPerDayDividesByTheObservedPeriod(t *testing.T) {
	// The fixture spans two hourly cycles, and the first is all first sightings, so
	// one hour of production has actually been observed. Dividing by a flat seven
	// days would report a 168th of the real rate — and then creep up to the truth
	// over the following week, which is precisely when a new donor is looking.
	srv := fixture(t)
	_, env := get(t, srv, "/v1/donors/DH")
	d := decode[Donor](t, env.Data)

	if d.PointsLast7d != 500 {
		t.Fatalf("fixture changed: points_last_7d = %d, want 500", d.PointsLast7d)
	}
	// 500 points in the one observed hour is 12,000 a day.
	if d.PointsPerDay7dAvg != 12_000 {
		t.Errorf("points_per_day_7d_avg = %d, want 12000 (500 over one observed hour)",
			d.PointsPerDay7dAvg)
	}
	if flat := (d.PointsLast7d + 3) / 7; d.PointsPerDay7dAvg == flat {
		t.Errorf("points_per_day_7d_avg = %d, which is last7d/7 — six days nobody was watching", flat)
	}

	// Teams and members travel separate paths to the same divisor and must agree.
	_, env = get(t, srv, "/v1/teams/32")
	team := decode[Team](t, env.Data)
	if team.PointsPerDay7dAvg != team.PointsLast7d*24 {
		t.Errorf("team per-day = %d, want %d", team.PointsPerDay7dAvg, team.PointsLast7d*24)
	}

	// And the project summary, which sums totals rather than reading a window.
	_, env = get(t, srv, "/v1/summary")
	sum := decode[Summary](t, env.Data)
	if sum.PointsPerDay7dAvg != sum.PointsLast7d*24 {
		t.Errorf("summary per-day = %d, want %d", sum.PointsPerDay7dAvg, sum.PointsLast7d*24)
	}
}

func TestNamesWithPathologicalCharactersAreAddressable(t *testing.T) {
	// Real donor names contain tabs, newlines and slashes. They have to survive URL
	// encoding or those donors would be unreachable through the API.
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	state := model.NewState()
	memberWin := metrics.New(0)
	teamWin := metrics.New(0)
	rows := []parse.UserRow{u("/\ndy-Houston", 100, 1), u("oslo\t60p", 50, 1)}
	cy := state.Apply(at(1), nil, rows)
	memberWin.Grow(len(state.Members))
	memberWin.Push(at(1), cy.MemberDeltas)
	if err := st.WriteCycle(context.Background(), state, cy, store.CycleMeta{}); err != nil {
		t.Fatal(err)
	}
	tbl := rank.Build(state, at(1), rank.DefaultConfig)
	srv := NewServer()
	srv.Publish(Build(state, memberWin, teamWin, tbl, st, at(1), at(2), "e"))

	for _, name := range []string{"/\ndy-Houston", "oslo\t60p"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/donors/"+urlEscape(name), nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("donor %q: status %d, want 200", name, rec.Code)
			continue
		}
		var env Envelope
		json.Unmarshal(rec.Body.Bytes(), &env)
		if got := decode[Donor](t, env.Data); got.Name != name {
			t.Errorf("round-tripped name = %q, want %q", got.Name, name)
		}
	}
}

func urlEscape(s string) string { return url.PathEscape(s) }

func TestTeamAndMemberRatesDoNotAlias(t *testing.T) {
	// Team slots and member slots are separate id spaces that both start at zero.
	// Sharing one metrics window makes a team report a member's production —
	// invisible in unit tests with one entity, glaring on real data where team
	// slot 0 ("Default") reported member slot 0's ("Anonymous") rate.
	srv := fixture(t)

	_, env := get(t, srv, "/v1/teams/32")
	team := decode[Team](t, env.Data)
	// Team 32 went 1000 -> 1900.
	if team.PointsLast24h != 900 {
		t.Errorf("team points_last_24h = %d, want 900", team.PointsLast24h)
	}
	_, env = get(t, srv, "/v1/teams/51")
	team51 := decode[Team](t, env.Data)
	// Team 51 went 500 -> 700. A shared window would give this the wrong slot's
	// figure, or zero.
	if team51.PointsLast24h != 200 {
		t.Errorf("team 51 points_last_24h = %d, want 200", team51.PointsLast24h)
	}

	// Members must still carry their own rates, unaffected by the team window.
	_, env = get(t, srv, "/v1/teams/32/members")
	members := decode[[]Member](t, env.Data)
	byName := map[string]Member{}
	for _, m := range members {
		byName[m.Name] = m
	}
	if got := byName["toTOW"].PointsLast24h; got != 500 { // 400 -> 900
		t.Errorf("toTOW points_last_24h = %d, want 500", got)
	}
	if got := byName["DH"].PointsLast24h; got != 400 { // 600 -> 1000
		t.Errorf("DH-on-32 points_last_24h = %d, want 400", got)
	}
}

func TestSummaryRatesComeFromTeamWindow(t *testing.T) {
	// Site totals aggregate teams, so they must reflect the team window: 900 + 200.
	srv := fixture(t)
	_, env := get(t, srv, "/v1/summary")
	got := decode[Summary](t, env.Data)
	if got.PointsLast24h != 1100 {
		t.Errorf("summary points_last_24h = %d, want 1100", got.PointsLast24h)
	}
	if got.TeamsActive != 2 {
		t.Errorf("teams_active = %d, want 2", got.TeamsActive)
	}
}

func TestDonorBreakdownIsCappedAndOrdered(t *testing.T) {
	// A shared placeholder name spans thousands of teams upstream; embedding every
	// row would make a single response megabytes. The inline list is capped and the
	// full set moves to a paginated endpoint.
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var rows []parse.UserRow
	for i := int32(0); i < maxEmbeddedTeams+50; i++ {
		rows = append(rows, u("PS3", int64(i+1), i+1))
	}
	state := model.NewState()
	memberWin, teamWin := metrics.New(0), metrics.New(0)
	cy := state.Apply(at(1), nil, rows)
	memberWin.Grow(len(state.Members))
	memberWin.Push(at(1), cy.MemberDeltas)
	if err := st.WriteCycle(context.Background(), state, cy, store.CycleMeta{}); err != nil {
		t.Fatal(err)
	}
	tbl := rank.Build(state, at(1), rank.DefaultConfig)
	srv := NewServer()
	srv.Publish(Build(state, memberWin, teamWin, tbl, st, at(1), at(2), "e"))

	_, env := get(t, srv, "/v1/donors/PS3")
	d := decode[Donor](t, env.Data)

	if len(d.Teams) != maxEmbeddedTeams {
		t.Errorf("inline breakdown = %d rows, want %d", len(d.Teams), maxEmbeddedTeams)
	}
	if !d.TeamsTruncated {
		t.Error("teams_truncated not set on a capped breakdown")
	}
	if d.TeamCount != maxEmbeddedTeams+50 {
		t.Errorf("team_count = %d, want %d", d.TeamCount, maxEmbeddedTeams+50)
	}
	// Truncation must drop the least significant rows, so the list is ordered by
	// points before the cap is applied.
	for i := 1; i < len(d.Teams); i++ {
		if d.Teams[i-1].PointsTotal < d.Teams[i].PointsTotal {
			t.Fatalf("breakdown not ordered by points at %d", i)
		}
	}

	// The full list is reachable, paginated.
	_, env = get(t, srv, "/v1/donors/PS3/teams?per_page=1000")
	all := decode[[]Member](t, env.Data)
	if len(all) != maxEmbeddedTeams+50 {
		t.Errorf("paginated breakdown = %d rows, want %d", len(all), maxEmbeddedTeams+50)
	}
	if env.Page == nil || env.Page.TotalItems != maxEmbeddedTeams+50 {
		t.Errorf("page = %+v", env.Page)
	}
}

func TestSmallDonorBreakdownIsNotTruncated(t *testing.T) {
	srv := fixture(t)
	_, env := get(t, srv, "/v1/donors/DH")
	d := decode[Donor](t, env.Data)
	if d.TeamsTruncated {
		t.Error("teams_truncated set on a 2-team donor")
	}
	if len(d.Teams) != 2 {
		t.Errorf("breakdown = %d rows, want 2", len(d.Teams))
	}
}

func TestHistoryRangeIsBounded(t *testing.T) {
	// An unbounded cycle-granularity range would scan every delta an entity ever
	// produced — a cheap way for one request to become expensive for everyone.
	srv := fixture(t)
	rec, _ := get(t, srv,
		"/v1/teams/32/history?granularity=cycle&from=2000-01-01T00:00:00Z&to=2026-08-03T00:00:00Z")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d for an over-long cycle range, want 400", rec.Code)
	}
	// Coarser buckets return fewer rows per unit time, so they may span more.
	rec, _ = get(t, srv,
		"/v1/teams/32/history?granularity=monthly&from=2000-01-01T00:00:00Z&to=2026-08-03T00:00:00Z")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d for a long monthly range, want 200", rec.Code)
	}
}

func TestDonorHistoryUsesOneAggregatedQuery(t *testing.T) {
	// Merging happens in SQL; the handler must not loop per team.
	srv := fixture(t)
	_, env := get(t, srv,
		"/v1/donors/DH/history?granularity=cycle&from=2026-08-03T00:00:00Z&to=2026-08-03T23:00:00Z")
	h := decode[History](t, env.Data)
	var total int64
	for _, p := range h.Points {
		total += p.Points
	}
	if total != 500 { // 400 on team 32 + 100 on team 51
		t.Errorf("merged history total = %d, want 500", total)
	}
	// Buckets must be merged, not duplicated per team.
	seen := map[int64]bool{}
	for _, p := range h.Points {
		if seen[p.At.Unix()] {
			t.Errorf("duplicate bucket at %v: teams were not merged", p.At)
		}
		seen[p.At.Unix()] = true
	}
}

func TestGranularityAliasAndValidation(t *testing.T) {
	// "hourly" is the documented name; "cycle" was the original and still works so
	// existing callers do not break.
	srv := fixture(t)
	q := "&from=2026-08-03T00:00:00Z&to=2026-08-03T23:00:00Z"

	var totals []int64
	for _, g := range []string{"hourly", "cycle"} {
		rec, env := get(t, srv, "/v1/teams/32/history?granularity="+g+q)
		if rec.Code != http.StatusOK {
			t.Fatalf("granularity=%s: status %d", g, rec.Code)
		}
		h := decode[History](t, env.Data)
		// Both report the canonical name, so responses never disagree about what
		// the buckets are.
		if h.Granularity != "hourly" {
			t.Errorf("granularity=%s reported %q, want hourly", g, h.Granularity)
		}
		var total int64
		for _, p := range h.Points {
			total += p.Points
		}
		totals = append(totals, total)
	}
	if totals[0] != totals[1] || totals[0] == 0 {
		t.Errorf("alias returned different data: %v", totals)
	}

	// Every accepted granularity must have a configured range limit; a missing one
	// would silently reject all ranges.
	for _, g := range []string{"hourly", "daily", "weekly", "monthly"} {
		rec, _ := get(t, srv, "/v1/teams/32/history?granularity="+g)
		if rec.Code != http.StatusOK {
			t.Errorf("granularity=%s with default range: status %d, want 200", g, rec.Code)
		}
	}
	rec, _ := get(t, srv, "/v1/teams/32/history?granularity=fortnightly")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown granularity: status %d, want 400", rec.Code)
	}
}

func TestDonorTeamsSortByProduction(t *testing.T) {
	// A card titled "production by team" must rank teams by production. Ordering it
	// by lifetime points selects the teams with nothing to plot: in the reference
	// corpus one donor's largest lifetime team had produced nothing all week, while
	// 87% of its current output came from a team far down that ranking.
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	state := model.NewState()
	memberWin, teamWin := metrics.New(0), metrics.New(0)
	ctx := context.Background()

	// Team 1: enormous lifetime total, dormant. Team 2: small, actively producing.
	cycles := []struct {
		when time.Time
		rows []parse.UserRow
	}{
		{at(1), []parse.UserRow{u("DH", 1_000_000, 1), u("DH", 100, 2)}},
		{at(2), []parse.UserRow{u("DH", 1_000_000, 1), u("DH", 900, 2)}},
	}
	for _, c := range cycles {
		cy := state.Apply(c.when, nil, c.rows)
		if err := st.WriteCycle(ctx, state, cy, store.CycleMeta{
			TeamSnapshotAt: c.when, UserSnapshotAt: c.when}); err != nil {
			t.Fatal(err)
		}
		memberWin.Grow(len(state.Members))
		memberWin.Push(c.when, cy.MemberDeltas)
	}

	tbl := rank.Build(state, at(2), rank.DefaultConfig)
	srv := NewServer()
	srv.Publish(Build(state, memberWin, teamWin, tbl, st, at(2), at(3), "e"))

	// Default ordering is lifetime points: the dormant giant leads.
	_, env := get(t, srv, "/v1/donors/DH/teams")
	byPoints := decode[[]Member](t, env.Data)
	if len(byPoints) != 2 || byPoints[0].TeamID != 1 {
		t.Fatalf("default order = %+v, want team 1 first", byPoints)
	}

	// By production, the active team leads even though it is tiny.
	_, env = get(t, srv, "/v1/donors/DH/teams?sort=production")
	byProd := decode[[]Member](t, env.Data)
	if len(byProd) != 2 {
		t.Fatalf("got %d teams", len(byProd))
	}
	if byProd[0].TeamID != 2 {
		t.Errorf("production order = team %d first, want team 2 (the only producer)", byProd[0].TeamID)
	}
	if byProd[0].PointsLast7d == 0 {
		t.Error("leading team by production has no recent production")
	}

	rec, _ := get(t, srv, "/v1/donors/DH/teams?sort=nonsense")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad sort: status %d, want 400", rec.Code)
	}
}

func TestProjectHistoryIsNotTeamZero(t *testing.T) {
	// The overview's chart is the project's own series. Standing in team 0 — the
	// "no team specified" bucket — understates the project by whatever share the
	// rest of the teams contribute, while claiming to be the whole thing.
	srv := fixture(t)
	q := "?granularity=hourly&from=2026-08-03T00:00:00Z&to=2026-08-03T23:00:00Z"

	_, env := get(t, srv, "/v1/summary/history"+q)
	project := decode[History](t, env.Data)
	var projectTotal int64
	for _, p := range project.Points {
		projectTotal += p.Points
	}

	// The fixture has two teams: 32 (+900) and 51 (+200).
	var teamsTotal int64
	for _, id := range []int{32, 51} {
		_, env := get(t, srv, fmt.Sprintf("/v1/teams/%d/history%s", id, q))
		h := decode[History](t, env.Data)
		for _, p := range h.Points {
			teamsTotal += p.Points
		}
	}

	if projectTotal != teamsTotal {
		t.Errorf("project history %d != sum of teams %d", projectTotal, teamsTotal)
	}
	if projectTotal != 1100 {
		t.Errorf("project total = %d, want 1100", projectTotal)
	}
	// And it must exceed any single team, or it is not the project.
	_, env = get(t, srv, "/v1/teams/32/history"+q)
	var one int64
	for _, p := range decode[History](t, env.Data).Points {
		one += p.Points
	}
	if projectTotal <= one {
		t.Errorf("project total %d is not greater than one team's %d", projectTotal, one)
	}
}

// TestStatusIsNeverCached protects the freshness probe.
//
// /v1/status answers "has anything changed yet". A cached response to that question
// is not a cheap answer but a wrong one: a client polling for a publish would be
// handed its own previous result and wait forever.
func TestStatusIsNeverCached(t *testing.T) {
	srv := fixture(t)

	res, _ := get(t, srv, "/v1/status")
	if cc := res.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("/v1/status cache-control = %q, want no-store", cc)
	}

	// Data routes keep caching — they are immutable between cycles, which is the
	// whole reason the probe exists separately.
	res, _ = get(t, srv, "/v1/summary")
	if cc := res.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Errorf("/v1/summary cache-control = %q, want a max-age", cc)
	}
}

// TestPostsCacheOnTheirOwnIdentity keeps article caching off the data schedule.
//
// Posts change when the binary does; the snapshot timestamp says nothing about
// whether an article was edited. Keyed to the snapshot, an edited post stayed hidden
// behind a max-age derived from the next expected feed — up to an hour of serving
// text that had already been replaced.
func TestPostsCacheOnTheirOwnIdentity(t *testing.T) {
	srv := fixture(t)

	res, _ := get(t, srv, "/v1/posts")
	if cc := res.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("/v1/posts cache-control = %q, want no-cache", cc)
	}
	postETag := res.Header().Get("ETag")
	if !strings.Contains(postETag, "-posts-") {
		t.Errorf("/v1/posts etag = %q, want a content fingerprint", postETag)
	}

	// The data routes keep the snapshot's identity, which is right for them.
	res, _ = get(t, srv, "/v1/summary")
	if dataETag := res.Header().Get("ETag"); dataETag == postETag {
		t.Errorf("posts and data share the etag %q", dataETag)
	}
	if cc := res.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Errorf("/v1/summary cache-control = %q, want a max-age", cc)
	}
}

func TestCompression(t *testing.T) {
	srv := fixture(t)

	// A body over the threshold, asked for gzipped, comes back gzipped and readable.
	req := httptest.NewRequest(http.MethodGet, "/v1/teams/32/members?per_page=1000", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	big := rec.Body.Len()
	if enc := rec.Header().Get("Content-Encoding"); enc == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("response claims gzip but does not decode: %v", err)
		}
		out, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("reading gzip body: %v", err)
		}
		if !json.Valid(out) {
			t.Error("decompressed body is not valid JSON")
		}
		if len(out) <= big {
			t.Errorf("compressed %d >= original %d — not actually a saving", big, len(out))
		}
	}

	// Vary must be present either way, or a shared cache can hand a gzipped body to
	// a client that never asked for one.
	if v := rec.Header().Get("Vary"); !strings.Contains(v, "Accept-Encoding") {
		t.Errorf("Vary = %q, want it to include Accept-Encoding", v)
	}

	// A client that did not ask must get plain bytes.
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/v1/teams/32/members?per_page=1000", nil))
	if enc := rec2.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("unrequested Content-Encoding = %q", enc)
	}
	if !json.Valid(rec2.Body.Bytes()) {
		t.Error("uncompressed body is not valid JSON")
	}
}

func TestCompressionSkipsSmallBodies(t *testing.T) {
	// Over half of real traffic is sub-kilobyte. Compressing those spends CPU on
	// every request to save a fraction of one packet.
	srv := fixture(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/summary", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Body.Len() >= compressMinBytes {
		t.Skipf("summary is %d bytes, at or over the threshold", rec.Body.Len())
	}
	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("small body was compressed anyway (Content-Encoding %q)", enc)
	}
}

func TestETagDistinguishesEncodings(t *testing.T) {
	// A gzipped body is a different entity. Vary handles caches that honour it; a
	// distinct validator means the ones that do not simply miss rather than serve
	// bytes the client cannot read.
	srv := fixture(t)

	plain := httptest.NewRecorder()
	srv.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/v1/summary", nil))

	req := httptest.NewRequest(http.MethodGet, "/v1/summary", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	gz := httptest.NewRecorder()
	srv.ServeHTTP(gz, req)

	if plain.Header().Get("ETag") == gz.Header().Get("ETag") {
		t.Errorf("both encodings share the ETag %q", plain.Header().Get("ETag"))
	}

	// Conditional requests must still work per encoding.
	cond := httptest.NewRequest(http.MethodGet, "/v1/summary", nil)
	cond.Header.Set("Accept-Encoding", "gzip")
	cond.Header.Set("If-None-Match", gz.Header().Get("ETag"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, cond)
	if rec.Code != http.StatusNotModified {
		t.Errorf("conditional gzip request = %d, want 304", rec.Code)
	}
}

func TestAcceptsGzipTokenParsing(t *testing.T) {
	cases := map[string]bool{
		"gzip":                true,
		"gzip, deflate, br":   true,
		"deflate, gzip;q=1.0": true,
		"":                    false,
		"deflate":             false,
		"gzip;q=0":            false, // an explicit refusal
		"x-gzip-not-really":   false, // substring, not a token
	}
	for header, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if header != "" {
			r.Header.Set("Accept-Encoding", header)
		}
		if got := acceptsGzip(r); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}

// TestEnvelopeCarriesNoDerivableFields keeps the block small.
//
// It rides on every response and most responses are under a kilobyte, so a field that
// duplicates another is not a rounding error — interval_sec was 42% of a team lookup's
// snapshot block and exactly next_expected_at minus at.
func TestEnvelopeCarriesNoDerivableFields(t *testing.T) {
	srv := fixture(t)
	res, _ := get(t, srv, "/v1/summary")

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(res.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var snap map[string]any
	if err := json.Unmarshal(raw["snapshot"], &snap); err != nil {
		t.Fatal(err)
	}

	for _, gone := range []string{"interval_sec", "interval_measured", "avg_window_complete", "history_span_sec"} {
		if _, present := snap[gone]; present {
			t.Errorf("snapshot still carries %q at the top level", gone)
		}
	}
	for _, required := range []string{"at", "next_expected_at", "stale", "server_time"} {
		if _, present := snap[required]; !present {
			t.Errorf("snapshot is missing %q", required)
		}
	}
}

// TestWarmingUpDisappearsWhenWarm is the property that makes omitting it safe.
//
// The block is a *presence* signal deliberately: a client testing
// `if (!snapshot.avg_window_complete)` against an omitted boolean would read absent as
// incomplete and invert the meaning. Testing for an object has no such trap.
func TestWarmingUpDisappearsWhenWarm(t *testing.T) {
	srv := fixture(t)
	snap := srv.Current()

	// The fixture has an hour of history, so it must be present.
	if warmingUp(snap) == nil {
		t.Fatal("warming_up absent with one hour of history")
	}

	// With a full window and a measured cadence there is nothing left to qualify.
	warm := *snap
	warm.IntervalMeasured = true
	if w := warmingUp(&warm); w != nil && w.HistorySpanSec == 0 && !w.IntervalEstimated {
		t.Error("warming_up returned an empty object; it should be nil")
	}
}

// rankChangeFixture builds a world whose two cycles are more than a day apart, so a
// 24h baseline exists and rank movement is actually reportable. The shared fixture
// deliberately spans one hour, which is the opposite case.
func rankChangeFixture(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	state := model.NewState()
	memberWin, teamWin := metrics.New(0), metrics.New(0)
	ctx := context.Background()

	cycles := []struct {
		when  time.Time
		teams []parse.TeamRow
		users []parse.UserRow
	}{
		{at(1),
			[]parse.TeamRow{tr(32, "overclockers", 500), tr(51, "Alliance", 900)},
			[]parse.UserRow{u("climber", 100, 32), u("faller", 800, 51)},
		},
		// 25 hours on: the first cycle ages out of the rolling window and becomes the
		// baseline. climber overtakes faller, so both must report movement.
		{at(1).Add(25 * time.Hour),
			[]parse.TeamRow{tr(32, "overclockers", 5000), tr(51, "Alliance", 900)},
			[]parse.UserRow{u("climber", 4000, 32), u("faller", 800, 51), u("newcomer", 9000, 32)},
		},
	}
	var last time.Time
	for _, c := range cycles {
		cy := state.Apply(c.when, c.teams, c.users)
		if err := st.WriteCycle(ctx, state, cy, store.CycleMeta{
			TeamSnapshotAt: c.when, UserSnapshotAt: c.when}); err != nil {
			t.Fatal(err)
		}
		memberWin.Grow(len(state.Members))
		memberWin.Push(c.when, cy.MemberDeltas)
		teamWin.Grow(len(state.Teams))
		teamWin.Push(c.when, cy.TeamDeltas)
		last = c.when
	}

	tbl := rank.Build(state, last, rank.DefaultConfig)
	tbl.BuildChange24h(state, memberWin, teamWin)

	srv := NewServer()
	srv.Publish(Build(state, memberWin, teamWin, tbl, st, last, last.Add(time.Hour), "rc-etag"))
	return srv
}

func TestRankChange24hOnTheWire(t *testing.T) {
	srv := rankChangeFixture(t)

	// Rank movement is movement in position, not in who you beat. climber overtakes
	// faller but a newcomer lands above both, so climber holds position 2 and reports
	// no movement — while faller drops two, one to each. Asserting the arithmetic
	// rather than just the sign is the point: an entity that gained ground on a rival
	// while the list grew above it genuinely has not climbed.
	for _, tc := range []struct {
		donor string
		want  float64
	}{
		{"climber", 0},
		{"faller", -2},
	} {
		_, env := get(t, srv, "/v1/donors/"+tc.donor)
		d := env.Data.(map[string]any)
		got, ok := d["rank_change_24h"]
		if !ok {
			t.Errorf("%s: rank_change_24h absent, want %+v", tc.donor, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: rank_change_24h = %v, want %+v", tc.donor, got, tc.want)
		}
	}

	// An entity younger than the baseline has no earlier rank. The field must be
	// absent rather than 0, which would read as "held its position".
	_, env := get(t, srv, "/v1/donors/newcomer")
	if v, ok := env.Data.(map[string]any)["rank_change_24h"]; ok {
		t.Errorf("newcomer: rank_change_24h = %v, want the field to be absent", v)
	}

	// Teams travel the same path through a different window, and no team is created
	// in the second cycle — so this is the clean overtake: 32 passes 51, +1 and -1.
	for _, tc := range []struct {
		id   string
		want float64
	}{
		{"32", 1},
		{"51", -1},
	} {
		_, env := get(t, srv, "/v1/teams/"+tc.id)
		got, ok := env.Data.(map[string]any)["rank_change_24h"]
		if !ok {
			t.Errorf("team %s: rank_change_24h absent, want %+v", tc.id, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("team %s: rank_change_24h = %v, want %+v", tc.id, got, tc.want)
		}
	}
}

func TestRankChangeUnavailableIsAdvertisedWhileWarmingUp(t *testing.T) {
	// The one-hour fixture cannot compute movement for anyone. A client seeing the
	// field missing everywhere needs to tell "nobody moved" from "we cannot say yet".
	_, env := get(t, fixture(t), "/v1/donors")
	if env.Snapshot.WarmingUp == nil || !env.Snapshot.WarmingUp.RankChange24hUnavailable {
		t.Errorf("warming_up.rank_change_24h_unavailable not set with under 24h of cycles: %+v",
			env.Snapshot.WarmingUp)
	}

	// And it must clear once a baseline exists, or it would be permanent noise.
	_, env = get(t, rankChangeFixture(t), "/v1/donors")
	if env.Snapshot.WarmingUp != nil && env.Snapshot.WarmingUp.RankChange24hUnavailable {
		t.Error("rank_change_24h_unavailable still set once a 24h baseline exists")
	}
}

// periodFixture makes lifetime order and daily order disagree, which the shared
// fixture cannot: there DH's 400+100 across two teams ties toTOW's 500, and a tie
// resolves to lifetime order whether the sort works or not.
func periodFixture(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	state := model.NewState()
	memberWin, teamWin := metrics.New(0), metrics.New(0)
	ctx := context.Background()

	cycles := []struct {
		when  time.Time
		teams []parse.TeamRow
		users []parse.UserRow
	}{
		{at(1),
			[]parse.TeamRow{tr(1, "steady", 1000), tr(2, "surging", 100)},
			[]parse.UserRow{u("steady", 1000, 1), u("surging", 100, 2)},
		},
		// Same UTC day, so this cycle is the whole of today and of this week.
		// steady keeps the lifetime lead; surging out-produces it eightfold.
		{at(2),
			[]parse.TeamRow{tr(1, "steady", 1100), tr(2, "surging", 900)},
			[]parse.UserRow{u("steady", 1100, 1), u("surging", 900, 2)},
		},
	}
	for _, c := range cycles {
		cy := state.Apply(c.when, c.teams, c.users)
		if err := st.WriteCycle(ctx, state, cy, store.CycleMeta{
			TeamSnapshotAt: c.when, UserSnapshotAt: c.when}); err != nil {
			t.Fatal(err)
		}
		memberWin.Grow(len(state.Members))
		memberWin.Push(c.when, cy.MemberDeltas)
		teamWin.Grow(len(state.Teams))
		teamWin.Push(c.when, cy.TeamDeltas)
	}

	teamMonth, err := st.MonthTotals(ctx, "team", at(2), len(state.Teams))
	if err != nil {
		t.Fatal(err)
	}
	memberMonth, err := st.MonthTotals(ctx, "member", at(2), len(state.Members))
	if err != nil {
		t.Fatal(err)
	}

	tbl := rank.Build(state, at(2), rank.DefaultConfig)
	tbl.BuildOrders(state, memberWin, teamWin, teamMonth, memberMonth)
	snap := Build(state, memberWin, teamWin, tbl, st, at(2), at(3), "period-etag")
	snap.TeamMonth, snap.MemberMonth = teamMonth, memberMonth

	srv := NewServer()
	srv.Publish(snap)
	return srv
}

func TestLeaderboardSortByPeriod(t *testing.T) {
	srv := periodFixture(t)

	order := func(path string) []string {
		t.Helper()
		_, env := get(t, srv, path)
		var names []string
		for _, row := range env.Data.([]any) {
			names = append(names, row.(map[string]any)["name"].(string))
		}
		return names
	}

	for _, ep := range []string{"/v1/donors", "/v1/teams"} {
		lifetime := order(ep)
		if len(lifetime) != 2 {
			t.Fatalf("%s: got %d rows, want 2", ep, len(lifetime))
		}
		if lifetime[0] != "steady" {
			t.Errorf("%s: lifetime leader = %s, want steady", ep, lifetime[0])
		}
		// An omitted sort must keep meaning what it always meant.
		if got := order(ep + "?sort=lifetime"); got[0] != lifetime[0] {
			t.Errorf("%s: explicit lifetime %v differs from default %v", ep, got, lifetime)
		}
		// Both cycles fall on one UTC day, so daily, weekly and monthly all cover
		// exactly the second cycle — and all three must reorder the board.
		for _, period := range []string{"daily", "weekly", "monthly"} {
			got := order(ep + "?sort=" + period)
			if len(got) != len(lifetime) {
				t.Errorf("%s sort=%s: %d rows, want %d — an ordering must not drop anyone",
					ep, period, len(got), len(lifetime))
			}
			if got[0] != "surging" {
				t.Errorf("%s sort=%s: leader = %s, want surging (800 this period vs 100)",
					ep, period, got[0])
			}
		}
	}

	rec, _ := get(t, srv, "/v1/donors?sort=yearly")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown sort: status %d, want 400", rec.Code)
	}
}

func TestPointsThisMonthIsReported(t *testing.T) {
	_, env := get(t, fixture(t), "/v1/teams/32")
	if _, ok := env.Data.(map[string]any)["points_this_month_utc"]; !ok {
		t.Error("points_this_month_utc missing from a team response")
	}
}

func TestValidatorsChangeWhenTheBinaryDoes(t *testing.T) {
	// Every figure served here is derived, not stored, so a response is a function of
	// the snapshot *and* of the code that computed it. Keyed on the snapshot alone, a
	// deploy that changed a derivation left every cached copy answering 304 with
	// numbers that no longer existed — the client asks whether anything changed, is
	// told no, and keeps the stale answer until upstream happens to publish.
	srv := fixture(t)

	for _, path := range []string{"/v1/summary", "/v1/teams", "/v1/donors", "/v1/posts"} {
		res, _ := get(t, srv, path)
		etag := res.Header().Get("ETag")
		if etag == "" {
			t.Errorf("%s: no ETag", path)
			continue
		}
		if !strings.Contains(etag, buildID()) {
			t.Errorf("%s: etag %q does not identify the build (%s), so a deploy that "+
				"changes a derivation cannot invalidate it", path, etag, buildID())
		}
	}

	// It must still carry the snapshot's identity, or a new cycle would be missed
	// instead — trading one stale-cache bug for its mirror image.
	res, _ := get(t, srv, "/v1/summary")
	if etag := res.Header().Get("ETag"); !strings.Contains(etag, "test-etag") {
		t.Errorf("etag %q no longer carries the snapshot identity", etag)
	}
}

func TestBuildIDIsStableWithinAProcess(t *testing.T) {
	// It rides on every response, so recomputing or drifting would be both a cost and
	// a correctness problem: a validator that changes without the build changing
	// invalidates every cache for nothing.
	first := buildID()
	if first == "" {
		t.Fatal("empty build id")
	}
	for i := 0; i < 3; i++ {
		if got := buildID(); got != first {
			t.Fatalf("build id changed within one process: %q then %q", first, got)
		}
	}
}

func TestOvertakeProjection(t *testing.T) {
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	day := func(d float64) *float64 { return &d }

	for _, tc := range []struct {
		name                  string
		selfScore, selfRate   int64
		rivalScore, rivalRate int64
		wantGap               int64
		wantDays              *float64
	}{
		// Chasing someone ahead: 1000 behind, gaining 100 a day.
		{"catching the one ahead", 9000, 500, 10000, 400, 1000, day(10)},
		// Being chased: symmetric, and the same number of days either way round.
		{"being caught from behind", 10000, 400, 9000, 500, 1000, day(10)},
		// Ahead and pulling away.
		{"leader pulling away", 10000, 500, 9000, 400, 1000, nil},
		// Behind and falling further behind.
		{"trailing and losing ground", 9000, 400, 10000, 500, 1000, nil},
		// Equal rates never converge, however small the gap.
		{"identical rates", 9000, 500, 10000, 500, 1000, nil},
		// An idle leader is caught by anyone still producing.
		{"idle leader", 9000, 100, 10000, 0, 1000, day(10)},
		// Nobody catches anybody at a standstill.
		{"both idle", 9000, 0, 10000, 0, 1000, nil},
		// Already level.
		{"dead level", 10000, 300, 10000, 100, 0, day(0)},
		// Beyond the horizon: a trillion-point gap closing at one point a day is
		// arithmetic, not a forecast, and must not be reported as a date.
		{"past the horizon", 0, 2, 1_000_000_000_000, 1, 1_000_000_000_000, nil},
	} {
		gap, days, at := projectOvertake(now, tc.selfScore, tc.selfRate, tc.rivalScore, tc.rivalRate)
		if gap != tc.wantGap {
			t.Errorf("%s: gap = %d, want %d", tc.name, gap, tc.wantGap)
		}
		switch {
		case tc.wantDays == nil && days != nil:
			t.Errorf("%s: projected %.2f days, want no projection", tc.name, *days)
		case tc.wantDays != nil && days == nil:
			t.Errorf("%s: no projection, want %.2f days", tc.name, *tc.wantDays)
		case tc.wantDays != nil && *days != *tc.wantDays:
			t.Errorf("%s: projected %.2f days, want %.2f", tc.name, *days, *tc.wantDays)
		}
		if (days == nil) != (at == nil) {
			t.Errorf("%s: overtake_days and overtake_at disagree about whether there is one", tc.name)
		}
		if days != nil && at != nil {
			want := now.Add(time.Duration(*days * float64(24*time.Hour)))
			if !at.Equal(want) {
				t.Errorf("%s: overtake_at = %v, want %v", tc.name, at, want)
			}
		}
	}
}

func TestRivalsEndpoint(t *testing.T) {
	srv := fixture(t)

	_, env := get(t, srv, "/v1/teams/32/rivals")
	got := decode[Rivals](t, env.Data)
	if got.Name != "overclockers" || got.Rank != 1 {
		t.Errorf("subject = %q rank %d, want overclockers rank 1", got.Name, got.Rank)
	}
	if got.HorizonDays == 0 {
		t.Error("horizon_days not reported; a client cannot tell a null projection from a missing feature")
	}
	// The neighbourhood includes the subject, so a client renders one list rather
	// than splicing two.
	var selves int
	for _, rv := range got.Rivals {
		if rv.Self {
			selves++
			if rv.PointsGap != 0 {
				t.Errorf("subject's own row has a gap of %d, want 0", rv.PointsGap)
			}
			// "You will overtake yourself, now" is not a thing to publish.
			if rv.OvertakeDays != nil || rv.OvertakeAt != nil {
				t.Errorf("subject's own row projects an overtake against itself: %v", rv.OvertakeDays)
			}
		}
	}
	if selves != 1 {
		t.Errorf("%d rows marked self, want exactly 1", selves)
	}
	// Ordered best-first, like every other ranked list here.
	for i := 1; i < len(got.Rivals); i++ {
		if got.Rivals[i].Rank <= got.Rivals[i-1].Rank {
			t.Errorf("rivals not in rank order: %d then %d", got.Rivals[i-1].Rank, got.Rivals[i].Rank)
		}
	}

	// Donors travel a separate path to the same shape.
	_, env = get(t, srv, "/v1/donors/DH/rivals")
	d := decode[Rivals](t, env.Data)
	if d.Name != "DH" {
		t.Errorf("donor subject = %q, want DH", d.Name)
	}
	if len(d.Rivals) == 0 {
		t.Error("no donor rivals returned")
	}
	for _, rv := range d.Rivals {
		if rv.TeamID != nil {
			t.Errorf("donor rival %q carries a team_id; a donor is not a member", rv.Name)
		}
	}

	if rec, _ := get(t, srv, "/v1/teams/999999/rivals"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown team: status %d, want 404", rec.Code)
	}
}

func TestRivalsOpenOnTheSubjectsOwnPage(t *testing.T) {
	// A rivals view opened at rank 1 answers a question nobody asked. Without an
	// explicit page it lands where the subject is; with one it means exactly what it
	// says, so a pager works from there like any other collection's.
	srv := periodFixture(t)

	// per_page=1 makes the page number and the rank the same thing, so "did it open
	// on the right page" is checkable rather than a matter of arithmetic luck.
	_, env := get(t, srv, "/v1/donors/surging/rivals?per_page=1")
	if env.Page == nil {
		t.Fatal("no page info on a paginated rivals response")
	}
	got := decode[Rivals](t, env.Data)
	if env.Page.Page != int(got.Rank) {
		t.Errorf("opened on page %d for a subject ranked #%d, want its own page",
			env.Page.Page, got.Rank)
	}
	if len(got.Rivals) != 1 || !got.Rivals[0].Self {
		t.Errorf("subject's own page does not contain the subject: %+v", got.Rivals)
	}

	// An explicit page overrides the anchor rather than being ignored.
	_, env = get(t, srv, "/v1/donors/surging/rivals?per_page=1&page=1")
	if env.Page.Page != 1 {
		t.Errorf("explicit page=1 landed on page %d", env.Page.Page)
	}
	first := decode[Rivals](t, env.Data)
	if len(first.Rivals) != 1 {
		t.Fatalf("page=1 returned %d rows, want 1", len(first.Rivals))
	}
	// The subject's identity travels even on a page it does not appear on, so a
	// client always knows whose projections these are.
	if first.Name != "surging" {
		t.Errorf("subject name = %q on another page, want surging", first.Name)
	}
	// Projections are still measured against the subject, not against the page.
	if first.Rivals[0].Self && int(got.Rank) != 1 {
		t.Error("a row on another page is marked self")
	}

	// Pagination rejects nonsense the same way every other collection does.
	for _, bad := range []string{"page=0", "per_page=0", "page=abc", "per_page=99999"} {
		if rec, _ := get(t, srv, "/v1/teams/1/rivals?"+bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", bad, rec.Code)
		}
	}
}

func TestEveryNumericColumnIsSortable(t *testing.T) {
	// Every numeric column a reader can see has to be orderable by it, or the table
	// has headings that look clickable and are not. per_day is the one that matters
	// most and the one that was missing: it is the column people actually rank by.
	srv := periodFixture(t)

	names := func(path string) []string {
		t.Helper()
		rec, env := get(t, srv, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		var out []string
		for _, row := range env.Data.([]any) {
			out = append(out, row.(map[string]any)["name"].(string))
		}
		return out
	}

	for _, ep := range []string{"/v1/teams", "/v1/donors"} {
		base := names(ep)
		for _, key := range []string{
			"lifetime", "per_day", "today", "this_week", "this_month",
			"last_24h", "wus", "members", "teams",
		} {
			got := names(ep + "?sort=" + key)
			if len(got) != len(base) {
				t.Errorf("%s sort=%s: %d rows, want %d — an ordering must not drop anyone",
					ep, key, len(got), len(base))
			}
		}
	}

	// per_day orders by the seven-day average, which is a different question from
	// lifetime: surging out-produces steady while trailing it on cumulative points.
	if got := names("/v1/donors?sort=per_day"); got[0] != "surging" {
		t.Errorf("sort=per_day leader = %s, want surging", got[0])
	}
	if got := names("/v1/teams?sort=per_day"); got[0] != "surging" {
		t.Errorf("teams sort=per_day leader = %s, want surging", got[0])
	}
	// ...and is not merely lifetime under another name.
	if names("/v1/donors?sort=per_day")[0] == names("/v1/donors?sort=lifetime")[0] {
		t.Error("sort=per_day returned the lifetime order; the key is not wired to the average")
	}

	// The first published names still mean what they meant. They shipped before
	// per_day existed, at which point "daily" became ambiguous with it.
	for alias, canonical := range map[string]string{
		"daily": "today", "weekly": "this_week", "monthly": "this_month",
	} {
		a, c := names("/v1/donors?sort="+alias), names("/v1/donors?sort="+canonical)
		if len(a) != len(c) || (len(a) > 0 && a[0] != c[0]) {
			t.Errorf("alias %s no longer matches %s", alias, canonical)
		}
	}

	if rec, _ := get(t, srv, "/v1/teams?sort=nonsense"); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown sort: status %d, want 400", rec.Code)
	}
	// The error has to name the alternatives; a bare "invalid" makes the caller guess.
	_, _ = get(t, srv, "/v1/teams?sort=nonsense")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/teams?sort=nonsense", nil))
	if !strings.Contains(rec.Body.String(), "per_day") {
		t.Errorf("400 body does not list the valid keys: %s", rec.Body.String())
	}
}

func TestEveryHistoryEndpointServesEveryGranularity(t *testing.T) {
	// Three separate functions in the store map a granularity onto a table, and
	// weekly was added to two of them. The donor path is the third, and it answered
	// `store: unknown granularity "weekly"` on a live page.
	//
	// The mapping being duplicated is the defect; this is the check that makes the
	// duplication survivable, by refusing to let any one copy fall behind. Endpoints
	// times granularities is a small enough matrix to just take all of it.
	srv := fixture(t)

	paths := []string{
		"/v1/summary/history",
		"/v1/teams/32/history",
		"/v1/donors/DH/history",            // aggregates across a donor's members
		"/v1/donors/DH/history?team_id=32", // scoped to one, a different code path
	}
	grans := []string{"hourly", "cycle", "daily", "weekly", "monthly"}

	for _, path := range paths {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		for _, g := range grans {
			url := path + sep + "granularity=" + g
			rec, env := get(t, srv, url)
			if rec.Code != http.StatusOK {
				t.Errorf("%s: status %d, want 200\n%s", url, rec.Code, rec.Body.String())
				continue
			}
			h := decode[History](t, env.Data)
			want := g
			if g == "cycle" {
				want = "hourly"
			}
			if h.Granularity != want {
				t.Errorf("%s: granularity echoed as %q, want %q", url, h.Granularity, want)
			}
		}
	}
}

func TestPaginationCannotOverflowIntoANegativeBound(t *testing.T) {
	// `page` was bounded below but not above, so (page-1)*perPage overflowed to a
	// negative offset that survived the `lo > n` clamp and reached the caller's slice
	// expression. One URL, no authentication, panic on every paginated route.
	srv := fixture(t)
	huge := []string{
		"9223372036854775807", // MaxInt64
		"9223372036854775806",
		"92233720368547758", // still overflows once multiplied
		"1000000000000",
	}
	paths := []string{
		"/v1/teams", "/v1/donors", "/v1/teams/32/members",
		"/v1/teams/32/rivals", "/v1/donors/DH/rivals", "/v1/donors/DH/teams",
	}
	for _, p := range paths {
		for _, page := range huge {
			rec, _ := get(t, srv, p+"?page="+page)
			// Past the end is an empty page, which is what it has always meant.
			// Anything except a clean status means the bound escaped again.
			if rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest {
				t.Errorf("%s?page=%s: status %d, want 200 or 400", p, page, rec.Code)
			}
		}
		// And an ordinary page still works.
		if rec, _ := get(t, srv, p+"?page=1"); rec.Code != http.StatusOK {
			t.Errorf("%s?page=1: status %d, want 200", p, rec.Code)
		}
	}
}

// growState adds one team and one donor to a live State without rebuilding the table,
// which is exactly what happens between state.Apply and Publish on every cycle.
func growState(t *testing.T, state *model.State) {
	t.Helper()
	state.Apply(at(3),
		[]parse.TeamRow{tr(32, "overclockers", 2500), tr(51, "Alliance", 900), tr(77, "newcomers", 400)},
		[]parse.UserRow{u("DH", 1200, 32), u("toTOW", 1000, 32), u("DH", 500, 51),
			u("solo", 400, 51), u("brandnew", 300, 77)})
}

func TestSnapshotSurvivesStateGrowingPastItsTable(t *testing.T) {
	// The Snapshot points at the LIVE State but a table frozen at the last publish,
	// so between state.Apply and Publish — seconds, every cycle — State holds entities
	// the table has never seen. Their slots run past the rank arrays, and indexing
	// blind panicked the handler for anyone who named one.
	st, err := store.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	state := model.NewState()
	memberWin, teamWin := metrics.New(0), metrics.New(0)
	ctx := context.Background()
	for _, c := range []struct {
		when  time.Time
		teams []parse.TeamRow
		users []parse.UserRow
	}{
		{at(1), []parse.TeamRow{tr(32, "overclockers", 1000), tr(51, "Alliance", 500)},
			[]parse.UserRow{u("DH", 600, 32), u("toTOW", 400, 32), u("DH", 300, 51), u("solo", 200, 51)}},
		{at(2), []parse.TeamRow{tr(32, "overclockers", 1900), tr(51, "Alliance", 700)},
			[]parse.UserRow{u("DH", 1000, 32), u("toTOW", 900, 32), u("DH", 400, 51), u("solo", 300, 51)}},
	} {
		cy := state.Apply(c.when, c.teams, c.users)
		if err := st.WriteCycle(ctx, state, cy, store.CycleMeta{
			TeamSnapshotAt: c.when, UserSnapshotAt: c.when}); err != nil {
			t.Fatal(err)
		}
		memberWin.Grow(len(state.Members))
		memberWin.Push(c.when, cy.MemberDeltas)
		teamWin.Grow(len(state.Teams))
		teamWin.Push(c.when, cy.TeamDeltas)
	}

	tbl := rank.Build(state, at(2), rank.DefaultConfig)
	tbl.BuildOrders(state, memberWin, teamWin, nil, nil)
	srv := NewServer()
	srv.Publish(Build(state, memberWin, teamWin, tbl, st, at(2), at(3), "grow-etag"))

	// The next cycle lands in State. The published table still predates it.
	growState(t, state)

	// Every route that resolves an entity from live State and then reads the table.
	for _, path := range []string{
		"/v1/teams/77",
		"/v1/teams/77/rivals",
		"/v1/teams/77/members",
		"/v1/donors/brandnew",
		"/v1/donors/brandnew/rivals",
		"/v1/donors/brandnew/teams",
		"/v1/donors/brandnew/history",
		"/v1/search?q=newcomers",
		"/v1/search?q=brandnew",
		"/v1/teams", "/v1/donors", "/v1/summary",
	} {
		rec, _ := get(t, srv, path)
		// 404 is fine — this snapshot legitimately predates the entity. A panic is
		// not, and net/http turns one into a torn-down connection mid-response.
		if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 200 or 404", path, rec.Code)
		}
	}

	// Entities the table does know are unaffected.
	if rec, _ := get(t, srv, "/v1/teams/32"); rec.Code != http.StatusOK {
		t.Errorf("established team: status %d, want 200", rec.Code)
	}
}

// blockingWriter stalls inside Write, the way a client that stops reading stalls the
// kernel send buffer.
type blockingWriter struct {
	hdr     http.Header
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingWriter) Header() http.Header { return w.hdr }
func (w *blockingWriter) WriteHeader(int)     {}
func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

func TestGuardIsReleasedBeforeTheResponseIsWritten(t *testing.T) {
	// The read lock used to be held by a deferred unlock for the whole handler, so it
	// covered w.Write. A client that requested a large page and stopped reading blocked
	// there for up to WriteTimeout while holding it; ingest's writer queued behind it,
	// and Go's RWMutex blocks new readers once a writer waits — so every subsequent
	// request blocked too, including /v1/status. One connection renewed every 59
	// seconds was an unauthenticated outage from a single host.
	srv := fixture(t)
	guard := &sync.RWMutex{}
	srv.Current().Guard = guard

	bw := &blockingWriter{hdr: http.Header{}, started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeHTTP(bw, httptest.NewRequest(http.MethodGet, "/v1/teams?per_page=1000", nil))
	}()

	select {
	case <-bw.started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never reached Write")
	}

	// We are now parked inside Write. The writer must be able to take the lock — that
	// is ingest, and on the old code it could not until the client drained or the
	// write timed out.
	if !guard.TryLock() {
		close(bw.release)
		<-done
		t.Fatal("the response write still holds the read lock; ingest would block behind a stalled client")
	}
	guard.Unlock()

	close(bw.release)
	<-done
}

func TestTeamSearchIndexPreservesSemantics(t *testing.T) {
	// Search's team half stopped scanning every team and now uses a sorted name
	// index, the way the donor half always did. The results have to be identical,
	// including the two cases the old full scan handled incidentally: several teams
	// sharing one exact name, and a name that is a strict prefix of longer ones.
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	state := model.NewState()
	memberWin, teamWin := metrics.New(0), metrics.New(0)
	// "Folders" is an exact name shared by two teams, and a prefix of three more.
	// Scores set the rank order the results must come back in.
	teams := []parse.TeamRow{
		tr(1, "Folders", 900), tr(2, "Folders United", 800), tr(3, "Folders", 700),
		tr(4, "folders anonymous", 600), tr(5, "FOLDERSXTREME", 500), tr(6, "Unrelated", 400),
	}
	users := []parse.UserRow{u("someone", 100, 1)}
	for _, when := range []time.Time{at(1), at(2)} {
		cy := state.Apply(when, teams, users)
		if err := st.WriteCycle(context.Background(), state, cy, store.CycleMeta{
			TeamSnapshotAt: when, UserSnapshotAt: when}); err != nil {
			t.Fatal(err)
		}
		memberWin.Grow(len(state.Members))
		memberWin.Push(when, cy.MemberDeltas)
		teamWin.Grow(len(state.Teams))
		teamWin.Push(when, cy.TeamDeltas)
	}
	tbl := rank.Build(state, at(2), rank.DefaultConfig)
	tbl.BuildOrders(state, memberWin, teamWin, nil, nil)
	srv := NewServer()
	srv.Publish(Build(state, memberWin, teamWin, tbl, st, at(2), at(3), "search-etag"))

	ids := func(path string) ([]int32, bool) {
		t.Helper()
		_, env := get(t, srv, path)
		r := decode[SearchResults](t, env.Data)
		var out []int32
		for _, tm := range r.Teams {
			out = append(out, tm.TeamID)
		}
		return out, r.ExactTeam
	}

	// Case-insensitive prefix, best-ranked first.
	got, exact := ids("/v1/search?q=Folders&type=team&limit=50")
	if !exact {
		t.Error("exact_team not set for a name that exists verbatim")
	}
	// Both teams named exactly "Folders" lead, in rank order, then the longer names.
	want := []int32{1, 3, 2, 4, 5}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("q=Folders order = %v, want %v", got, want)
			break
		}
	}

	// A lowercase query must find the mixed- and upper-case names too.
	if got, _ := ids("/v1/search?q=folders&type=team&limit=50"); len(got) != 5 {
		t.Errorf("q=folders matched %d teams, want 5 — the fold is not case-insensitive", len(got))
	}
	// A prefix that matches nothing is empty and, critically, not an error.
	if got, exact := ids("/v1/search?q=zzzzzzzz&type=team&limit=50"); len(got) != 0 || exact {
		t.Errorf("q=zzzzzzzz returned %v exact=%v, want none", got, exact)
	}
	// Numeric exact-by-id still leads and is still marked.
	if got, exact := ids("/v1/search?q=6&type=team&limit=50"); len(got) == 0 || got[0] != 6 || !exact {
		t.Errorf("q=6 returned %v exact=%v, want team 6 first and marked", got, exact)
	}
	// A longer prefix narrows rather than widening.
	if got, _ := ids("/v1/search?q=Folders%20U&type=team&limit=50"); len(got) != 1 || got[0] != 2 {
		t.Errorf("q='Folders U' returned %v, want just team 2", got)
	}
}

func TestWarmedProjectHistoryMatchesTheQueryItReplaces(t *testing.T) {
	// The precomputed history is served instead of running the aggregate, so the two
	// have to be the same answer to the same question. The dangerous failure is not a
	// crash: it is the warm path covering a slightly different window from the one
	// the request parser derives, which returns a plausible list of points that is
	// quietly wrong at the edges.
	//
	// Both are exercised through the handler, since that is where the substitution
	// happens — comparing the store calls directly would test neither.
	srv := fixture(t)

	for _, g := range []string{"hourly", "daily", "weekly", "monthly"} {
		// Cold: the fixture's snapshot has no warm cache, so this is the query path.
		_, cold := get(t, srv, "/v1/summary/history?granularity="+g)
		want := decode[History](t, cold.Data)

		snap := srv.Current()
		if err := snap.WarmProjectHistory(context.Background()); err != nil {
			t.Fatalf("%s: warming: %v", g, err)
		}
		if _, ok := snap.ProjectHist[store.Granularity(g).Normalize()]; !ok {
			t.Fatalf("%s: warming left no entry, so the handler would silently keep "+
				"using the query path and this test would prove nothing", g)
		}

		// Warm: same request, now answered from the precomputed map.
		_, hot := get(t, srv, "/v1/summary/history?granularity="+g)
		got := decode[History](t, hot.Data)

		if len(got.Points) != len(want.Points) {
			t.Fatalf("%s: warm returned %d points, query returned %d",
				g, len(got.Points), len(want.Points))
		}
		for i := range want.Points {
			if got.Points[i] != want.Points[i] {
				t.Errorf("%s: point %d: warm %+v, query %+v",
					g, i, got.Points[i], want.Points[i])
			}
		}
	}
}

func TestCallerSuppliedHistoryRangeIgnoresTheWarmCache(t *testing.T) {
	// The cache holds exactly one window per granularity, so a request naming its own
	// from/to must not be answered from it. Serving the default seven days to someone
	// who asked for one hour would be wrong in the direction that looks right.
	srv := fixture(t)
	snap := srv.Current()
	if err := snap.WarmProjectHistory(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The default window covers the fixture's production.
	_, full := get(t, srv, "/v1/summary/history?granularity=hourly")
	if wide := decode[History](t, full.Data); len(wide.Points) == 0 {
		t.Fatal("default window is empty, so this test cannot tell the paths apart")
	}

	// A window ending before the fixture begins contains nothing. Anything returned
	// here came from the cache, which holds the default range.
	_, empty := get(t, srv,
		"/v1/summary/history?granularity=hourly&from="+at(1).Add(-48*time.Hour).Format(time.RFC3339)+
			"&to="+at(1).Add(-24*time.Hour).Format(time.RFC3339))
	if got := decode[History](t, empty.Data); len(got.Points) != 0 {
		t.Errorf("a range predating every cycle returned %d points: the caller's "+
			"window was ignored and the cached default served instead", len(got.Points))
	}
}

// TestActiveOnlyRosterPagesOverExactlyTheActiveMembers pins the invariant the fast
// path depends on: the precomputed active count it paginates against has to equal the
// number of members the walk will actually yield.
//
// If those two ever disagree the failure is silent and ugly — a total_pages promising
// a page that comes back empty, or a last page cut short — so this walks every page
// and checks the concatenation against the filter done the slow, obvious way.
func TestActiveOnlyRosterPagesOverExactlyTheActiveMembers(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	state := model.NewState()
	memberWin, teamWin := metrics.New(0), metrics.New(0)

	// 25 members on one team. Only those whose score moves between the two cycles are
	// active, and the dormant ones are interleaved with the producers rather than
	// grouped, so a walk that stops early or an off-by-one lands on a real member.
	const n = 25
	mk := func(cycle int) []parse.UserRow {
		rows := make([]parse.UserRow, 0, n)
		for i := 0; i < n; i++ {
			score := int64(1000 - i*10) // descending, so rank order is index order
			if cycle == 2 && i%3 == 0 {
				score += 500 // every third member produces
			}
			rows = append(rows, u(fmt.Sprintf("m%02d", i), score, 42))
		}
		return rows
	}
	for i, when := range []time.Time{at(1), at(2)} {
		cy := state.Apply(when, []parse.TeamRow{tr(42, "mixed", 100000)}, mk(i+1))
		if err := st.WriteCycle(context.Background(), state, cy, store.CycleMeta{
			TeamSnapshotAt: when, UserSnapshotAt: when}); err != nil {
			t.Fatal(err)
		}
		memberWin.Grow(len(state.Members))
		memberWin.Push(when, cy.MemberDeltas)
		teamWin.Grow(len(state.Teams))
		teamWin.Push(when, cy.TeamDeltas)
	}
	tbl := rank.Build(state, at(2), rank.DefaultConfig)
	tbl.BuildOrders(state, memberWin, teamWin, nil, nil)
	snap := Build(state, memberWin, teamWin, tbl, st, at(2), at(3), "active-etag")
	srv := NewServer()
	srv.Publish(snap)

	// The filter done the obvious way, over the whole roster.
	var want []string
	for _, slot := range tbl.TeamMembers(42) {
		if memberWin.Last7d(slot) > 0 {
			want = append(want, state.Names.Name(state.Members[slot].NameID))
		}
	}
	if len(want) < 5 || len(want) >= n {
		t.Fatalf("%d of %d members active; the fixture needs a real mix", len(want), n)
	}

	// Page through the endpoint and concatenate.
	var got []string
	perPage := 3
	for page := 1; ; page++ {
		rec, env := get(t, srv, fmt.Sprintf(
			"/v1/teams/42/members?active_only=true&per_page=%d&page=%d", perPage, page))
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status %d", page, rec.Code)
		}
		if env.Page == nil {
			t.Fatal("no page info")
		}
		if env.Page.TotalItems != len(want) {
			t.Fatalf("page %d reports %d total items, filter finds %d",
				page, env.Page.TotalItems, len(want))
		}
		rows := decode[[]Member](t, env.Data)
		if len(rows) == 0 {
			t.Fatalf("page %d of %d is empty but was promised", page, env.Page.TotalPages)
		}
		for _, m := range rows {
			got = append(got, m.Name)
			if m.PointsLast7d <= 0 {
				t.Errorf("%s has no recent production but appears in the active roster", m.Name)
			}
		}
		if page >= env.Page.TotalPages {
			break
		}
	}

	if len(got) != len(want) {
		t.Fatalf("paged over %d active members, filter finds %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: paged %q, filter %q", i, got[i], want[i])
		}
	}
}

// TestDonorTotalsAgreeWithSummingTheMembers holds the precomputed per-donor figures
// to the walk they replace.
//
// donorView has two paths now, and they must produce the same Donor. The fallback is
// still reachable — any Table built without BuildOrders takes it — so this builds the
// same state both ways and compares every field of every donor. A drift here would
// not look like a bug from outside: it would be a slightly wrong points-per-day on
// exactly the donors nobody can check by hand, the ones on thousands of teams.
func TestDonorTotalsAgreeWithSummingTheMembers(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	state := model.NewState()
	memberWin, teamWin := metrics.New(0), metrics.New(0)

	// A spread of shapes: a donor on many teams (the expensive case the precompute
	// exists for), donors on one, a donor whose teams produce unevenly, and one that
	// appears late so its observed span is shorter than the window.
	rows := func(cycle int) []parse.UserRow {
		var out []parse.UserRow
		for team := int32(1); team <= 12; team++ {
			out = append(out, u("spread", int64(100*int(team)+cycle*7*int(team)), team))
		}
		out = append(out, u("solo", int64(5000+cycle*250), 1))
		out = append(out, u("dormant", 900, 2))
		for team := int32(1); team <= 3; team++ {
			out = append(out, u("uneven", int64(200+cycle*cycle*int(team)*13), team))
		}
		if cycle >= 2 {
			out = append(out, u("latecomer", int64(40*cycle), 4))
		}
		return out
	}
	var teamRows []parse.TeamRow
	for id := int32(1); id <= 12; id++ {
		teamRows = append(teamRows, tr(id, fmt.Sprintf("t%d", id), int64(10000*int(id))))
	}
	for i, when := range []time.Time{at(1), at(2), at(3)} {
		cy := state.Apply(when, teamRows, rows(i+1))
		if err := st.WriteCycle(context.Background(), state, cy, store.CycleMeta{
			TeamSnapshotAt: when, UserSnapshotAt: when}); err != nil {
			t.Fatal(err)
		}
		memberWin.Grow(len(state.Members))
		memberWin.Push(when, cy.MemberDeltas)
		teamWin.Grow(len(state.Teams))
		teamWin.Push(when, cy.TeamDeltas)
	}

	// Fast path: BuildOrders fills the per-donor totals.
	fast := rank.Build(state, at(3), rank.DefaultConfig)
	fast.BuildOrders(state, memberWin, teamWin, nil, nil)
	fastSnap := Build(state, memberWin, teamWin, fast, st, at(3), at(4), "fast")

	// Fallback: no BuildOrders, so donorView sums the members itself.
	slow := rank.Build(state, at(3), rank.DefaultConfig)
	slowSnap := Build(state, memberWin, teamWin, slow, st, at(3), at(4), "slow")

	if _, ok := fast.DonorTotals(0); !ok {
		t.Fatal("fast table has no precomputed totals; the paths are not being compared")
	}
	if _, ok := slow.DonorTotals(0); ok {
		t.Fatal("slow table has precomputed totals; the fallback is not being exercised")
	}
	if len(fast.Donors) < 5 {
		t.Fatalf("only %d donors in the fixture", len(fast.Donors))
	}

	var checked int
	for i := range fast.Donors {
		// ThisMonth comes from the rollup, which neither path computes here, so it is
		// zero on both and is not what this test is about.
		a := fastSnap.donorView(int32(i), true)
		b := slowSnap.donorView(int32(i), true)
		if a.Name != b.Name {
			t.Fatalf("donor %d: fast is %q, fallback is %q — the tables disagree on order",
				i, a.Name, b.Name)
		}
		if a.Production != b.Production {
			t.Errorf("donor %q:\n  precomputed %+v\n  summed      %+v", a.Name, a.Production, b.Production)
		}
		if !reflect.DeepEqual(a.Teams, b.Teams) || a.TeamsTruncated != b.TeamsTruncated {
			t.Errorf("donor %q: the embedded team breakdown differs between paths", a.Name)
		}
		if a.PointsLast7d > 0 {
			checked++
		}
	}
	if checked < 3 {
		t.Fatalf("only %d donors had any production; the fixture proves little", checked)
	}
}

// TestCappedDonorHistorySelectsTheSameMembers pins the shortcut to the thing it
// replaced.
//
// The capped set decides which teams a wide donor's history actually sums, so
// selecting a different hundred does not fail — it returns different numbers under
// the same label. This rebuilds the old route (order, render to views, look the slots
// back up by name and team) and requires the prefix to match it exactly.
func TestCappedDonorHistorySelectsTheSameMembers(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	state := model.NewState()
	memberWin, teamWin := metrics.New(0), metrics.New(0)

	// One donor across more teams than the cap, with scores descending by team so
	// rank order is unambiguous, plus unrelated donors so the CSR is not trivially
	// the whole member array.
	const teams = maxHistoryTeams + 37
	var teamRows []parse.TeamRow
	for id := int32(1); id <= teams; id++ {
		teamRows = append(teamRows, tr(id, fmt.Sprintf("t%d", id), int64(1000*int(id))))
	}
	rows := func(cycle int) []parse.UserRow {
		var out []parse.UserRow
		for id := int32(1); id <= teams; id++ {
			// The base term sets rank order (team 1 highest), and it dominates so that
			// order is stable. The per-cycle term is what *differs per member*, which
			// is the point: if every member produced the same amount, any hundred of
			// them would sum alike and this test could not tell a wrong prefix from a
			// right one.
			base := int64(teams-id+1) * 10000
			out = append(out, u("wide", base+int64(cycle)*int64(id), id))
			out = append(out, u(fmt.Sprintf("other%d", id), int64(50+cycle), id))
		}
		return out
	}
	for i, when := range []time.Time{at(1), at(2)} {
		cy := state.Apply(when, teamRows, rows(i+1))
		if err := st.WriteCycle(context.Background(), state, cy, store.CycleMeta{
			TeamSnapshotAt: when, UserSnapshotAt: when}); err != nil {
			t.Fatal(err)
		}
		memberWin.Grow(len(state.Members))
		memberWin.Push(when, cy.MemberDeltas)
		teamWin.Grow(len(state.Teams))
		teamWin.Push(when, cy.TeamDeltas)
	}
	tbl := rank.Build(state, at(2), rank.DefaultConfig)
	tbl.BuildOrders(state, memberWin, teamWin, nil, nil)
	snap := Build(state, memberWin, teamWin, tbl, st, at(2), at(3), "cap")

	idx, ok := snap.donorIndexByName("wide")
	if !ok {
		t.Fatal("donor not found")
	}
	members := snap.Ranks.DonorMembers(idx)
	if len(members) <= maxHistoryTeams {
		t.Fatalf("donor has %d members, need more than the cap of %d",
			len(members), maxHistoryTeams)
	}

	// The old route, rebuilt here.
	ordered, truncated := snap.breakdown(members, maxHistoryTeams)
	if !truncated {
		t.Fatal("breakdown did not truncate; the comparison is vacuous")
	}
	var want []int32
	for _, m := range ordered {
		if slot, ok := snap.memberSlot(m.Name, m.TeamID); ok {
			want = append(want, slot)
		}
	}

	got := members[:maxHistoryTeams]
	if len(got) != len(want) {
		t.Fatalf("prefix has %d members, the view round trip yields %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position %d: prefix slot %d, round trip slot %d", i, got[i], want[i])
		}
	}

	// The equivalence above is only worth anything if the handler is the thing using
	// it, so compare what the endpoint actually returns against the history of the
	// members the old route would have chosen. A prefix off by one selects a
	// different team and changes these totals without changing anything visible.
	srv := NewServer()
	srv.Publish(snap)
	_, env := get(t, srv, "/v1/donors/wide/history?granularity=daily")
	h := decode[History](t, env.Data)
	if !h.TeamsTruncated || h.TeamsIncluded != maxHistoryTeams {
		t.Errorf("response reports truncated=%v included=%d, want true and %d",
			h.TeamsTruncated, h.TeamsIncluded, maxHistoryTeams)
	}

	from, to := defaultHistoryRange(snap.At, store.Daily)
	expect, err := st.MembersHistory(context.Background(), want, from, to, store.Daily)
	if err != nil {
		t.Fatal(err)
	}
	if len(expect) == 0 {
		t.Fatal("no history for the expected members; the comparison proves nothing")
	}
	if len(h.Points) != len(expect) {
		t.Fatalf("endpoint returned %d buckets, the expected members have %d",
			len(h.Points), len(expect))
	}
	for i := range expect {
		if h.Points[i].Points != expect[i].Points || h.Points[i].WUs != expect[i].WUs {
			t.Errorf("bucket %s: endpoint %d pts/%d wus, expected members sum to %d/%d",
				expect[i].At.Format(time.RFC3339), h.Points[i].Points, h.Points[i].WUs,
				expect[i].Points, expect[i].WUs)
		}
	}
}

// TestTeamRosterSortsByEveryColumn pins the ordering a team's member table offers.
//
// A wrong order here does not fail: it returns the right members with the right
// numbers in an order that is simply not the one asked for, which nobody notices
// until they compare two pages. So every column is checked for a monotonically
// descending sequence, and the paging is checked to concatenate without gaps or
// repeats — the failure a head/tail split invites.
func TestTeamRosterSortsByEveryColumn(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	state := model.NewState()
	memberWin, teamWin := metrics.New(0), metrics.New(0)

	// 30 members on one team. Production is deliberately uncorrelated with lifetime
	// total, so a sort that silently fell back to the stored order would be caught:
	// the biggest lifetime scores produce nothing, and the recent producers are at
	// the bottom of the lifetime ranking.
	const n = 30
	rows := func(cycle int) []parse.UserRow {
		out := make([]parse.UserRow, 0, n)
		for i := 0; i < n; i++ {
			score := int64(100000 - i*1000) // lifetime: descending by index
			if cycle == 2 && i >= n/2 {
				score += int64((i - n/2 + 1) * 137) // only the lifetime tail produces
			}
			out = append(out, u(fmt.Sprintf("m%02d", i), score, 7))
		}
		return out
	}
	for i, when := range []time.Time{at(1), at(2)} {
		cy := state.Apply(when, []parse.TeamRow{tr(7, "mixed", 900000)}, rows(i+1))
		if err := st.WriteCycle(context.Background(), state, cy, store.CycleMeta{
			TeamSnapshotAt: when, UserSnapshotAt: when}); err != nil {
			t.Fatal(err)
		}
		memberWin.Grow(len(state.Members))
		memberWin.Push(when, cy.MemberDeltas)
		teamWin.Grow(len(state.Teams))
		teamWin.Push(when, cy.TeamDeltas)
	}
	tbl := rank.Build(state, at(2), rank.DefaultConfig)
	tbl.BuildOrders(state, memberWin, teamWin, nil, nil)
	srv := NewServer()
	srv.Publish(Build(state, memberWin, teamWin, tbl, st, at(2), at(3), "roster"))

	field := map[string]func(Member) int64{
		"lifetime":  func(m Member) int64 { return m.PointsTotal },
		"per_day":   func(m Member) int64 { return m.PointsPerDay7dAvg },
		"last_24h":  func(m Member) int64 { return m.PointsLast24h },
		"this_week": func(m Member) int64 { return m.PointsThisWeekUTC },
		"wus":       func(m Member) int64 { return m.WUsTotal },
	}
	for key, value := range field {
		_, env := get(t, srv, "/v1/teams/7/members?sort="+key+"&per_page=100")
		rows := decode[[]Member](t, env.Data)
		if len(rows) != n {
			t.Fatalf("%s: got %d rows, want %d", key, len(rows), n)
		}
		for i := 1; i < len(rows); i++ {
			if value(rows[i-1]) < value(rows[i]) {
				t.Errorf("%s: row %d (%s=%d) sorts above row %d (%s=%d)",
					key, i-1, rows[i-1].Name, value(rows[i-1]), i, rows[i].Name, value(rows[i]))
				break
			}
		}
		// A rate column must not simply reproduce the lifetime order, or the sort is
		// doing nothing and this test is proving nothing.
		if key == "per_day" || key == "last_24h" {
			same := true
			_, lenv := get(t, srv, "/v1/teams/7/members?per_page=100")
			base := decode[[]Member](t, lenv.Data)
			for i := range rows {
				if rows[i].Name != base[i].Name {
					same = false
					break
				}
			}
			if same {
				t.Errorf("%s produced the lifetime order unchanged", key)
			}
		}
	}

	// Paging across the head/tail boundary must not drop or repeat anyone. The head
	// is the members with a non-zero value; everyone else is served from the stored
	// order, and getting the hand-off wrong is invisible on a single page.
	seen := map[string]int{}
	for p := 1; p <= 3; p++ {
		_, env := get(t, srv, fmt.Sprintf("/v1/teams/7/members?sort=last_24h&per_page=10&page=%d", p))
		for _, m := range decode[[]Member](t, env.Data) {
			seen[m.Name]++
		}
	}
	if len(seen) != n {
		t.Errorf("paging saw %d distinct members across 3 pages of 10, want %d", len(seen), n)
	}
	for name, c := range seen {
		if c != 1 {
			t.Errorf("%s appeared %d times across the pages", name, c)
		}
	}
}

func TestPointsPerWURoundsAndSurvivesZero(t *testing.T) {
	// A ratio nobody can compute is reported as zero rather than as a panic or an
	// infinity, and the rounding is to nearest so a figure quoted back at us
	// reconstructs the totals it came from.
	for _, c := range []struct {
		points, wus, want int64
	}{
		{0, 0, 0},       // a member seen but never credited
		{500, 0, 0},     // points without units: no ratio exists
		{1000, 1, 1000}, // one unit
		{1000, 3, 333},  // 333.33 rounds down
		{2000, 3, 667},  // 666.67 rounds up
		{7, 2, 4},       // 3.5 rounds up, not toward zero
		// No negative case: these are cumulative lifetime totals, which upstream only
		// ever adds to. A regression makes a delta negative, never a total.
	} {
		if got := perWU(c.points, c.wus); got != c.want {
			t.Errorf("perWU(%d, %d) = %d, want %d", c.points, c.wus, got, c.want)
		}
	}
}
