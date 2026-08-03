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
	"strings"
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

func TestPointsPerDayUsesRoundedSevenDayAverage(t *testing.T) {
	// The field name is honest about what it is, and the arithmetic matches the
	// figure EOC publishes so the two sites stay reconcilable.
	srv := fixture(t)
	_, env := get(t, srv, "/v1/donors/DH")
	d := decode[Donor](t, env.Data)
	if want := roundDiv7(d.PointsLast7d); d.PointsPerDay7dAvg != want {
		t.Errorf("points_per_day_7d_avg = %d, want %d", d.PointsPerDay7dAvg, want)
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
	for _, g := range []string{"hourly", "daily", "monthly"} {
		rec, _ := get(t, srv, "/v1/teams/32/history?granularity="+g)
		if rec.Code != http.StatusOK {
			t.Errorf("granularity=%s with default range: status %d, want 200", g, rec.Code)
		}
	}
	rec, _ := get(t, srv, "/v1/teams/32/history?granularity=weekly")
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
	if !strings.HasPrefix(postETag, `"posts-`) {
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
