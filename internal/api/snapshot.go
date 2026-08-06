// Package api serves the read-only HTTP interface.
//
// Everything served between ingest cycles is a pure function of one immutable
// Snapshot, so handlers never lock, never query, and never recompute. A cycle builds
// a fresh Snapshot and swaps it in atomically; in-flight requests finish against the
// old one.
//
// # Resource model
//
// EOC exposes one page template at three scopes — site aggregate, team, user — with
// blocks dropped where they do not apply. That structure is sound and is kept, but
// expressed as a proper hierarchy rather than as a stateful sidebar selection:
//
//	/v1/summary                      the whole project
//	/v1/teams/{id}                   one team
//	/v1/donors/{name}                one donor, aggregated across teams
//
// The significant departure is the donor. EOC keys users per team, so one person on
// three teams is three unrelated users with three ranks and no way to relate them.
// Here a donor is a name aggregated across every team it folds for, with the per-team
// breakdown embedded — so the question "how am I doing overall?" has an answer.
package api

import (
	"context"
	"sync"
	"time"

	"folding/internal/metrics"
	"folding/internal/model"
	"folding/internal/rank"
	"folding/internal/store"
)

// Snapshot is the complete served view of one ingest cycle. Every field is
// read-only once published.
type Snapshot struct {
	State *model.State
	Ranks *rank.Table
	Store *store.Store

	// Members and Teams are separate windows because their slot numbering is
	// separate: member slot 7 and team slot 7 are unrelated entities. Sharing one
	// window silently reports one entity's production as the other's.
	Members *metrics.Window
	Teams   *metrics.Window

	// TeamMonth and MemberMonth are month-to-date production, indexed by slot. The
	// rolling windows cover seven days, so the calendar month is the one period
	// figure that has to come from the rollup tables rather than from memory.
	TeamMonth   []int64
	MemberMonth []int64

	// At is the upstream publish time this snapshot reflects, and NextExpected is
	// when the following one should arrive. Serving both lets clients cache
	// precisely rather than polling blindly.
	At           time.Time
	NextExpected time.Time

	// Stale marks a snapshot still being served after its successor failed to
	// arrive. Upstream feed outages are routine, so this is expected rather than
	// exceptional — but it must be visible, never silently papered over.
	// StaleAfter is when this snapshot becomes late: the predicted publish plus a
	// grace period covering routine drift and our own poll interval.
	StaleAfter time.Time

	// Interval is the measured upstream publish cadence, which is not the hour it
	// nominally is. Clients counting down to the next update need this rather than
	// their own assumption.
	Interval time.Duration
	// IntervalMeasured is false while Interval is still the fallback, so a client can
	// present the countdown as approximate instead of implying a precision it lacks.
	IntervalMeasured bool

	// ETag identifies this snapshot for conditional requests.
	ETag string

	// CollectionStart is the earliest snapshot ever ingested — where the record
	// begins, as distinct from where the rolling windows do.
	//
	// It bounds every streak. An entity that has produced on every day we have watched
	// has a streak equal to the age of this service, which is a fact about the service
	// and not about the entity, and the response has to say which it is reporting.
	CollectionStart time.Time

	// ProjectHist is the default-window project history for each granularity,
	// computed once when the snapshot is published.
	//
	// It is the one endpoint whose cost does not shrink with paging: every other
	// route reads a bounded slice of one entity, while this sums every team in the
	// database. That made it 2.8ms against a 0.09ms median, on the request the
	// overview page issues first.
	//
	// A missing entry is not an error — the handler runs the query. Nothing depends
	// on the cache being populated, so a Snapshot built without it (every test that
	// calls Build directly) still answers correctly, just slower.
	ProjectHist map[store.Granularity][]store.Point

	// Guard protects State, Members and Teams, which are the live ingest
	// structures rather than copies.
	//
	// Publishing a new snapshot does not detach the old one: both point at the same
	// arrays, which the next cycle mutates in place and may reallocate. Copying
	// instead would mean duplicating ~300 MB every hour. So readers take the read
	// lock and the ingest loop takes the write lock for the ~1.3 s it spends
	// mutating — about 0.04% of each hour.
	//
	// May be nil in tests that never ingest concurrently.
	Guard *sync.RWMutex

	// Per-team member counts, computed once here rather than per request.
	teamMembers []int32
	teamActive  []int32

	// Site-wide aggregates.
	Totals Totals
}

// Totals is the site-wide roll-up: the whole project as a single entity.
type Totals struct {
	PointsTotal int64
	WUsTotal    int64

	PointsLastUpdate int64
	PointsToday      int64
	PointsThisWeek   int64
	PointsLast24h    int64
	PointsLast7d     int64
	// PointsThisMonth is summed from the rollup rather than the windows, so unlike
	// its neighbours it is filled in after Build, once the month totals are attached.
	PointsThisMonth int64

	Teams        int
	Donors       int
	Members      int
	DonorsActive int
	TeamsActive  int
}

// Build assembles a served snapshot from freshly ingested state.
func Build(st *model.State, members, teams *metrics.Window, tbl *rank.Table, s *store.Store,
	at, nextExpected time.Time, etag string) *Snapshot {

	snap := &Snapshot{
		State: st, Members: members, Teams: teams, Ranks: tbl, Store: s,
		At: at, NextExpected: nextExpected, ETag: etag,
	}

	maxTeamID := int32(0)
	for _, m := range st.Members {
		if m.TeamID > maxTeamID {
			maxTeamID = m.TeamID
		}
	}
	snap.teamMembers = make([]int32, maxTeamID+1)
	snap.teamActive = make([]int32, maxTeamID+1)

	// "Active" means produced anything in the trailing 7 days. EOC never documents
	// its own definition; ours matches the averaging window, so "active" and
	// "points per day" tell a consistent story.
	for slot, m := range st.Members {
		snap.teamMembers[m.TeamID]++
		if members.Last7d(int32(slot)) > 0 {
			snap.teamActive[m.TeamID]++
		}
	}

	t := Totals{
		Teams:   len(st.Teams),
		Donors:  len(tbl.Donors),
		Members: len(st.Members),
	}
	// Site totals come from the team feed, which is authoritative: it exceeds the
	// sum of member rows for ~0.16% of teams because the two feeds publish a minute
	// apart, and some production is not attributable to any listed donor.
	for slot, tm := range st.Teams {
		t.PointsTotal += tm.Score
		t.WUsTotal += tm.WUs
		id := int32(slot)
		t.PointsLastUpdate += teams.LastUpdate(id)
		t.PointsToday += teams.Today(id)
		t.PointsThisWeek += teams.ThisWeek(id)
		t.PointsLast24h += teams.Last24h(id)
		t.PointsLast7d += teams.Last7d(id)
		if teams.Last7d(id) > 0 {
			t.TeamsActive++
		}
	}
	for i := range tbl.Donors {
		if snap.donorLast7d(int32(i)) > 0 {
			t.DonorsActive++
		}
	}
	snap.Totals = t
	return snap
}

// TeamMemberCounts returns the total and active member counts for a team.
func (s *Snapshot) TeamMemberCounts(teamID int32) (total, active int32) {
	if teamID < 0 || int(teamID) >= len(s.teamMembers) {
		return 0, 0
	}
	return s.teamMembers[teamID], s.teamActive[teamID]
}

// donorWindow sums a window across every member belonging to a donor. Donors are a
// read-time view (R1), so their rates are summed on demand rather than stored.
func (s *Snapshot) donorWindow(donorIdx int32, f func(int32) int64) int64 {
	var n int64
	for _, slot := range s.Ranks.DonorMembers(donorIdx) {
		n += f(slot)
	}
	return n
}

func (s *Snapshot) donorLast7d(i int32) int64 {
	return s.donorWindow(i, s.Members.Last7d)
}

// AvgWindowComplete reports whether the 7-day average spans a full week yet. During
// the first week of collection it does not, and the average reads low — the API says
// so rather than presenting a number that looks authoritative but is not.
func (s *Snapshot) AvgWindowComplete() bool { return s.Members.Complete() }

// WarmProjectHistory precomputes the project-wide history for the four default
// windows, so the request path never runs the aggregate.
//
// The whole endpoint is a GROUP BY over every team that produced in the range —
// millions of rows at hourly granularity — and the answer is identical for every
// caller until the next cycle lands. Doing it once per publish rather than once per
// request is the entire optimisation; there is nothing to invalidate, because a
// snapshot is replaced wholesale rather than updated.
//
// Errors are returned rather than stored. A failure here must leave the map empty so
// the handler falls back to querying, never cache a truncated list as if it were the
// history.
func (s *Snapshot) WarmProjectHistory(ctx context.Context) error {
	if s.Store == nil {
		return nil
	}
	out := make(map[store.Granularity][]store.Point, 4)
	for _, g := range []store.Granularity{store.Hourly, store.Daily, store.Weekly, store.Monthly} {
		from, to := defaultHistoryRange(s.At, g)
		pts, err := s.Store.ProjectHistory(ctx, from, to, g)
		if err != nil {
			return err
		}
		out[g] = pts
	}
	s.ProjectHist = out
	return nil
}
