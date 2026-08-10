package api

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"folding/content"
	"folding/internal/metrics"
	"folding/internal/rank"
	"folding/internal/store"
)

func (s *Server) status(snap *Snapshot, _ *http.Request) (any, *PageInfo, error) {
	// Published here because this route is already no-store and already bypasses the
	// CDN — it exists so a poller sees a publish the moment it lands. A live figure
	// needs exactly that, so it needs no cache rule of its own.
	perSec, last60 := s.rate.rate(time.Now())
	return map[string]any{
		"cycles_retained":  snap.Members.Cycles(),
		"history_span_sec": int64(snap.Members.Span().Seconds()),
		"teams":            len(snap.State.Teams),
		"donors":           len(snap.Ranks.Donors),
		"members":          len(snap.State.Members),
		// Requests this process served in the last minute, and that as a rate. Not
		// what the world asked for: the CDN answers the hottest URLs from cache and
		// those never arrive here. /v1/status itself is excluded.
		"requests_last_60s":   last60,
		"requests_per_second": math.Round(perSec*100) / 100,
	}, nil, nil
}

func (s *Server) summary(snap *Snapshot, _ *http.Request) (any, *PageInfo, error) {
	t := snap.Totals
	out := Summary{
		TeamsTotal:   t.Teams,
		TeamsActive:  t.TeamsActive,
		DonorsTotal:  t.Donors,
		DonorsActive: t.DonorsActive,
		MembersTotal: t.Members,
		Production: Production{
			PointsTotal:       t.PointsTotal,
			WUsTotal:          t.WUsTotal,
			PointsLastUpdate:  t.PointsLastUpdate,
			PointsLast24h:     t.PointsLast24h,
			PointsLast7d:      t.PointsLast7d,
			PointsTodayUTC:    t.PointsToday,
			PointsThisWeekUTC: t.PointsThisWeek,
			// The project has existed for the whole window by definition, so its
			// divisor is the period the retained deltas cover.
			PointsPerDay7dAvg:  metrics.PerDay(t.PointsLast7d, snap.Teams.CoveredSpan()),
			PointsThisMonthUTC: t.PointsThisMonth,
			PointsPerWU:        perWU(t.PointsTotal, t.WUsTotal),
		},
	}
	if d, tm, m, ok := snap.Ranks.NewSince24h(t.Members, t.Teams); ok {
		out.NewDonors24h, out.NewTeams24h, out.NewMembers24h = &d, &tm, &m
	}
	return out, nil, nil
}

// posts lists published articles, newest first.
//
// The blog is served through the same API as everything else rather than as
// server-rendered pages: it keeps one rendering path in the frontend, and it means a
// reader who wants the announcements programmatically has them.
func (s *Server) posts(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	all := content.Published()
	out := make([]PostSummary, 0, len(all))
	for _, p := range all {
		out = append(out, PostSummary{
			Slug: p.Slug, Title: p.Title, Date: p.Date, Summary: p.Summary,
		})
	}
	return map[string]any{"posts": out}, nil, nil
}

// post returns one article with its rendered body.
func (s *Server) post(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	slug := r.PathValue("slug")
	p, ok := content.Lookup(slug)
	if !ok {
		return nil, nil, notFound("no post %q", slug)
	}
	return p, nil, nil
}

// projectHistory is the whole project's production over time.
func (s *Server) projectHistory(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	q, err := parseHistoryQuery(r, snap.At)
	if err != nil {
		return nil, nil, err
	}
	pts, ok := snap.ProjectHist[q.gran]
	if !q.defaulted || !ok {
		if pts, err = snap.Store.ProjectHistory(r.Context(), q.from, q.to, q.gran); err != nil {
			return nil, nil, err
		}
	}
	return historyView(q, pts), nil, nil
}

// sortKeys is the accepted ?sort= set, in the order the docs list them. Kept here so
// the error message and the documentation cannot drift from what is actually served.
var sortKeys = []rank.SortKey{
	rank.Lifetime, rank.PerDay, rank.Today, rank.ThisWeek, rank.ThisMonth,
	rank.Last24h, rank.WUs, rank.Members, rank.Teams,
}

// parseSort resolves the ?sort= leaderboard ordering, defaulting to lifetime.
func parseSort(r *http.Request) (rank.SortKey, error) {
	v := r.URL.Query().Get("sort")
	if v == "" {
		return rank.Lifetime, nil
	}
	k, ok := rank.NormalizeSort(rank.SortKey(v))
	if !ok {
		names := make([]string, len(sortKeys))
		for i, k := range sortKeys {
			names[i] = string(k)
		}
		return "", badRequest("sort must be one of: %s", strings.Join(names, ", "))
	}
	return k, nil
}

func (s *Server) teams(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	sortKey, err := parseSort(r)
	if err != nil {
		return nil, nil, err
	}
	order := snap.Ranks.TeamOrderFor(sortKey)
	lo, hi, page, err := paginate(r, len(order))
	if err != nil {
		return nil, nil, err
	}
	out := make([]Team, 0, hi-lo)
	for _, slot := range order[lo:hi] {
		out = append(out, snap.teamView(slot))
	}
	return out, page, nil
}

func (s *Server) team(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 32)
	if err != nil {
		return nil, nil, badRequest("team id must be an integer")
	}
	slot, ok := snap.State.TeamSlot(int32(id))
	if !ok {
		return nil, nil, notFound("no team with id %d", id)
	}
	return snap.teamDetailView(r.Context(), slot), nil, nil
}

func (s *Server) teamMembers(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 32)
	if err != nil {
		return nil, nil, badRequest("team id must be an integer")
	}
	teamID := int32(id)
	if _, ok := snap.State.TeamSlot(teamID); !ok {
		return nil, nil, notFound("no team with id %d", id)
	}

	// EOC offers an all/active-only filter on its member lists; most teams are
	// overwhelmingly dormant, so the filtered view is usually the one people want.
	activeOnly := r.URL.Query().Get("active_only") == "true"

	// The roster is precomputed per team in rank order, so this is proportional to
	// the team rather than to the corpus. Scanning the global 2.7M-entry order on
	// every request would work, but it would cost the same for a two-person team as
	// for the largest one.
	sortKey, err := parseSort(r)
	if err != nil {
		return nil, nil, err
	}
	// Roster size is a team property, not a member one, so it is not orderable here.
	if sortKey == rank.Members || sortKey == rank.Teams {
		sortKey = rank.Lifetime
	}

	slots := snap.Ranks.TeamMembers(teamID)

	total := len(slots)
	if activeOnly {
		// The count is already known per team from the snapshot build, so the filter
		// does not have to materialise the whole roster just to learn how long it is.
		// Copying every member of the largest team to return a hundred of them made
		// the filtered view twelve times more expensive than the unfiltered one,
		// which is backwards — asking for fewer rows should not cost more.
		_, active := snap.TeamMemberCounts(teamID)
		total = int(active)
	}

	lo, hi, page, err := paginate(r, total)
	if err != nil {
		return nil, nil, err
	}

	// Unfiltered and in the stored order is the common case and stays a slice.
	if !activeOnly && sortKey == rank.Lifetime {
		out := make([]Member, 0, hi-lo)
		for _, slot := range slots[lo:hi] {
			out = append(out, snap.memberView(slot, false))
		}
		return out, page, nil
	}
	ordered := snap.orderRoster(slots, sortKey, activeOnly, lo, hi)
	out := make([]Member, 0, len(ordered))
	for _, slot := range ordered {
		out = append(out, snap.memberView(slot, false))
	}
	return out, page, nil
}

func (s *Server) donors(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	sortKey, err := parseSort(r)
	if err != nil {
		return nil, nil, err
	}
	order := snap.Ranks.DonorOrderFor(sortKey)
	lo, hi, page, err := paginate(r, len(order))
	if err != nil {
		return nil, nil, err
	}
	out := make([]Donor, 0, hi-lo)
	for _, idx := range order[lo:hi] {
		out = append(out, snap.donorView(idx, false))
	}
	return out, page, nil
}

func (s *Server) donor(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	name := r.PathValue("name")
	idx, ok := snap.donorIndexByName(name)
	if !ok {
		return nil, nil, notFound("no donor named %q", name)
	}
	// The per-team breakdown ships with the donor: a client must never have to
	// issue one request per team to assemble a donor's page (R10).
	return snap.donorDetailView(r.Context(), idx), nil, nil
}

// donorTeams is the paginated form of the breakdown, for donors whose inline list
// was capped.
//
// `sort=production` orders by recent output rather than lifetime total. The two
// answer different questions and a donor's biggest lifetime teams are frequently
// dormant: one shared name here holds 55 trillion points on a team that has
// produced nothing in a week, while 87% of its current output comes from a team
// far down the lifetime ranking.
func (s *Server) donorTeams(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	name := r.PathValue("name")
	idx, ok := snap.donorIndexByName(name)
	if !ok {
		return nil, nil, notFound("no donor named %q", name)
	}

	byProduction := false
	switch v := r.URL.Query().Get("sort"); v {
	case "", "points":
	case "production":
		byProduction = true
	default:
		return nil, nil, badRequest("sort must be points or production")
	}

	// Order first, slice, then build only the page's rows. This used to materialise
	// every membership and throw all but one page away: for "PS3", 10,426 Member
	// views — each two arena copies, a team-slot lookup and a window read — to return
	// 100. Measured at 6.0ms against 0.22ms for any other 100-row page, on a public
	// endpoint reachable by name.
	ordered := snap.orderMembers(snap.Ranks.DonorMembers(idx), byProduction)
	lo, hi, page, err := paginate(r, len(ordered))
	if err != nil {
		return nil, nil, err
	}
	return snap.memberViews(ordered[lo:hi]), page, nil
}

func (s *Server) teamHistory(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 32)
	if err != nil {
		return nil, nil, badRequest("team id must be an integer")
	}
	slot, ok := snap.State.TeamSlot(int32(id))
	if !ok {
		return nil, nil, notFound("no team with id %d", id)
	}
	q, err := parseHistoryQuery(r, snap.At)
	if err != nil {
		return nil, nil, err
	}
	pts, err := snap.Store.TeamHistory(r.Context(), slot, q.from, q.to, q.gran)
	if err != nil {
		return nil, nil, err
	}
	return historyView(q, pts), nil, nil
}

func (s *Server) donorHistory(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	name := r.PathValue("name")
	idx, ok := snap.donorIndexByName(name)
	if !ok {
		return nil, nil, notFound("no donor named %q", name)
	}
	q, err := parseHistoryQuery(r, snap.At)
	if err != nil {
		return nil, nil, err
	}

	// A donor's series is the sum of their members'. Restricting to one team is
	// what makes the per-team tabs on a donor page possible.
	members := snap.Ranks.DonorMembers(idx)
	if raw := r.URL.Query().Get("team_id"); raw != "" {
		tid, err := strconv.ParseInt(raw, 10, 32)
		if err != nil {
			return nil, nil, badRequest("team_id must be an integer")
		}
		var filtered []int32
		for _, slot := range members {
			if snap.State.Members[slot].TeamID == int32(tid) {
				filtered = append(filtered, slot)
			}
		}
		if len(filtered) == 0 {
			return nil, nil, notFound("donor %q has no record on team %d", name, tid)
		}
		members = filtered
	}

	// Bound the query as well as the round trips. Summing across every member of a
	// name that spans thousands of teams means an IN list that large, and the cost
	// shows up directly in the tail: it was the only endpoint left above 1 ms at
	// p99. The teams are ordered by points, so a capped result is the aggregate of
	// the ones that actually matter — and it says so.
	truncated := false
	if len(members) > maxHistoryTeams {
		// A donor's members are stored in global rank order, which is lifetime points
		// order, so the ones that matter most are already at the front. This used to
		// build a hundred Member views and then look their names back up to recover
		// the slots it started with — a round trip through the view layer, two map
		// lookups per row, to select a prefix.
		members, truncated = members[:maxHistoryTeams], true
	}

	pts, err := snap.Store.MembersHistory(r.Context(), members, q.from, q.to, q.gran)
	if err != nil {
		return nil, nil, err
	}
	h := historyView(q, pts)
	h.TeamsTruncated = truncated
	h.TeamsIncluded = len(members)
	return h, nil, nil
}

// maxHistoryTeams bounds how many of a donor's teams contribute to a merged series.
const maxHistoryTeams = 100

func (s *Server) search(snap *Snapshot, r *http.Request) (any, *PageInfo, error) {
	q := r.URL.Query().Get("q")
	if q == "" {
		return nil, nil, badRequest("q is required")
	}
	kind := r.URL.Query().Get("type")

	limit, err := intParam(r, "limit", defaultSearchLimit)
	if err != nil {
		return nil, nil, err
	}
	if limit < 1 || limit > maxSearchLimit {
		return nil, nil, badRequest("limit must be between 1 and %d", maxSearchLimit)
	}

	res := SearchResults{Query: q, Teams: []Team{}, Donors: []Donor{}}

	// An exact, case-sensitive hit always leads, then case-insensitive prefix
	// matches. There is no minimum length: EOC's three-character floor makes short
	// real names such as "DH" permanently unfindable, which is the whole reason
	// this endpoint exists.
	if kind == "" || kind == "donor" {
		seen := map[int32]bool{}
		if idx, ok := snap.donorIndexByName(q); ok {
			res.Donors = append(res.Donors, snap.donorView(idx, false))
			res.ExactDonor = true
			seen[idx] = true
		}
		for _, idx := range snap.Ranks.DonorPrefix(snap.State, q, limit) {
			if seen[idx] || len(res.Donors) >= limit {
				continue
			}
			res.Donors = append(res.Donors, snap.donorView(idx, false))
		}
	}

	if kind == "" || kind == "team" {
		seen := map[int32]bool{}
		add := func(slot int32) {
			if seen[slot] || len(res.Teams) >= limit {
				return
			}
			seen[slot] = true
			res.Teams = append(res.Teams, snap.teamView(slot))
		}
		if id, err := strconv.ParseInt(q, 10, 32); err == nil {
			if slot, ok := snap.State.TeamSlot(int32(id)); ok {
				add(slot)
				res.ExactTeam = true
			}
		}

		// One indexed lookup, where there used to be two full passes over every team.
		//
		// The old exact-name branch scanned all ~130k teams whenever the query was an
		// interned string — which every one of the 2.1M donor names is — and the prefix
		// branch scanned them again, allocating a string per team to read the name and
		// another to lowercase it. A query matching nothing did both and cost 16.8ms
		// against a 0.18ms baseline, on the endpoint the search box calls as you type.
		//
		// An exact name is a prefix of itself, so both questions are answered by the
		// same range. Exact hits lead: the index is ordered case-insensitively, so a
		// name equal to the query sorts ahead of every longer name sharing it.
		exactNameID, hasExactName := snap.State.Names.Lookup(q)
		hits := snap.Ranks.TeamPrefix(snap.State, q, limit)
		if hasExactName {
			exact := make([]int32, 0, len(hits))
			rest := make([]int32, 0, len(hits))
			for _, slot := range hits {
				if snap.State.Teams[slot].NameID == exactNameID {
					exact = append(exact, slot)
				} else {
					rest = append(rest, slot)
				}
			}
			if len(exact) > 0 {
				res.ExactTeam = true
				hits = append(exact, rest...)
			}
		}
		for _, slot := range hits {
			add(slot)
		}
	}
	return res, nil, nil
}

const (
	defaultSearchLimit = 8
	maxSearchLimit     = 50
)

type historyQuery struct {
	metric   string
	gran     store.Granularity
	from, to time.Time
	// defaulted marks a request that named neither from nor to, so its range is
	// derived entirely from the snapshot and the granularity. Those are the only
	// ranges worth precomputing: there are four of them per cycle and they are what
	// the site itself asks for, while a caller-supplied window is unbounded in
	// variety and must not be allowed to fill a cache.
	defaulted bool
}

// defaultHistoryRange is the window an unparameterised history request covers.
//
// One definition, called by the request parser and by the precompute that warms
// them. Two copies of this would drift, and a precomputed range that disagreed with
// the parsed one by a second would serve a cached answer to a different question —
// silently, since both are plausible-looking lists of points.
func defaultHistoryRange(now time.Time, g store.Granularity) (from, to time.Time) {
	return now.Add(-defaultWindow[g]), now.Add(time.Hour)
}

func parseHistoryQuery(r *http.Request, now time.Time) (historyQuery, error) {
	q := historyQuery{metric: "points", gran: store.Hourly}

	if m := r.URL.Query().Get("metric"); m != "" {
		if m != "points" && m != "wus" {
			return q, badRequest("metric must be points or wus")
		}
		q.metric = m
	}
	switch g := r.URL.Query().Get("granularity"); g {
	case "":
	// "cycle" was the original name for "hourly" and still works, so existing
	// callers keep functioning.
	case "hourly", "cycle", "daily", "weekly", "monthly":
		q.gran = store.Granularity(g).Normalize()
	default:
		return q, badRequest("granularity must be hourly, daily, weekly or monthly")
	}

	var err error
	// Default windows are per-granularity: an unparameterised hourly request over
	// 30 days is 720 points of noise, while a month of daily buckets is too little
	// to see a trend. Asking for more is deliberate.
	q.from, q.to = defaultHistoryRange(now, q.gran)
	q.defaulted = true
	if v := r.URL.Query().Get("from"); v != "" {
		if q.from, err = time.Parse(time.RFC3339, v); err != nil {
			return q, badRequest("from must be an RFC3339 timestamp")
		}
		q.defaulted = false
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if q.to, err = time.Parse(time.RFC3339, v); err != nil {
			return q, badRequest("to must be an RFC3339 timestamp")
		}
		q.defaulted = false
	}
	if !q.to.After(q.from) {
		return q, badRequest("to must be after from")
	}
	// Cap the span. At cycle granularity an unbounded range would scan every delta
	// a donor ever produced, and for a name spanning thousands of teams that is a
	// cheap way to make one request expensive for everyone.
	max, ok := maxRange[q.gran]
	if !ok {
		// A missing entry would otherwise read as a zero limit and reject every
		// range, which looks like "no data" rather than a configuration mistake.
		return q, fmt.Errorf("no range limit configured for granularity %q", q.gran)
	}
	if q.to.Sub(q.from) > max {
		return q, badRequest("range of %s exceeds the %s maximum for granularity %q",
			q.to.Sub(q.from).Round(time.Hour), max, q.gran)
	}
	return q, nil
}

// defaultWindow is how far back an unparameterised history request reaches.
var defaultWindow = map[store.Granularity]time.Duration{
	store.Hourly: 7 * 24 * time.Hour,
	store.Daily:  90 * 24 * time.Hour,
	// A year of weeks is 52 points — dense enough to read a seasonal trend, which is
	// the thing weekly buckets are for and that daily buries in noise.
	store.Weekly:  365 * 24 * time.Hour,
	store.Monthly: 3 * 365 * 24 * time.Hour,
}

// maxRange bounds a history query per granularity. Coarser buckets return far fewer
// rows per unit time, so they can span proportionally more.
var maxRange = map[store.Granularity]time.Duration{
	store.Hourly: 90 * 24 * time.Hour, // matches raw delta retention
	store.Daily:  5 * 365 * 24 * time.Hour,
	// Weekly sums the daily rollup on read, so its ceiling is daily's: past that
	// there are no day buckets left to sum.
	store.Weekly:  5 * 365 * 24 * time.Hour,
	store.Monthly: 50 * 365 * 24 * time.Hour,
}

func historyView(q historyQuery, pts []store.Point) History {
	out := make([]HistoryPoint, len(pts))
	for i, p := range pts {
		out[i] = HistoryPoint{At: p.At, Points: p.Points, WUs: p.WUs}
	}
	return History{
		Metric: q.metric, Granularity: string(q.gran),
		From: q.from, To: q.to, Points: out,
	}
}
