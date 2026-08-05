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
	"encoding/json"
	"fmt"
	"net/http"
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
		TeamID      *int32 `json:"team_id"`
		Limit       int    `json:"limit"`
		Members     int    `json:"members"`
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
		return snap.mcpDonor(a.Name)
	case "get_team":
		return snap.mcpTeam(a.TeamID, a.Members)
	case "leaderboard":
		return snap.mcpLeaderboard(a.Kind, a.Sort, a.Limit)
	case "production_history":
		return snap.mcpHistory(r, a.Scope, a.TeamID, a.Donor, a.Granularity)
	case "compare":
		return snap.mcpCompare(a.Kind, a.A, a.B)
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

func (s *Snapshot) mcpDonor(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("get_donor needs a name")
	}
	idx, ok := s.donorIndexByName(name)
	if !ok {
		return "", fmt.Errorf("no donor named %q. Names are case-sensitive and must match "+
			"exactly — use search to find the right spelling.", name)
	}
	d := s.donorView(idx, true)

	var b strings.Builder
	fmt.Fprintf(&b, "%s — donor rank #%s of %s\n", d.Name, fmtInt(int64(d.Rank)), fmtInt(int64(s.Totals.Donors)))
	if d.LikelyNotAPerson {
		fmt.Fprintf(&b, "\nNOTE: this name appears on %d teams and is almost certainly shared by many\n"+
			"unrelated people rather than being one donor. The points are real; the person is not.\n", d.TeamCount)
	}
	fmt.Fprintf(&b, "\n  Lifetime      %s points, %s work units\n", fmtInt(d.PointsTotal), fmtInt(d.WUsTotal))
	fmt.Fprintf(&b, "  Per day       %s   (seven-day average)\n", fmtInt(d.PointsPerDay7dAvg))
	fmt.Fprintf(&b, "  Last 24h      %s\n", fmtInt(d.PointsLast24h))
	fmt.Fprintf(&b, "  Today (UTC)   %s\n", fmtInt(d.PointsTodayUTC))
	fmt.Fprintf(&b, "  This week     %s   (weeks start Sunday, UTC)\n", fmtInt(d.PointsThisWeekUTC))
	fmt.Fprintf(&b, "  This month    %s\n", fmtInt(d.PointsThisMonthUTC))
	b.WriteString(mcpMovement("  Rank moved    ", d.RankChange24h))

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

func (s *Snapshot) mcpTeam(id *int32, members int) (string, error) {
	if id == nil {
		return "", fmt.Errorf("get_team needs a team_id")
	}
	slot, ok := s.State.TeamSlot(*id)
	if !ok {
		return "", fmt.Errorf("no team numbered %d", *id)
	}
	t := s.teamView(slot)
	members = clampInt(members, 5, 0, 25)

	var b strings.Builder
	fmt.Fprintf(&b, "%s — team %d, rank #%s of %s\n", t.Name, t.TeamID,
		fmtInt(int64(t.Rank)), fmtInt(int64(s.Totals.Teams)))
	fmt.Fprintf(&b, "\n  Lifetime      %s points, %s work units\n", fmtInt(t.PointsTotal), fmtInt(t.WUsTotal))
	fmt.Fprintf(&b, "  Per day       %s   (seven-day average)\n", fmtInt(t.PointsPerDay7dAvg))
	fmt.Fprintf(&b, "  Last 24h      %s\n", fmtInt(t.PointsLast24h))
	fmt.Fprintf(&b, "  Today (UTC)   %s\n", fmtInt(t.PointsTodayUTC))
	fmt.Fprintf(&b, "  This month    %s\n", fmtInt(t.PointsThisMonthUTC))
	fmt.Fprintf(&b, "  Members       %s total, %s produced in the last 7 days\n",
		fmtInt(int64(t.MembersTotal)), fmtInt(int64(t.MembersActive)))
	b.WriteString(mcpMovement("  Rank moved    ", t.RankChange24h))

	if members > 0 {
		roster := s.Ranks.TeamMembers(t.TeamID)
		if n := min(members, len(roster)); n > 0 {
			fmt.Fprintf(&b, "\nTop %d of %d members:\n", n, len(roster))
			for _, slot := range roster[:n] {
				m := s.memberView(slot, false)
				fmt.Fprintf(&b, "  %-26s %s points, %s/day\n",
					truncate(m.Name, 26), fmtShort(m.PointsTotal), fmtShort(m.PointsPerDay7dAvg))
			}
		}
	}
	return b.String() + s.mcpFooter(), nil
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
