package api

// Model Context Protocol server.
//
// The site already has an open JSON API, so the obvious thing would be to expose
// each REST route as a tool and be done. That would be worse than useless: a model
// asking "is my team catching up to theirs?" would have to discover an id, page a
// leaderboard, fetch two entities, pull two histories and do the arithmetic — five
// round trips to answer one question, with five chances to get the join wrong.
//
// So the tools here are shaped like questions rather than like endpoints. Each one
// answers something a person actually asks, in one call, and returns prose with the
// numbers already formatted and the caveats already attached. The REST API remains
// the right interface for a program that wants data; this is the right interface for
// something that wants an answer.
//
// Transport is Streamable HTTP (JSON-RPC 2.0 over POST) with no sessions, because
// every tool is a pure read of an immutable snapshot. Nothing to remember between
// calls means nothing to expire, resume, or get wrong.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"folding/internal/rank"
	"folding/internal/store"
)

// mcpProtocolVersion is what this server speaks. A client asking for a different
// version is answered with this one rather than refused: every method here is from
// the stable core of the protocol, and a client that cannot cope will say so.
const mcpProtocolVersion = "2025-06-18"

const (
	mcpParseError     = -32700
	mcpInvalidRequest = -32600
	mcpMethodNotFound = -32601
	mcpInvalidParams  = -32602
	mcpInternalError  = -32603
)

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// MCPHandler serves the Model Context Protocol endpoint.
func (s *Server) MCPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Permissive CORS, matching the rest of the site: this is public read-only
		// data and a browser-hosted client is as welcome as any other.
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Headers", "content-type, mcp-protocol-version")
		h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")

		switch r.Method {
		case http.MethodOptions:
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodPost:
		case http.MethodGet:
			// A conforming client opening a stream asks for one by Accept, and the
			// spec says to refuse when there is nothing to stream — every tool here is
			// a pure read, so there never is.
			//
			// Anything else issuing a GET is a person who pasted the URL into a
			// browser, and answering them with a JSON-RPC error is a wasted chance to
			// explain what they found.
			if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
				h.Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusMethodNotAllowed)
				_ = json.NewEncoder(w).Encode(mcpResponse{
					JSONRPC: "2.0",
					Error: &mcpError{mcpInvalidRequest,
						"this endpoint is POST-only: every tool is a pure read, so there is no event stream to subscribe to"},
				})
				return
			}
			http.Redirect(w, r, "/agents", http.StatusSeeOther)
			return
		default:
			h.Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(mcpResponse{
				JSONRPC: "2.0",
				Error:   &mcpError{mcpInvalidRequest, "this endpoint takes POST"},
			})
			return
		}

		var req mcpRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeMCP(w, mcpResponse{JSONRPC: "2.0",
				Error: &mcpError{mcpParseError, "invalid JSON: " + err.Error()}})
			return
		}

		// A notification carries no id and expects no reply.
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		resp := s.mcpDispatch(r, req)
		resp.JSONRPC, resp.ID = "2.0", req.ID
		writeMCP(w, resp)
	})
}

func writeMCP(w http.ResponseWriter, resp mcpResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Always 200: JSON-RPC carries its own errors, and an HTTP error code here would
	// have a client reporting "the server is down" for "no donor by that name".
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) mcpDispatch(r *http.Request, req mcpRequest) mcpResponse {
	switch req.Method {
	case "initialize":
		return mcpResponse{Result: map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{
				"name":    "folding-stats",
				"title":   "Folding@home statistics",
				"version": buildID(),
			},
			"instructions": "Live Folding@home donor and team statistics, refreshed hourly. " +
				"Start with search when you have a name and not an id. Every figure is derived " +
				"by comparing successive published snapshots, so rates and history only exist " +
				"from 3 August 2026 onward; lifetime totals go back to the beginning.",
		}}

	case "ping":
		return mcpResponse{Result: map[string]any{}}

	case "tools/list":
		return mcpResponse{Result: map[string]any{"tools": mcpTools()}}

	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return mcpResponse{Error: &mcpError{mcpInvalidParams, err.Error()}}
		}
		snap := s.Current()
		if snap == nil {
			return mcpResponse{Result: mcpToolResult{
				Content: []mcpContent{{Type: "text", Text: "The service is still starting up and has no snapshot yet."}},
				IsError: true,
			}}
		}
		text, err := s.mcpCall(r, snap, p.Name, p.Arguments)
		if err != nil {
			// A tool failure is a result, not a protocol error: the model should see
			// what went wrong and try something else, not have the call rejected.
			return mcpResponse{Result: mcpToolResult{
				Content: []mcpContent{{Type: "text", Text: err.Error()}}, IsError: true}}
		}
		return mcpResponse{Result: mcpToolResult{Content: []mcpContent{{Type: "text", Text: text}}}}
	}
	return mcpResponse{Error: &mcpError{mcpMethodNotFound, "unknown method " + req.Method}}
}

/* ------------------------------------------------------------- the tools --- */

func strSchema(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func intSchema(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func mcpTools() []mcpTool {
	sortKeys := []any{"lifetime", "per_day", "today", "this_week", "this_month", "last_24h", "wus"}
	grans := []any{"hourly", "daily", "weekly", "monthly"}

	return []mcpTool{{
		Name:  "search",
		Title: "Find a donor or team",
		Description: "Find donors and teams by name, or a team by its number. Start here when " +
			"you have a name rather than an id — every other tool needs the exact name or " +
			"team number, and names are not unique. Returns enough of each match to tell " +
			"them apart.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{"query"},
			"properties": map[string]any{
				"query": strSchema("A name, name prefix, or team number."),
				"limit": intSchema("Maximum matches of each kind. Default 8."),
			},
		},
	}, {
		Name:  "get_donor",
		Title: "One donor's standing",
		Description: "Everything about one donor: lifetime total, rank across all donors, " +
			"how fast they are producing, which teams they fold for, and how their rank " +
			"moved in the last day. Names are case-sensitive and must be exact — use search first.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{"name"},
			"properties": map[string]any{
				"name": strSchema("Exact donor name, as returned by search."),
			},
		},
	}, {
		Name:  "get_team",
		Title: "One team's standing",
		Description: "Everything about one team: lifetime total, rank, production rate, how " +
			"many members it has and how many are active, its rank movement, and its top " +
			"contributors.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{"team_id"},
			"properties": map[string]any{
				"team_id": intSchema("The team number."),
				"members": intSchema("How many top members to list. Default 5, max 25."),
				"sort":    map[string]any{"type": "string", "enum": sortKeys, "description": "How to order those members. Default lifetime — use this_month or per_day to ask who is carrying the team now rather than who built it."},
			},
		},
	}, {
		Name:  "leaderboard",
		Title: "Top teams or donors",
		Description: "The top of any ranking. Order by lifetime points or by a rate — note " +
			"these answer different questions: lifetime rewards having folded for twenty " +
			"years, per_day rewards folding hard right now.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{"kind"},
			"properties": map[string]any{
				"kind":  map[string]any{"type": "string", "enum": []any{"teams", "donors"}, "description": "Which ranking."},
				"sort":  map[string]any{"type": "string", "enum": sortKeys, "description": "Ordering. Default lifetime."},
				"limit": intSchema("How many rows. Default 10, max 50."),
			},
		},
	}, {
		Name:  "production_history",
		Title: "Production over time",
		Description: "How much was produced over time, for the whole project, one team, or " +
			"one donor — and, with scope=donor plus team_id, for one donor on one team, " +
			"which is a different question from their overall output. History only exists " +
			"from 3 August 2026, when collection started; lifetime totals are older but " +
			"cannot be broken down before that date.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{"scope"},
			"properties": map[string]any{
				"scope":       map[string]any{"type": "string", "enum": []any{"project", "team", "donor"}, "description": "Whose history."},
				"team_id":     intSchema("Required when scope is team. Optional when scope is donor, to narrow that donor's history to a single team."),
				"donor":       strSchema("Required when scope is donor."),
				"granularity": map[string]any{"type": "string", "enum": grans, "description": "Bucket size. Default daily."},
			},
		},
	}, {
		Name:  "compare",
		Title: "Head to head",
		Description: "Compare two teams or two donors directly: the gap between them, who is " +
			"gaining, and roughly when one would overtake the other if both held their " +
			"current rate. That projection is a guess and is reported as one — nobody holds " +
			"a constant rate.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{"kind", "a", "b"},
			"properties": map[string]any{
				"kind": map[string]any{"type": "string", "enum": []any{"teams", "donors"}, "description": "What is being compared."},
				"a":    strSchema("First entity: a donor name, or a team number."),
				"b":    strSchema("Second entity: a donor name, or a team number."),
			},
		},
	}, {
		Name:  "movers",
		Title: "Who climbed and who fell",
		Description: "The biggest rank movements over the last 24 hours, within the top of " +
			"a ranking. Bounded to the top on purpose: further down, thousands of entities " +
			"are separated by a handful of points, so the largest movements there are ties " +
			"reshuffling and say nothing about anybody.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{"kind"},
			"properties": map[string]any{
				"kind":      map[string]any{"type": "string", "enum": []any{"teams", "donors"}, "description": "Which ranking."},
				"direction": map[string]any{"type": "string", "enum": []any{"up", "down", "both"}, "description": "Climbers, fallers, or both. Default both."},
				"within":    intSchema("How far down the ranking to look. Default 1000, max 10000."),
				"limit":     intSchema("How many to list per direction. Default 10, max 25."),
			},
		},
	}, {
		Name:  "team_activity",
		Title: "What changed on a team",
		Description: "What is different about a team's roster today: members who were " +
			"producing all week and have stopped, members producing far above their own " +
			"average, and members who joined in the last day. get_team says how a team is " +
			"doing; this says what changed, which is what somebody running a team actually " +
			"needs and what no leaderboard shows.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{"team_id"},
			"properties": map[string]any{
				"team_id": intSchema("The team number."),
				"limit":   intSchema("How many to list in each section. Default 10, max 25."),
			},
		},
	}, {
		Name:  "rivals",
		Title: "Who is just ahead and just behind",
		Description: "The immediate neighbourhood in the rankings around one team or donor: " +
			"who is directly ahead, who is directly behind, the gap to each, and when the " +
			"order would swap at current rates. This is the tool for \"who am I about to " +
			"pass\" — compare answers the same question but requires already knowing who " +
			"the rival is, which is usually the part being asked.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{"kind", "who"},
			"properties": map[string]any{
				"kind": map[string]any{"type": "string", "enum": []any{"teams", "donors"}, "description": "Which ranking."},
				"who":  strSchema("A team number, or an exact donor name."),
				"span": intSchema("How many to show on each side. Default 5, max 15."),
			},
		},
	}, {
		Name:  "project_status",
		Title: "The project right now",
		Description: "Project-wide totals and how fresh the data is: how many donors and teams " +
			"exist, how many are actively producing, total points and work units, and when " +
			"the next update is due.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}}
}

func (s *Server) mcpCall(r *http.Request, snap *Snapshot, name string, raw json.RawMessage) (string, error) {
	var a struct {
		Query       string `json:"query"`
		Name        string `json:"name"`
		Donor       string `json:"donor"`
		Kind        string `json:"kind"`
		Sort        string `json:"sort"`
		Scope       string `json:"scope"`
		Granularity string `json:"granularity"`
		A           string `json:"a"`
		B           string `json:"b"`
		Who         string `json:"who"`
		Direction   string `json:"direction"`
		Within      int    `json:"within"`
		TeamID      *int32 `json:"team_id"`
		Limit       int    `json:"limit"`
		Members     int    `json:"members"`
		Span        int    `json:"span"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("could not read the arguments: %v", err)
		}
	}

	if snap.Guard != nil {
		snap.Guard.RLock()
		defer snap.Guard.RUnlock()
	}

	switch name {
	case "search":
		return snap.mcpSearch(a.Query, a.Limit)
	case "get_donor":
		return snap.mcpDonor(r.Context(), a.Name)
	case "get_team":
		return snap.mcpTeam(r.Context(), a.TeamID, a.Members, a.Sort)
	case "leaderboard":
		return snap.mcpLeaderboard(a.Kind, a.Sort, a.Limit)
	case "production_history":
		return snap.mcpHistory(r, a.Scope, a.TeamID, a.Donor, a.Granularity)
	case "compare":
		return snap.mcpCompare(a.Kind, a.A, a.B)
	case "rivals":
		return snap.mcpRivals(a.Kind, a.Who, a.Span)
	case "team_activity":
		return snap.mcpTeamActivity(a.TeamID, a.Limit)
	case "movers":
		return snap.mcpMovers(a.Kind, a.Direction, a.Within, a.Limit)
	case "project_status":
		return snap.mcpStatus(), nil
	}
	return "", fmt.Errorf("no tool called %q — call tools/list to see what exists", name)
}

/* --------------------------------------------------------- tool bodies --- */

func (s *Snapshot) mcpSearch(q string, limit int) (string, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return "", fmt.Errorf("search needs a query")
	}
	limit = clampInt(limit, 8, 1, 25)

	var b strings.Builder
	fmt.Fprintf(&b, "Search results for %q\n", q)

	teams := s.Ranks.TeamPrefix(s.State, q, limit)
	// A bare number is far more likely to be a team number than a name prefix, and
	// nobody typing "32" wants the teams whose names begin "32".
	if id, err := strconv.ParseInt(q, 10, 32); err == nil {
		if slot, ok := s.State.TeamSlot(int32(id)); ok {
			teams = append([]int32{slot}, teams...)
		}
	}
	if len(teams) > 0 {
		b.WriteString("\nTeams:\n")
		seen := map[int32]bool{}
		for _, slot := range teams {
			if seen[slot] {
				continue
			}
			seen[slot] = true
			t := s.teamView(slot)
			fmt.Fprintf(&b, "  #%d  %s  — team %d, %s points lifetime, %s/day\n",
				t.Rank, t.Name, t.TeamID, fmtShort(t.PointsTotal), fmtShort(t.PointsPerDay7dAvg))
		}
	}

	if idxs := s.Ranks.DonorPrefix(s.State, q, limit); len(idxs) > 0 {
		b.WriteString("\nDonors:\n")
		for _, i := range idxs {
			d := s.donorView(i, false)
			note := ""
			if d.LikelyNotAPerson {
				note = "  [shared name — many unrelated people]"
			}
			fmt.Fprintf(&b, "  #%d  %s  — %s points lifetime, %s/day, %d team(s)%s\n",
				d.Rank, d.Name, fmtShort(d.PointsTotal), fmtShort(d.PointsPerDay7dAvg), d.TeamCount, note)
		}
	}

	if !strings.Contains(b.String(), "\nTeams:") && !strings.Contains(b.String(), "\nDonors:") {
		return fmt.Sprintf("Nothing matched %q. Matching is by name prefix and is "+
			"case-insensitive, but the name must start with the query — try a shorter one.", q), nil
	}
	b.WriteString("\nNames are matched by prefix. Donor names are not unique.\n")
	return b.String() + s.mcpFooter(), nil
}

func (s *Snapshot) mcpDonor(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("get_donor needs a name")
	}
	idx, ok := s.donorIndexByName(name)
	if !ok {
		return "", fmt.Errorf("no donor named %q. Names are case-sensitive and must match "+
			"exactly — use search to find the right spelling.", name)
	}
	d := s.donorDetailView(ctx, idx)

	var b strings.Builder
	fmt.Fprintf(&b, "%s — donor rank #%s of %s\n", d.Name, fmtInt(int64(d.Rank)), fmtInt(int64(s.Totals.Donors)))
	if d.LikelyNotAPerson {
		fmt.Fprintf(&b, "\nNOTE: this name appears on %d teams and is almost certainly shared by many\n"+
			"unrelated people rather than being one donor. The points are real; the person is not.\n", d.TeamCount)
	}
	fmt.Fprintf(&b, "\n  Lifetime      %s points, %s work units\n", fmtInt(d.PointsTotal), fmtInt(d.WUsTotal))
	b.WriteString(mcpPerWU(d.PointsPerWU))
	fmt.Fprintf(&b, "  Per day       %s   (seven-day average)\n", fmtInt(d.PointsPerDay7dAvg))
	fmt.Fprintf(&b, "  Last 24h      %s\n", fmtInt(d.PointsLast24h))
	fmt.Fprintf(&b, "  Today (UTC)   %s\n", fmtInt(d.PointsTodayUTC))
	fmt.Fprintf(&b, "  This week     %s   (weeks start Sunday, UTC)\n", fmtInt(d.PointsThisWeekUTC))
	fmt.Fprintf(&b, "  This month    %s\n", fmtInt(d.PointsThisMonthUTC))
	b.WriteString(mcpMovement("  Rank moved    ", d.RankChange24h))
	b.WriteString(mcpStanding(d.Standing, "donors"))
	b.WriteString(mcpStreak(d.Streak))

	fmt.Fprintf(&b, "\nFolds for %d team(s)", d.TeamCount)
	if len(d.Teams) > 0 {
		b.WriteString(":\n")
		for _, m := range d.Teams {
			fmt.Fprintf(&b, "  team %-8d %-28s %s points, %s/day\n",
				m.TeamID, truncate(m.TeamName, 28), fmtShort(m.PointsTotal), fmtShort(m.PointsPerDay7dAvg))
		}
		if d.TeamsTruncated {
			b.WriteString("  (largest teams only)\n")
		}
	} else {
		b.WriteString("\n")
	}
	return b.String() + s.mcpFooter(), nil
}

func (s *Snapshot) mcpTeam(ctx context.Context, id *int32, members int, sortKey string) (string, error) {
	if id == nil {
		return "", fmt.Errorf("get_team needs a team_id")
	}
	slot, ok := s.State.TeamSlot(*id)
	if !ok {
		return "", fmt.Errorf("no team numbered %d", *id)
	}
	if sortKey == "" {
		sortKey = "lifetime"
	}
	key, ok := rank.NormalizeSort(rank.SortKey(sortKey))
	if !ok || key == rank.Members || key == rank.Teams {
		return "", fmt.Errorf("unknown member ordering %q — try lifetime, per_day, today, "+
			"this_week, this_month, last_24h or wus", sortKey)
	}
	t := s.teamDetailView(ctx, slot)
	members = clampInt(members, 5, 0, 25)

	var b strings.Builder
	fmt.Fprintf(&b, "%s — team %d, rank #%s of %s\n", t.Name, t.TeamID,
		fmtInt(int64(t.Rank)), fmtInt(int64(s.Totals.Teams)))
	fmt.Fprintf(&b, "\n  Lifetime      %s points, %s work units\n", fmtInt(t.PointsTotal), fmtInt(t.WUsTotal))
	b.WriteString(mcpPerWU(t.PointsPerWU))
	fmt.Fprintf(&b, "  Per day       %s   (seven-day average)\n", fmtInt(t.PointsPerDay7dAvg))
	fmt.Fprintf(&b, "  Last 24h      %s\n", fmtInt(t.PointsLast24h))
	fmt.Fprintf(&b, "  Today (UTC)   %s\n", fmtInt(t.PointsTodayUTC))
	fmt.Fprintf(&b, "  This month    %s\n", fmtInt(t.PointsThisMonthUTC))
	fmt.Fprintf(&b, "  Members       %s total, %s produced in the last 7 days\n",
		fmtInt(int64(t.MembersTotal)), fmtInt(int64(t.MembersActive)))
	b.WriteString(mcpMovement("  Rank moved    ", t.RankChange24h))
	b.WriteString(mcpStanding(t.Standing, "teams"))
	b.WriteString(mcpStreak(t.Streak))

	roster := s.Ranks.TeamMembers(t.TeamID)
	b.WriteString(s.mcpConcentration(roster, t.PointsTotal))

	if members > 0 {
		if n := min(members, len(roster)); n > 0 {
			ordered := roster[:n]
			if key != rank.Lifetime {
				ordered = s.orderRoster(roster, key, false, 0, n)
			}
			fmt.Fprintf(&b, "\nTop %d of %d members by %s:\n", len(ordered), len(roster), key)
			for _, slot := range ordered {
				m := s.memberView(slot, false)
				fmt.Fprintf(&b, "  %-26s %14s points, %12s/day, %s this month\n",
					truncate(m.Name, 26), fmtShort(m.PointsTotal),
					fmtShort(m.PointsPerDay7dAvg), fmtShort(m.PointsThisMonthUTC))
			}
		}
	}
	return b.String() + s.mcpFooter(), nil
}

// mcpConcentration says whether a team is one machine or a crowd.
//
// Two teams with the same total and the same rate can be completely different
// organisms — one person with a rack, or four hundred people with a spare desktop each
// — and every other figure reported here renders them identically. The distinction
// decides what advice is worth giving about the team, so it is worth a line.
//
// Bounded on purpose: the roster is already in lifetime order, so the top hundred are a
// slice, and the largest team on the site has 882,940 members. Walking further to find
// a median would put the cost of this line above everything else in the response.
func (s *Snapshot) mcpConcentration(roster []int32, total int64) string {
	if len(roster) < 10 || total <= 0 {
		return ""
	}
	share := func(n int) int {
		var sum int64
		for _, slot := range roster[:min(n, len(roster))] {
			sum += s.State.Members[slot].Score
		}
		return int(math.Round(float64(sum) / float64(total) * 100))
	}
	top10 := share(10)
	if len(roster) < 100 {
		return fmt.Sprintf("\n  Concentration the top 10 of %d members hold %d%% of the lifetime points\n",
			len(roster), top10)
	}
	return fmt.Sprintf("\n  Concentration the top 10 hold %d%% of the lifetime points, the top 100 hold %d%%\n"+
		"                (of %s members; lifetime, so it describes who built the total\n"+
		"                rather than who is producing now)\n",
		top10, share(100), fmtInt(int64(len(roster))))
}

func (s *Snapshot) mcpLeaderboard(kind, sortKey string, limit int) (string, error) {
	limit = clampInt(limit, 10, 1, 50)
	if sortKey == "" {
		sortKey = "lifetime"
	}
	key, ok := rank.NormalizeSort(rank.SortKey(sortKey))
	if !ok {
		return "", fmt.Errorf("unknown sort %q — try lifetime, per_day, today, this_week, this_month, last_24h or wus", sortKey)
	}

	var b strings.Builder
	blurb := map[rank.SortKey]string{
		rank.Lifetime:  "cumulative points since the beginning",
		rank.PerDay:    "seven-day average, points per day",
		rank.Today:     "points since 00:00 UTC today",
		rank.ThisWeek:  "points since Sunday 00:00 UTC",
		rank.ThisMonth: "points since the 1st, UTC",
		rank.Last24h:   "rolling last 24 hours",
		rank.WUs:       "work units completed",
	}[key]

	switch kind {
	case "teams":
		order := s.Ranks.TeamOrderFor(key)
		fmt.Fprintf(&b, "Top %d teams by %s (%s)\n\n", min(limit, len(order)), key, blurb)
		for i, slot := range order[:min(limit, len(order))] {
			t := s.teamView(slot)
			fmt.Fprintf(&b, "%3d. %-32s %14s   team %d\n",
				i+1, truncate(t.Name, 32), fmtInt(mcpSortValue(t.Production, key, int64(t.MembersTotal))), t.TeamID)
		}
	case "donors":
		order := s.Ranks.DonorOrderFor(key)
		fmt.Fprintf(&b, "Top %d donors by %s (%s)\n\n", min(limit, len(order)), key, blurb)
		for i, idx := range order[:min(limit, len(order))] {
			d := s.donorView(idx, false)
			shared := ""
			if d.LikelyNotAPerson {
				shared = "  [shared name]"
			}
			fmt.Fprintf(&b, "%3d. %-32s %14s%s\n",
				i+1, truncate(d.Name, 32), fmtInt(mcpSortValue(d.Production, key, int64(d.TeamCount))), shared)
		}
	default:
		return "", fmt.Errorf("kind must be \"teams\" or \"donors\"")
	}
	if key != rank.Lifetime {
		b.WriteString("\nPosition here is by the selected column, which is not the same as overall rank.\n")
	}
	return b.String() + s.mcpFooter(), nil
}

func mcpSortValue(p Production, k rank.SortKey, count int64) int64 {
	switch k {
	case rank.PerDay:
		return p.PointsPerDay7dAvg
	case rank.Today:
		return p.PointsTodayUTC
	case rank.ThisWeek:
		return p.PointsThisWeekUTC
	case rank.ThisMonth:
		return p.PointsThisMonthUTC
	case rank.Last24h:
		return p.PointsLast24h
	case rank.WUs:
		return p.WUsTotal
	case rank.Members, rank.Teams:
		return count
	}
	return p.PointsTotal
}

func (s *Snapshot) mcpHistory(r *http.Request, scope string, teamID *int32, donor, gran string) (string, error) {
	if gran == "" {
		gran = "daily"
	}
	g, ok := map[string]store.Granularity{
		"hourly": store.Hourly, "daily": store.Daily,
		"weekly": store.Weekly, "monthly": store.Monthly,
	}[gran]
	if !ok {
		return "", fmt.Errorf("granularity must be hourly, daily, weekly or monthly")
	}
	from, to := defaultHistoryRange(s.At, g)
	ctx := r.Context()

	var pts []store.Point
	var who string
	var err error
	switch scope {
	case "project":
		who = "the whole project"
		if cached, ok := s.ProjectHist[g]; ok {
			pts = cached
		} else {
			pts, err = s.Store.ProjectHistory(ctx, from, to, g)
		}
	case "team":
		if teamID == nil {
			return "", fmt.Errorf("scope \"team\" needs a team_id")
		}
		slot, ok := s.State.TeamSlot(*teamID)
		if !ok {
			return "", fmt.Errorf("no team numbered %d", *teamID)
		}
		who = s.teamView(slot).Name
		pts, err = s.Store.TeamHistory(ctx, slot, from, to, g)
	case "donor":
		if donor == "" {
			return "", fmt.Errorf("scope \"donor\" needs a donor name")
		}
		idx, ok := s.donorIndexByName(donor)
		if !ok {
			return "", fmt.Errorf("no donor named %q — use search first", donor)
		}
		who = donor
		members := s.Ranks.DonorMembers(idx)
		// Scoped to one team when asked. A donor on many teams has one total and
		// several stories, and "how much have I folded for this team" is a different
		// question from "how much have I folded" — without this the two are
		// indistinguishable through the tool, which is how the gap was found.
		if teamID != nil {
			var only []int32
			for _, slot := range members {
				if s.State.Members[slot].TeamID == *teamID {
					only = append(only, slot)
				}
			}
			if len(only) == 0 {
				return "", fmt.Errorf("%q has no record on team %d", donor, *teamID)
			}
			members = only
			if slot, ok := s.State.TeamSlot(*teamID); ok {
				who = fmt.Sprintf("%s on %s", donor, s.teamView(slot).Name)
			}
		}
		if len(members) > maxHistoryTeams {
			members = members[:maxHistoryTeams]
		}
		pts, err = s.Store.MembersHistory(ctx, members, from, to, g)
	default:
		return "", fmt.Errorf("scope must be \"project\", \"team\" or \"donor\"")
	}
	if err != nil {
		return "", fmt.Errorf("could not read history: %v", err)
	}
	if len(pts) == 0 {
		return fmt.Sprintf("No recorded production for %s at %s granularity. History begins "+
			"3 August 2026 — before that only lifetime totals exist.", who, gran), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Production for %s, by %s bucket\n\n", who, gran)
	var total int64
	for _, p := range pts {
		total += p.Points
	}
	layout := map[store.Granularity]string{
		store.Hourly: "2006-01-02 15:04Z", store.Daily: "2006-01-02",
		store.Weekly: "week of 2006-01-02", store.Monthly: "2006-01",
	}[g]
	for _, p := range pts {
		fmt.Fprintf(&b, "  %-20s %16s points  %10s WUs\n",
			p.At.Format(layout), fmtInt(p.Points), fmtInt(p.WUs))
	}
	fmt.Fprintf(&b, "\n  %d buckets, %s points total.\n", len(pts), fmtInt(total))
	if g == store.Hourly {
		b.WriteString("  Hourly buckets are one upstream publish each, about 3,610s apart — not clock hours.\n")
	}
	return b.String() + s.mcpFooter(), nil
}

func (s *Snapshot) mcpCompare(kind, aRef, bRef string) (string, error) {
	type side struct {
		name  string
		score int64
		rate  int64
		rank  int32
	}
	var x, y side

	load := func(ref string) (side, error) {
		switch kind {
		case "teams":
			id, err := strconv.ParseInt(strings.TrimSpace(ref), 10, 32)
			if err != nil {
				return side{}, fmt.Errorf("%q is not a team number — compare with kind \"teams\" takes numbers", ref)
			}
			slot, ok := s.State.TeamSlot(int32(id))
			if !ok {
				return side{}, fmt.Errorf("no team numbered %d", id)
			}
			t := s.teamView(slot)
			return side{t.Name, t.PointsTotal, t.PointsPerDay7dAvg, t.Rank}, nil
		case "donors":
			idx, ok := s.donorIndexByName(ref)
			if !ok {
				return side{}, fmt.Errorf("no donor named %q — use search first", ref)
			}
			d := s.donorView(idx, false)
			return side{d.Name, d.PointsTotal, d.PointsPerDay7dAvg, d.Rank}, nil
		}
		return side{}, fmt.Errorf("kind must be \"teams\" or \"donors\"")
	}
	var err error
	if x, err = load(aRef); err != nil {
		return "", err
	}
	if y, err = load(bRef); err != nil {
		return "", err
	}

	// Order them so the narrative is always "behind is chasing ahead".
	ahead, behind := x, y
	if behind.score > ahead.score {
		ahead, behind = behind, ahead
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s  vs  %s\n\n", x.name, y.name)
	fmt.Fprintf(&b, "  %-30s rank #%-8s %16s points   %12s/day\n", truncate(ahead.name, 30),
		fmtInt(int64(ahead.rank)), fmtInt(ahead.score), fmtInt(ahead.rate))
	fmt.Fprintf(&b, "  %-30s rank #%-8s %16s points   %12s/day\n", truncate(behind.name, 30),
		fmtInt(int64(behind.rank)), fmtInt(behind.score), fmtInt(behind.rate))

	gap := ahead.score - behind.score
	fmt.Fprintf(&b, "\n  %s is ahead by %s points.\n", truncate(ahead.name, 30), fmtInt(gap))

	_, days, at := projectOvertake(s.At, behind.score, behind.rate, ahead.score, ahead.rate)
	switch {
	case gap == 0:
		b.WriteString("  They are level.\n")
	case days == nil:
		if behind.rate <= ahead.rate {
			fmt.Fprintf(&b, "  %s is not gaining — at current rates the gap widens rather than closes.\n",
				truncate(behind.name, 30))
		} else {
			b.WriteString("  The gap is closing, but too slowly to project a date worth quoting.\n")
		}
	default:
		fmt.Fprintf(&b, "  At current rates %s would overtake in about %s (around %s).\n",
			truncate(behind.name, 30), humanDays(*days), at.Format("2 January 2006"))
		b.WriteString("\n  That is a projection, not a forecast: it assumes both sides hold today's\n" +
			"  seven-day average forever, which nobody with a job or a power bill does.\n")
	}
	return b.String() + s.mcpFooter(), nil
}

// mcpMovers reports the biggest rank movements near the top of a ranking.
//
// Bounded to the top deliberately, and the bound is the whole design. Rank movement is
// largest exactly where it means least: past a few thousand places down, entities are
// separated by a handful of points, so a single work unit vaults somebody twenty
// thousand places and an unbounded "biggest movers" list is a readout of who happened
// to break a tie. Near the top, the same movement takes real production.
func (s *Snapshot) mcpMovers(kind, direction string, within, limit int) (string, error) {
	if direction == "" {
		direction = "both"
	}
	if direction != "up" && direction != "down" && direction != "both" {
		return "", fmt.Errorf("direction must be \"up\", \"down\" or \"both\"")
	}
	within = clampInt(within, 1000, 10, 10000)
	limit = clampInt(limit, 10, 1, 25)

	type mover struct {
		name   string
		rank   int32
		change int32
		rate   int64
	}
	var all []mover
	var field int

	switch kind {
	case "teams":
		order := s.Ranks.TeamOrder
		field = len(order)
		for _, slot := range order[:min(within, len(order))] {
			c, ok := s.Ranks.TeamChange24h(slot)
			if !ok || c == 0 {
				continue
			}
			v := s.teamView(slot)
			all = append(all, mover{fmt.Sprintf("%s (team %d)", v.Name, v.TeamID), v.Rank, c, v.PointsPerDay7dAvg})
		}
	case "donors":
		field = len(s.Ranks.Donors)
		for i := 0; i < min(within, field); i++ {
			c, ok := s.Ranks.DonorChange24h(int32(i))
			if !ok || c == 0 {
				continue
			}
			v := s.donorView(int32(i), false)
			all = append(all, mover{v.Name, v.Rank, c, v.PointsPerDay7dAvg})
		}
	default:
		return "", fmt.Errorf("kind must be \"teams\" or \"donors\"")
	}

	if len(all) == 0 {
		return fmt.Sprintf("Nothing in the top %s %s moved in the last 24 hours — or less "+
			"than a day of history has been observed, in which case there is no earlier "+
			"ranking to compare against.", fmtInt(int64(within)), kind), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Biggest 24-hour rank movements in the top %s of %s %s\n",
		fmtInt(int64(within)), fmtInt(int64(field)), kind)

	show := func(title string, up bool) {
		sort.Slice(all, func(i, j int) bool {
			if up {
				return all[i].change > all[j].change
			}
			return all[i].change < all[j].change
		})
		var shown int
		var body strings.Builder
		for _, m := range all {
			if shown == limit || (up && m.change <= 0) || (!up && m.change >= 0) {
				break
			}
			fmt.Fprintf(&body, "  %+5d  #%-8s %-36s %12s/day\n",
				m.change, fmtInt(int64(m.rank)), truncate(m.name, 36), fmtInt(m.rate))
			shown++
		}
		if shown == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s:\n%s", title, body.String())
	}

	if direction != "down" {
		show("Climbed", true)
	}
	if direction != "up" {
		show("Fell", false)
	}

	b.WriteString("\nMovement is places gained or lost since 24 hours ago, and is measured against\n" +
		"the corpus as it stood then — an entity that did not exist a day ago has no\n" +
		"earlier rank and is not listed.\n")
	return b.String() + s.mcpFooter(), nil
}

// surgeRatio is how far above their own seven-day average a member has to be producing
// before it counts as a surge rather than as an ordinary good day.
const surgeRatio = 1.5

// mcpTeamActivity reports what changed on a roster.
//
// Every other view of a team is a level — points, rank, rate. None of them answers the
// question somebody running a team actually has, which is "what is different today":
// who has stopped, who has started, who turned up. A member who produced steadily all
// week and nothing in the last day is the single most useful thing this data can
// surface, and until now there was no way to ask for it — not here, and not on any
// comparable site.
//
// One pass over the roster, which is what every roster operation costs. The lists are
// capped, so a team where five hundred people went quiet reports the largest losses and
// a count, rather than five hundred lines.
func (s *Snapshot) mcpTeamActivity(id *int32, limit int) (string, error) {
	if id == nil {
		return "", fmt.Errorf("team_activity needs a team_id")
	}
	if _, ok := s.State.TeamSlot(*id); !ok {
		return "", fmt.Errorf("no team numbered %d", *id)
	}
	limit = clampInt(limit, 10, 1, 25)
	slot, _ := s.State.TeamSlot(*id)
	team := s.teamView(slot)
	roster := s.Ranks.TeamMembers(*id)

	type entry struct {
		slot int32
		by   int64 // what the section is ranked on
	}
	var stopped, surging, joined []entry
	newKnown := false

	for _, m := range roster {
		last24, last7 := s.Members.Last24h(m), s.Members.Last7d(m)
		if arrived, ok := s.Ranks.MemberArrivedSince24h(m); ok {
			newKnown = true
			if arrived {
				joined = append(joined, entry{m, s.State.Members[m].Score})
				// A member who did not exist yesterday has no yesterday to have stopped
				// or surged against, so they belong in one section only.
				continue
			}
		}
		switch {
		case last7 > 0 && last24 == 0:
			// Ranked by what the team is missing while they are quiet, so the biggest
			// loss is first rather than the biggest name.
			stopped = append(stopped, entry{m, last7})
		case last24 > 0:
			// Against their own average, not against the team's: the question is
			// whether this member changed, and a small folder doubling is the same
			// event as a large one doubling.
			if rate := s.Members.PointsPerDay(m); rate > 0 && float64(last24) >= surgeRatio*float64(rate) {
				surging = append(surging, entry{m, last24 - rate})
			}
		}
	}

	byMagnitude := func(e []entry) {
		sort.Slice(e, func(i, j int) bool { return e[i].by > e[j].by })
	}
	byMagnitude(stopped)
	byMagnitude(surging)
	byMagnitude(joined)

	var b strings.Builder
	fmt.Fprintf(&b, "What changed on %s (team %d)\n", team.Name, team.TeamID)
	fmt.Fprintf(&b, "%s members, %s produced in the last 7 days.\n",
		fmtInt(int64(team.MembersTotal)), fmtInt(int64(team.MembersActive)))

	section := func(title, note string, e []entry, show func(int32) string) {
		if len(e) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s (%s)", title, fmtInt(int64(len(e))))
		if len(e) > limit {
			fmt.Fprintf(&b, ", largest %d", limit)
		}
		fmt.Fprintf(&b, ":\n%s\n", note)
		for _, x := range e[:min(limit, len(e))] {
			b.WriteString(show(x.slot))
		}
	}

	section("Stopped", "  Producing all week, nothing in the last 24 hours.",
		stopped, func(m int32) string {
			v := s.memberView(m, false)
			return fmt.Sprintf("  %-26s %12s over 7 days, %s/day average\n",
				truncate(v.Name, 26), fmtShort(v.PointsLast7d), fmtShort(v.PointsPerDay7dAvg))
		})

	section("Producing above their own average", "  Last 24 hours against their seven-day rate.",
		surging, func(m int32) string {
			v := s.memberView(m, false)
			mult := float64(v.PointsLast24h) / float64(max64(v.PointsPerDay7dAvg, 1))
			return fmt.Sprintf("  %-26s %12s in 24h, %.1f× their %s/day average\n",
				truncate(v.Name, 26), fmtShort(v.PointsLast24h), mult, fmtShort(v.PointsPerDay7dAvg))
		})

	section("Joined", "  First seen on this team within the last 24 hours.",
		joined, func(m int32) string {
			v := s.memberView(m, false)
			return fmt.Sprintf("  %-26s arrived with %s points, %s in 24h\n",
				truncate(v.Name, 26), fmtShort(v.PointsTotal), fmtShort(v.PointsLast24h))
		})

	if len(stopped) == 0 && len(surging) == 0 && len(joined) == 0 {
		b.WriteString("\nNothing changed: no arrivals, nobody stopped, and nobody is producing\n" +
			"far above their own average.\n")
	}
	if !newKnown {
		b.WriteString("\nArrivals are not reported yet — less than 24 hours of history has been\n" +
			"observed, so there is no earlier roster to compare against.\n")
	}
	// A first sighting carries the entity's whole pre-existing lifetime total, which is
	// production we never saw. Left unsaid, a model reads "arrived with 4.2B points" as
	// four billion points earned yesterday.
	if len(joined) > 0 {
		b.WriteString("\nA joiner's lifetime total was earned before we first saw them; only the\n" +
			"24-hour figure is production we actually observed.\n")
	}
	return b.String() + s.mcpFooter(), nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// mcpRivals is the neighbourhood around one entity.
//
// compare already answers "when do I pass them", but only once you know who "them" is
// — and a model asked "who is my team about to overtake" would otherwise have to page a
// leaderboard, find the subject in it, and read off its neighbours by hand, which is
// three chances to get an off-by-one wrong in service of a question the ordering
// answers directly.
func (s *Snapshot) mcpRivals(kind, who string, span int) (string, error) {
	span = clampInt(span, 5, 1, 15)

	type row struct {
		rank        int32
		name        string
		score, rate int64
		self        bool
	}
	var rows []row
	var subject row

	switch kind {
	case "teams":
		id, err := strconv.ParseInt(strings.TrimSpace(who), 10, 32)
		if err != nil {
			return "", fmt.Errorf("%q is not a team number — rivals with kind \"teams\" takes numbers", who)
		}
		slot, ok := s.State.TeamSlot(int32(id))
		if !ok {
			return "", fmt.Errorf("no team numbered %d", id)
		}
		order := s.Ranks.TeamOrder
		self := s.teamView(slot)
		lo, hi := window(int(self.Rank)-1, span, len(order))
		for _, near := range order[lo:hi] {
			v := s.teamView(near)
			rows = append(rows, row{v.Rank, fmt.Sprintf("%s (team %d)", v.Name, v.TeamID),
				v.PointsTotal, v.PointsPerDay7dAvg, near == slot})
		}
	case "donors":
		idx, ok := s.donorIndexByName(who)
		if !ok {
			return "", fmt.Errorf("no donor named %q — use search first", who)
		}
		// Donors are stored in rank order, so the neighbourhood is a slice.
		lo, hi := window(int(idx), span, len(s.Ranks.Donors))
		for i := lo; i < hi; i++ {
			v := s.donorView(int32(i), false)
			rows = append(rows, row{v.Rank, v.Name, v.PointsTotal, v.PointsPerDay7dAvg, int32(i) == idx})
		}
	default:
		return "", fmt.Errorf("kind must be \"teams\" or \"donors\"")
	}
	for _, r := range rows {
		if r.self {
			subject = r
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Around %s — rank #%s of %s %s\n\n",
		subject.name, fmtInt(int64(subject.rank)),
		fmtInt(int64(map[string]int{"teams": s.Totals.Teams, "donors": s.Totals.Donors}[kind])), kind)

	now := time.Now().UTC()
	for _, r := range rows {
		mark := "  "
		if r.self {
			mark = "> "
		}
		fmt.Fprintf(&b, "%s#%-8s %-34s %16s   %12s/day",
			mark, fmtInt(int64(r.rank)), truncate(r.name, 34), fmtInt(r.score), fmtInt(r.rate))
		if r.self {
			b.WriteString("   ← this one\n")
			continue
		}
		// "Swap" rather than naming who overtakes whom: the rows are in rank order, so
		// the direction is already on the page, and a sentence about it would have to
		// get the subject's side right on every line to add nothing.
		gap, days, _ := projectOvertake(now, subject.score, subject.rate, r.score, r.rate)
		if days == nil {
			fmt.Fprintf(&b, "   %s apart, not converging\n", fmtShort(gap))
		} else {
			fmt.Fprintf(&b, "   %s apart, swap in %s\n", fmtShort(gap), humanDays(*days))
		}
	}

	b.WriteString("\nGaps are lifetime points. The times are projections that assume both sides hold\n" +
		"today's seven-day average forever, which nobody does; \"not converging\" means the\n" +
		"gap is widening or would take longer than a decade to close.\n")
	return b.String() + s.mcpFooter(), nil
}

// window centres a span of 2n+1 positions on i, clamped to the ends of the ordering so
// the leader still gets a neighbourhood rather than half of one.
func window(i, span, n int) (lo, hi int) {
	lo, hi = i-span, i+span+1
	if lo < 0 {
		hi, lo = min(hi-lo, n), 0
	}
	if hi > n {
		lo, hi = max(0, lo-(hi-n)), n
	}
	return lo, hi
}

func (s *Snapshot) mcpStatus() string {
	var b strings.Builder
	b.WriteString("Folding@home — project totals\n\n")
	fmt.Fprintf(&b, "  Points          %s\n", fmtInt(s.Totals.PointsTotal))
	fmt.Fprintf(&b, "  Work units      %s\n", fmtInt(s.Totals.WUsTotal))
	fmt.Fprintf(&b, "  Last 24 hours   %s points\n", fmtInt(s.Totals.PointsLast24h))
	fmt.Fprintf(&b, "  Today (UTC)     %s points\n", fmtInt(s.Totals.PointsToday))
	fmt.Fprintf(&b, "\n  Donors          %s total, %s produced in the last 7 days\n",
		fmtInt(int64(s.Totals.Donors)), fmtInt(int64(s.Totals.DonorsActive)))
	fmt.Fprintf(&b, "  Teams           %s total, %s producing\n",
		fmtInt(int64(s.Totals.Teams)), fmtInt(int64(s.Totals.TeamsActive)))
	fmt.Fprintf(&b, "  Members         %s   (a member is one donor on one team)\n", fmtInt(int64(s.Totals.Members)))
	if d, t, m, ok := s.Ranks.NewSince24h(s.Totals.Members, s.Totals.Teams); ok {
		fmt.Fprintf(&b, "\n  Arrived in 24h  %s donors, %s teams, %s memberships\n",
			fmtInt(int64(d)), fmtInt(int64(t)), fmtInt(int64(m)))
		b.WriteString("                  A donor already here joining another team makes a membership\n" +
			"                  but not a donor, which is why the two differ.\n")
	}

	if w := warmingUp(s); w != nil && w.HistorySpanSec > 0 {
		fmt.Fprintf(&b, "\n  Still warming up: only %s of history has been collected, so seven-day\n"+
			"  averages are computed over that shorter span rather than a full week.\n",
			(time.Duration(w.HistorySpanSec) * time.Second).Round(time.Hour))
	}
	return b.String() + s.mcpFooter()
}

/* --------------------------------------------------------------- format --- */

// mcpFooter states when the data is from. Every tool ends with it, because a model
// quoting a figure without its age is the failure this whole endpoint would
// otherwise invite.
func (s *Snapshot) mcpFooter() string {
	f := fmt.Sprintf("\nData as of %s (upstream publishes roughly hourly; next due %s).\n",
		s.At.Format("2006-01-02 15:04 UTC"), s.NextExpected.Format("15:04 UTC"))
	if !s.StaleAfter.IsZero() && time.Now().After(s.StaleAfter) {
		f += "The expected update has not arrived — this data is older than it should be.\n"
	}
	return f
}

// mcpStanding states position as a share of the field.
//
// A rank on its own has no scale: "#48,213" is meaningless until you know whether the
// field is fifty thousand or two million, and a model relaying it to someone will
// either guess or omit the context entirely. The share carries its own denominator, so
// it cannot be quoted without it.
func mcpStanding(st *Standings, noun string) string {
	if st == nil {
		return ""
	}
	var b strings.Builder
	if s := st.Lifetime; s != nil {
		fmt.Fprintf(&b, "  Standing      top %s of all %s %s by lifetime points\n",
			fmtPercent(s.TopPercent), fmtInt(int64(s.Of)), noun)
	}
	// Absent means no production this month, which is not last place — it is not being
	// in the field. Saying so beats leaving a model to infer it from a missing line.
	if s := st.ThisMonth; s != nil {
		fmt.Fprintf(&b, "                top %s of the %s %s that produced this month\n",
			fmtPercent(s.TopPercent), fmtInt(int64(s.Of)), noun)
	} else if st.Lifetime != nil {
		fmt.Fprintf(&b, "                nothing produced this month, so no standing among this month's %s\n", noun)
	}
	return b.String()
}

// fmtPercent keeps a share legible across the five orders of magnitude it spans, from
// a leader at 0.00005%% to the tail at 90%%. A fixed precision would print either
// "0.00%" for the best in the world or "12.3456%" for somebody mid-table.
func fmtPercent(p float64) string {
	switch {
	case p >= 10:
		return strconv.FormatFloat(p, 'f', 0, 64) + "%"
	case p >= 1:
		return strconv.FormatFloat(p, 'f', 1, 64) + "%"
	case p >= 0.01:
		return strconv.FormatFloat(p, 'f', 2, 64) + "%"
	}
	// Below a hundredth of a percent the digits stop carrying meaning: "top 0.000047%"
	// of two million donors is a way of writing "first", and the rank on the line above
	// already says it better.
	return "<0.01%"
}

// mcpStreak reports consecutive days of production.
//
// The caveat is not optional. This service started collecting on a particular day, so
// an entity that has folded daily for fifteen years reports whatever that is in days —
// and a model relaying "a 3-day streak" for a fifteen-year habit has said something
// false out of a number that is arithmetically correct. Where the run reaches the floor
// of the record, the line says so instead of quoting it flat.
func mcpStreak(s *Streak) string {
	if s == nil || s.ActiveDays == 0 {
		return ""
	}
	var b strings.Builder
	switch {
	case s.Current == 0:
		fmt.Fprintf(&b, "  Streak        none running; best was %s, %s active in total\n",
			plural(s.Longest, "day"), plural(s.ActiveDays, "day"))
	case s.AtCollectionFloor:
		fmt.Fprintf(&b, "  Streak        %s and counting — but that is every day on record, so it is a\n"+
			"                floor and not a measurement: collection began %s and whatever\n"+
			"                came before it is not visible here\n",
			plural(s.Current, "day"), s.Since.Format("2 January 2006"))
	default:
		fmt.Fprintf(&b, "  Streak        %s, since %s (best %s, %s active in total)\n",
			plural(s.Current, "day"), s.Since.Format("2 January 2006"),
			plural(s.Longest, "day"), plural(s.ActiveDays, "day"))
	}
	return b.String()
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%s %ss", fmtInt(int64(n)), noun)
}

// mcpPerWU reports points per work unit with the one caveat that stops a model
// turning a ratio into a hardware diagnosis.
//
// The figure is worth having: work units differ in value by orders of magnitude
// between project classes, so it is the only signal in the feed that separates a GPU
// from a CPU at all. But it is a lifetime average, and a model told "high means GPU"
// without being told "averaged over everything they have ever folded" will confidently
// describe last decade's hardware.
func mcpPerWU(ratio int64) string {
	if ratio == 0 {
		return ""
	}
	return fmt.Sprintf("  Per WU        %s points   (lifetime average; a rough proxy for hardware "+
		"class,\n                since GPU projects pay far more per unit than CPU ones)\n", fmtInt(ratio))
}

func mcpMovement(label string, change *int32) string {
	if change == nil {
		return label + "no comparable rank a day ago (newer than that, or not yet observed)\n"
	}
	switch c := int(*change); {
	case c > 0:
		return fmt.Sprintf("%sup %d place(s) in 24h\n", label, c)
	case c < 0:
		return fmt.Sprintf("%sdown %d place(s) in 24h\n", label, -c)
	default:
		return label + "unchanged in 24h\n"
	}
}

func humanDays(d float64) string {
	switch {
	case d < 1:
		return fmt.Sprintf("%d hours", max(1, int(d*24+0.5)))
	case d < 14:
		return fmt.Sprintf("%d days", int(d+0.5))
	case d < 90:
		return fmt.Sprintf("%d weeks", int(d/7+0.5))
	case d < 730:
		return fmt.Sprintf("%d months", int(d/30.4+0.5))
	}
	return fmt.Sprintf("%d years", int(d/365+0.5))
}

// fmtInt groups digits, because a model reading 8219397246031 aloud gets it wrong
// and a person checking the answer cannot see the magnitude at a glance.
func fmtInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

func fmtShort(n int64) string {
	f := float64(n)
	for _, u := range []struct {
		lim  float64
		suff string
	}{{1e12, "T"}, {1e9, "B"}, {1e6, "M"}, {1e3, "K"}} {
		if f >= u.lim {
			return strings.TrimSuffix(strconv.FormatFloat(f/u.lim, 'f', 2, 64), ".00") + u.suff
		}
	}
	return strconv.FormatInt(n, 10)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func clampInt(v, def, lo, hi int) int {
	if v == 0 {
		v = def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
