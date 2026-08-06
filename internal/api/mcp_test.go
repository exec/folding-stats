package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"folding/internal/metrics"
	"folding/internal/model"
	"folding/internal/parse"
	"folding/internal/rank"
	"folding/internal/store"
)

func mcpDo(t *testing.T, srv *Server, body string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	srv.MCPHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d for %s", rec.Code, body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unparseable response: %v\n%s", err, rec.Body.String())
	}
	return out
}

// mcpText returns a successful tool call's text, failing if the call errored.
func mcpText(t *testing.T, srv *Server, tool, args string) string {
	t.Helper()
	out := mcpDo(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+
		tool+`","arguments":`+args+`}}`)
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no result: %v", tool, out)
	}
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("%s: empty content", tool)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	if res["isError"] == true {
		t.Fatalf("%s(%s) errored: %s", tool, args, text)
	}
	return text
}

func TestMCPHandshake(t *testing.T) {
	srv := fixture(t)
	out := mcpDo(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	res := out["result"].(map[string]any)
	if res["protocolVersion"] != mcpProtocolVersion {
		t.Errorf("protocolVersion = %v, want %s", res["protocolVersion"], mcpProtocolVersion)
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Error("no tools capability advertised, so a client will never call tools/list")
	}
	// The instructions are how a model learns the data's shape before its first
	// call. Losing them is silent: every tool still works, slightly wrongly used.
	if s, _ := res["instructions"].(string); !strings.Contains(s, "hourly") {
		t.Error("instructions do not mention the refresh cadence")
	}

	// A notification has no id and must get no body back.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	srv.MCPHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Errorf("notification got %d with %d bytes; want 202 and nothing",
			rec.Code, rec.Body.Len())
	}
}

// TestMCPToolsAreWellFormed guards the contract a model reads before it can call
// anything. A tool with no description or a malformed schema is not a broken call —
// it is a tool the model declines to use, or uses wrongly, with no error anywhere.
func TestMCPToolsAreWellFormed(t *testing.T) {
	srv := fixture(t)
	out := mcpDo(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools, _ := out["result"].(map[string]any)["tools"].([]any)
	if len(tools) < 5 {
		t.Fatalf("only %d tools", len(tools))
	}
	seen := map[string]bool{}
	for _, raw := range tools {
		tl := raw.(map[string]any)
		name, _ := tl["name"].(string)
		if name == "" || seen[name] {
			t.Errorf("missing or duplicate tool name %q", name)
		}
		seen[name] = true
		if d, _ := tl["description"].(string); len(d) < 40 {
			t.Errorf("%s: description is too thin for a model to choose on: %q", name, d)
		}
		sch, ok := tl["inputSchema"].(map[string]any)
		if !ok || sch["type"] != "object" {
			t.Errorf("%s: inputSchema is not an object schema", name)
			continue
		}
		props, _ := sch["properties"].(map[string]any)
		// Every declared required field must exist in properties, or a client
		// generating arguments from the schema produces something unusable.
		for _, r := range toStrings(sch["required"]) {
			if _, ok := props[r]; !ok {
				t.Errorf("%s: required field %q is not described in properties", name, r)
			}
		}
		for pn, p := range props {
			if d, _ := p.(map[string]any)["description"].(string); d == "" {
				t.Errorf("%s.%s has no description", name, pn)
			}
		}
	}
	for _, want := range []string{"search", "get_donor", "get_team", "leaderboard", "compare"} {
		if !seen[want] {
			t.Errorf("tool %q is missing", want)
		}
	}
}

func TestMCPToolsAnswerFromTheFixture(t *testing.T) {
	srv := fixture(t)

	if s := mcpText(t, srv, "search", `{"query":"overclockers"}`); !strings.Contains(s, "overclockers") {
		t.Errorf("search did not find the fixture team:\n%s", s)
	}
	if s := mcpText(t, srv, "get_donor", `{"name":"DH"}`); !strings.Contains(s, "DH") ||
		!strings.Contains(s, "Lifetime") {
		t.Errorf("get_donor output is not a profile:\n%s", s)
	}
	if s := mcpText(t, srv, "get_team", `{"team_id":32}`); !strings.Contains(s, "Members") {
		t.Errorf("get_team output is not a profile:\n%s", s)
	}
	if s := mcpText(t, srv, "leaderboard", `{"kind":"donors","limit":3}`); !strings.Contains(s, "Top") {
		t.Errorf("leaderboard produced nothing:\n%s", s)
	}
	if s := mcpText(t, srv, "compare", `{"kind":"teams","a":"32","b":"51"}`); !strings.Contains(s, "ahead by") {
		t.Errorf("compare did not state a gap:\n%s", s)
	}
	if s := mcpText(t, srv, "project_status", `{}`); !strings.Contains(s, "Donors") {
		t.Errorf("project_status produced nothing:\n%s", s)
	}

	// Every answer carries its own age. A model quoting a figure without knowing
	// how old it is is the specific failure this endpoint would otherwise invite,
	// and it cannot be caught downstream.
	for _, c := range []struct{ tool, args string }{
		{"search", `{"query":"overclockers"}`}, {"get_donor", `{"name":"DH"}`},
		{"get_team", `{"team_id":32}`}, {"leaderboard", `{"kind":"teams"}`},
		{"compare", `{"kind":"teams","a":"32","b":"51"}`}, {"project_status", `{}`},
	} {
		if !strings.Contains(mcpText(t, srv, c.tool, c.args), "Data as of") {
			t.Errorf("%s does not say how fresh its answer is", c.tool)
		}
	}
}

// TestMCPFailuresAreResultsNotProtocolErrors keeps a bad argument recoverable. A
// JSON-RPC error makes a client report that the server is broken; a tool result with
// isError lets the model read what went wrong and try again.
func TestMCPFailuresAreResultsNotProtocolErrors(t *testing.T) {
	srv := fixture(t)
	for _, c := range []struct{ tool, args, want string }{
		{"get_donor", `{"name":"nobody-by-that-name"}`, "search"},
		{"get_team", `{"team_id":999999}`, "no team"},
		{"leaderboard", `{"kind":"sideways"}`, "teams"},
		{"production_history", `{"scope":"team"}`, "team_id"},
		{"compare", `{"kind":"teams","a":"notanumber","b":"32"}`, "team number"},
		{"no_such_tool", `{}`, "tools/list"},
	} {
		out := mcpDo(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+
			c.tool+`","arguments":`+c.args+`}}`)
		if _, isErr := out["error"]; isErr {
			t.Errorf("%s(%s) returned a protocol error; a client will call the server broken",
				c.tool, c.args)
			continue
		}
		res := out["result"].(map[string]any)
		if res["isError"] != true {
			t.Errorf("%s(%s) reported success", c.tool, c.args)
			continue
		}
		text := res["content"].([]any)[0].(map[string]any)["text"].(string)
		if !strings.Contains(text, c.want) {
			t.Errorf("%s(%s): message does not suggest a way forward (want %q):\n  %s",
				c.tool, c.args, c.want, text)
		}
	}

	// An unknown method is a genuine protocol error and should say so.
	if _, ok := mcpDo(t, srv, `{"jsonrpc":"2.0","id":1,"method":"no/such"}`)["error"]; !ok {
		t.Error("unknown method did not produce a JSON-RPC error")
	}
}

func toStrings(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, a := range arr {
		if s, ok := a.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// TestMCPGetIsNegotiated covers the two very different callers of GET on this
// endpoint. A conforming MCP client asks for a stream by Accept and must be refused,
// because there is nothing to stream. A person who pasted the URL into a browser is
// not an error and should be sent somewhere that explains what they found.
func TestMCPGetIsNegotiated(t *testing.T) {
	srv := fixture(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	srv.MCPHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("stream request got %d, want 405 — the spec requires refusal when "+
			"there are no server-initiated messages", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	srv.MCPHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/agents" {
		t.Errorf("browser request got %d to %q, want 303 to /agents",
			rec.Code, rec.Header().Get("Location"))
	}
}

// TestMCPDonorHistoryScopesToATeam covers the question the first version of this
// tool could not express: how much has one donor folded *for one team*.
//
// A donor on many teams has one total and several stories, and without the scope
// those are indistinguishable through the tool — the aggregate silently answers a
// different question from the one asked, which is the worst kind of wrong.
func TestMCPDonorHistoryScopesToATeam(t *testing.T) {
	srv := fixture(t)

	// The fixture's DH folds for teams 32 and 51, producing on both.
	all := mcpText(t, srv, "production_history", `{"scope":"donor","donor":"DH","granularity":"hourly"}`)
	one := mcpText(t, srv, "production_history",
		`{"scope":"donor","donor":"DH","team_id":32,"granularity":"hourly"}`)

	if !strings.Contains(one, "on ") {
		t.Errorf("scoped history does not name the team it is scoped to:\n%s", one)
	}
	sumAll, sumOne := totalFromHistory(t, all), totalFromHistory(t, one)
	if sumOne == 0 {
		t.Fatalf("scoped history came back empty:\n%s", one)
	}
	if sumOne >= sumAll {
		t.Errorf("one team (%d) is not less than every team (%d); the filter did nothing",
			sumOne, sumAll)
	}

	// Asking for a team the donor has no record on is a real answer, not an empty
	// series that reads as "produced nothing".
	out := mcpDo(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":
		"production_history","arguments":{"scope":"donor","donor":"DH","team_id":999}}}`)
	res, _ := out["result"].(map[string]any)
	if res == nil || res["isError"] != true {
		t.Error("a donor with no record on the requested team did not report an error")
	}
}

// totalFromHistory reads the "N buckets, X points total." line the tool emits.
func totalFromHistory(t *testing.T, s string) int64 {
	t.Helper()
	for _, line := range strings.Split(s, "\n") {
		if !strings.Contains(line, "points total") {
			continue
		}
		f := strings.Fields(strings.ReplaceAll(line, ",", ""))
		for i, w := range f {
			if strings.HasPrefix(w, "points") && i > 0 {
				var n int64
				fmt.Sscanf(f[i-1], "%d", &n)
				return n
			}
		}
	}
	t.Fatalf("no total line in:\n%s", s)
	return 0
}

// activityFixture builds a roster where each member has stopped, surged, or just
// arrived, so the three classifications can be told apart by name.
func activityFixture(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "act.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	state := model.NewState()
	memberWin, teamWin := metrics.New(0), metrics.New(0)
	ctx := context.Background()

	// t0, +30h, +60h. Only the last cycle is inside 24 hours of the snapshot, which is
	// what makes "produced this week, nothing today" expressible at all: the +30h
	// production sits inside the 7-day window and outside the 24-hour one.
	base := at(1)
	cycles := []struct {
		when  time.Time
		teams []parse.TeamRow
		users []parse.UserRow
	}{{
		base,
		[]parse.TeamRow{tr(32, "overclockers", 1000)},
		[]parse.UserRow{u("quitter", 500, 32), u("steady", 300, 32), u("spiker", 200, 32)},
	}, {
		// Inside the 7-day window but outside 24 hours: this is the production that
		// makes quitter look busy all week and idle today.
		base.Add(30 * time.Hour),
		[]parse.TeamRow{tr(32, "overclockers", 9000)},
		[]parse.UserRow{u("quitter", 5000, 32), u("steady", 2000, 32), u("spiker", 2000, 32)},
	}, {
		base.Add(60 * time.Hour),
		[]parse.TeamRow{tr(32, "overclockers", 60000)},
		[]parse.UserRow{
			u("quitter", 5000, 32), // nothing since: stopped
			u("steady", 2100, 32),  // a trickle: neither stopped nor surging
			u("spiker", 50000, 32), // far above its own average: surging
			u("fresh", 900, 32),    // never seen before: joined
		},
	}}

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
	tbl.BuildOrders(state, memberWin, teamWin, nil, nil)

	srv := NewServer()
	srv.Publish(Build(state, memberWin, teamWin, tbl, st, last, last.Add(time.Hour), "act-etag"))
	return srv
}

func TestTeamActivitySeparatesStoppedFromSurgingFromNew(t *testing.T) {
	// The whole value of this tool is the classification, and every branch of it is a
	// judgement that could be one comparison away from reporting the opposite.
	got := mcpText(t, activityFixture(t), "team_activity", `{"team_id":32}`)

	sections := map[string]string{}
	for _, part := range strings.Split(got, "\n\n") {
		switch {
		case strings.HasPrefix(part, "Stopped"):
			sections["stopped"] = part
		case strings.HasPrefix(part, "Producing above"):
			sections["surging"] = part
		case strings.HasPrefix(part, "Joined"):
			sections["joined"] = part
		}
	}

	for _, c := range []struct{ section, want, notWant string }{
		// Produced all week, nothing in the last 24 hours.
		{"stopped", "quitter", "spiker"},
		// 48,000 points in 24h against a much smaller average.
		{"surging", "spiker", "quitter"},
		// First seen in the final cycle.
		{"joined", "fresh", "steady"},
	} {
		body, ok := sections[c.section]
		if !ok {
			t.Errorf("no %s section in:\n%s", c.section, got)
			continue
		}
		if !strings.Contains(body, c.want) {
			t.Errorf("%s section does not name %q:\n%s", c.section, c.want, body)
		}
		if strings.Contains(body, c.notWant) {
			t.Errorf("%s section wrongly names %q:\n%s", c.section, c.notWant, body)
		}
	}

	// A member who kept producing, at their usual rate, is not news and must not
	// appear anywhere: a tool that reports everybody reports nothing.
	if strings.Count(got, "steady") != 0 {
		t.Errorf("steady was reported as a change:\n%s", got)
	}

	// A joiner's lifetime total predates us, and left unqualified a model reads it as
	// production we observed.
	if !strings.Contains(got, "earned before we first saw them") {
		t.Errorf("joiners reported without the first-sighting caveat:\n%s", got)
	}
}

func TestMoversSeparatesClimbersFromFallers(t *testing.T) {
	// What is actually load-bearing here is the sign guard, not the sort: it is what
	// keeps an entity out of the section its movement contradicts. A climber listed
	// under "Fell" reads as plausible on real data and would survive a glance, so it is
	// asserted line by line rather than by spot-checking one name.
	//
	// The ordering within a section is not exercised — this fixture has a single mover,
	// so there is nothing to order.
	got := mcpText(t, rankChangeFixture(t), "movers", `{"kind":"donors","within":100}`)

	climbed, fell := section(got, "Climbed"), section(got, "Fell")
	if climbed == "" && fell == "" {
		t.Fatalf("no movement reported at all:\n%s", got)
	}
	// faller drops two places as climber and a newcomer land above it.
	if !strings.Contains(fell, "faller") {
		t.Errorf("faller is not among the fallers:\n%s", got)
	}
	if strings.Contains(climbed, "faller") {
		t.Errorf("faller is listed as a climber:\n%s", got)
	}
	// Every line in a section must agree with its heading's sign.
	for _, c := range []struct {
		body string
		want bool // true when the sign should be positive
	}{{climbed, true}, {fell, false}} {
		for _, line := range strings.Split(c.body, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "-") {
				continue
			}
			if got := strings.HasPrefix(line, "+"); got != c.want {
				t.Errorf("line %q is in the wrong section", line)
			}
		}
	}
}

// section returns the body of one titled block of a tool's output.
func section(text, title string) string {
	for _, part := range strings.Split(text, "\n\n") {
		if strings.HasPrefix(part, title+":") {
			return part
		}
	}
	return ""
}

func TestGoalAccountsForTheTargetMovingToo(t *testing.T) {
	// The moving-target correction is the whole reason this tool is worth more than a
	// division. Overtaking somebody who is also producing costs the gap plus whatever
	// they add in the meantime, and a naive gap ÷ days understates it — sometimes by
	// more than the gap itself.
	//
	// It is checked as a property rather than against a hand-computed constant: the
	// rate required to pass a moving target must exceed the target's own rate, because
	// anything at or below it never closes the distance no matter how long it runs.
	srv := fixture(t)
	got := mcpText(t, srv, "what_would_it_take",
		`{"kind":"donors","who":"solo","overtake":"toTOW","by":"2026-09-03"}`)

	if !strings.Contains(got, "still climbing") {
		t.Errorf("a producing target was not flagged as moving:\n%s", got)
	}
	if !strings.Contains(got, "already includes what the target adds") {
		t.Errorf("output does not say the correction was applied:\n%s", got)
	}

	// Pull the required rate back out of the prose and check it against the target's.
	rate := extractRate(t, got, "it would take ")
	target := mcpText(t, srv, "get_donor", `{"name":"toTOW"}`)
	if rate <= extractRate(t, target, "Per day       ") {
		t.Errorf("required rate %d does not exceed the target's own rate, so it would "+
			"never catch them:\n%s", rate, got)
	}
}

func TestGoalRefusesTargetsThatAreNotTargets(t *testing.T) {
	srv := fixture(t)
	for _, c := range []struct{ args, want string }{
		{`{"kind":"donors","who":"solo"}`, "give a target"},
		{`{"kind":"donors","who":"solo","target_points":9999999,"by":"yesterday"}`, "must be a date"},
		{`{"kind":"donors","who":"solo","target_points":9999999,"by":"2020-01-01"}`, "not in the future"},
		{`{"kind":"donors","who":"nobody","target_points":1}`, "no donor named"},
		{`{"kind":"donors","who":"solo","target_rank":99999}`, "outside the"},
	} {
		out := mcpDo(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"what_would_it_take","arguments":`+c.args+`}}`)
		res := out["result"].(map[string]any)
		text, _ := res["content"].([]any)[0].(map[string]any)["text"].(string)
		if res["isError"] != true {
			t.Errorf("%s was accepted, want an error: %s", c.args, text)
			continue
		}
		if !strings.Contains(text, c.want) {
			t.Errorf("%s: error %q does not mention %q", c.args, text, c.want)
		}
	}
}

// extractRate reads the first grouped number following a marker.
func extractRate(t *testing.T, text, marker string) int64 {
	t.Helper()
	i := strings.Index(text, marker)
	if i < 0 {
		t.Fatalf("no %q in:\n%s", marker, text)
	}
	var digits strings.Builder
	for _, r := range text[i+len(marker):] {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
			continue
		}
		if r == ',' {
			continue
		}
		break
	}
	v, err := strconv.ParseInt(digits.String(), 10, 64)
	if err != nil {
		t.Fatalf("could not read a number after %q: %v", marker, err)
	}
	return v
}
