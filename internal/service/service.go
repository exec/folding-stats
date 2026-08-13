// Package service wires the pipeline together: archived snapshots in, a published
// API view out.
//
// Ingest is deliberately driven from the archive rather than from the network. The
// archiver's only job is to get bytes onto disk; this package consumes whatever is
// there. That means live ingest and historical replay are the same code path, so a
// metric change costs a replay rather than a gap in history.
package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"folding/internal/api"
	"folding/internal/feed"
	"folding/internal/metrics"
	"folding/internal/model"
	"folding/internal/parse"
	"folding/internal/rank"
	"folding/internal/store"
)

// staleGrace is how late a publish may be before the data is called stale.
//
// Upstream does not run on a wall clock. Measured intervals are 3606–3613s — always
// longer than an hour, drifting about 10s later each cycle, so the publish time
// sweeps a full hour roughly every two weeks. On top of that we poll every 10
// minutes and the user feed takes ~30s to fetch and decompress.
//
// Without grace, "stale" would be true for several minutes of every single hour,
// which is a false alarm every cycle and would train anyone reading the flag to
// ignore it. The grace covers routine lateness so the flag means what it says:
// upstream missed a publish.
const staleGrace = 20 * time.Minute

// windowSpan is how much history the rolling metrics windows cover, and therefore
// how far back a restart must replay deltas to rebuild them.
const windowSpan = 7 * 24 * time.Hour

// cycleWriteTimeout bounds a cycle's commit once it has started. Measured writes are
// ~50ms at corpus scale, so this is a wedged-database ceiling rather than a budget.
const cycleWriteTimeout = 60 * time.Second

// Service owns the mutable ingest state and publishes immutable views.
type Service struct {
	Archive *feed.Archive
	Store   *store.Store
	Server  *api.Server
	Log     *slog.Logger

	state *model.State
	// Separate windows: member and team slots are distinct id spaces that both
	// start at zero, so one shared window reports a member's production as a team's.
	memberWin *metrics.Window
	teamWin   *metrics.Window
	applied   map[int64]bool
	ingestErr error // fail-stop after a destructive Apply outlives a failed commit
	// cadence learns the upstream publish interval from observation. It drives both
	// the poll schedule and the next_expected_at the API advertises.
	cadence *cadence

	// purge drops the CDN's cached copies when a new snapshot lands, closing the gap
	// between a publish and the edge TTL lapsing. Nil when unconfigured, which is a
	// working no-op — the TTL alone is already correct, just less prompt.
	purge *purger

	// Month-to-date production, indexed by team slot and member slot. Refreshed on
	// ingest because that is the only thing that changes it: the rollup tables are
	// updated inside the cycle's own transaction.
	teamMonth   []int64
	memberMonth []int64

	// tbl is the published ranking, and tblAt the state it was built from. The table
	// is a pure function of state, so the key is enough to tell a rebuild from a
	// reuse — provided nothing publishes while state.At and the windows disagree,
	// which is why publish has a single caller. See the note there.
	tbl   *rank.Table
	tblAt time.Time

	// firstCycle is when collection started, read once and cached: it is the minimum
	// of a table nothing deletes from the front of, so it cannot move while we run.
	// Streaks are bounded by it — a run reaching this day is a lower bound rather than
	// a fact about the entity.
	firstCycle time.Time

	// guard serialises mutation of state and the windows against API reads, which
	// serve directly from those structures rather than from a copy.
	guard sync.RWMutex
	// publishMu serialises publishes against each other. Ingest and the periodic
	// refresh both call publish from their own goroutines, and guard is held for
	// reading there — which admits concurrent holders and so cannot protect the
	// table cache from being written twice.
	publishMu sync.Mutex
}

var (
	maxTeamRows = feed.MaxTeamRows
	maxUserRows = feed.MaxUserRows
)

// New returns a Service with identity restored from the store.
func New(archive *feed.Archive, st *store.Store, srv *api.Server, log *slog.Logger) (*Service, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Service{
		Archive: archive, Store: st, Server: srv, Log: log,
		state:     model.NewState(),
		memberWin: metrics.New(0),
		teamWin:   metrics.New(0),
		cadence:   newCadence(),
		// Credentials come from the environment rather than a flag: a flag is visible
		// in the process list to every user on the box.
		purge: newPurger(os.Getenv("FOLDING_CF_ZONE_ID"), os.Getenv("FOLDING_CF_PURGE_TOKEN"),
			os.Getenv("FOLDING_SITE_URL"), log),
	}
	if s.purge == nil {
		log.Info("cdn cache purging disabled; the edge will expire on Cache-Control alone")
	}
	ctx := context.Background()
	if err := st.LoadIdentity(ctx, s.state); err != nil {
		return nil, fmt.Errorf("service: restoring identity: %w", err)
	}
	applied, err := st.AppliedCycles(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: reading applied cycles: %w", err)
	}
	s.applied = applied

	latest, err := st.LatestCycle(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: reading latest cycle: %w", err)
	}
	s.state.At = latest

	if err := s.restoreWindows(ctx, latest); err != nil {
		return nil, fmt.Errorf("service: restoring windows: %w", err)
	}
	s.observeCadence(ctx)
	s.refreshMonthTotals(ctx)

	s.Log.Info("state restored",
		"members", len(s.state.Members), "teams", len(s.state.Teams),
		"names", s.state.Names.Len(), "cycles_applied", len(applied),
		"latest", latest.Format(time.RFC3339),
		"window_cycles", s.memberWin.Cycles(),
		"publish_interval", s.cadence.Interval())
	return s, nil
}

// restoreWindows replays the stored deltas covering the rolling windows.
//
// Cumulative totals come back from the identity tables, but the rates do not: they
// are derived from the last seven days of deltas. Skipping this leaves the service
// serving correct totals with every rate at zero, recovering only after a week — a
// failure that reports no error at all, which is why it is done at startup rather
// than lazily.
func (s *Service) restoreWindows(ctx context.Context, latest time.Time) error {
	if latest.IsZero() {
		return nil
	}
	cycles, err := s.Store.DeltasSince(ctx, latest.Add(-windowSpan))
	if err != nil {
		return err
	}

	// Replay the corpus growing, not just the deltas. The windows record how many
	// entities existed at each cycle, and that is what tells a donor watched all week
	// apart from one first seen an hour ago — which decides the divisor of their
	// per-day average and whether they have a comparable rank a day back.
	//
	// Growing straight to today's size instead would stamp today's count on every
	// replayed cycle, so every entity would look as though it had been present for
	// the whole window. Everything created shortly before a restart would then be
	// averaged over seven days it did not exist for, and would stay wrong until the
	// window rolled past the restart.
	//
	// Counts come from walking the first-sighting totals backwards from the present,
	// because that is the direction the arithmetic is exact in: today's size is known
	// and each cycle says how many it added.
	memberAt := make([]int, len(cycles))
	teamAt := make([]int, len(cycles))
	members, teams := len(s.state.Members), len(s.state.Teams)
	for i := len(cycles) - 1; i >= 0; i-- {
		memberAt[i], teamAt[i] = members, teams
		members -= int(cycles[i].NewMembers)
		teams -= int(cycles[i].NewTeams)
	}

	for i, c := range cycles {
		s.memberWin.Grow(memberAt[i])
		s.memberWin.Push(c.At, c.Members)
		s.teamWin.Grow(teamAt[i])
		s.teamWin.Push(c.At, c.Teams)
	}
	// Anything created after the newest replayed cycle — or before the window, if the
	// audit log is short — still needs a slot.
	s.memberWin.Grow(len(s.state.Members))
	s.teamWin.Grow(len(s.state.Teams))
	return nil
}

// snapshotPair is a team and user snapshot matched into one ingestable cycle.
type snapshotPair struct {
	at   time.Time
	team feed.Snapshot
	user feed.Snapshot
}

// pairSnapshots matches team and user snapshots into cycles.
//
// The feeds are not published atomically — the team file lands about a minute before
// the user file — so pairing has to be by proximity rather than by equality. Each
// user snapshot is matched with the newest team snapshot at or before it, which is
// the pairing upstream itself implies.
func pairSnapshots(teams, users []feed.Snapshot) []snapshotPair {
	if len(teams) == 0 {
		return nil
	}
	var out []snapshotPair
	ti := 0 // index of the newest team snapshot at or before the current user one
	for _, u := range users {
		for ti+1 < len(teams) && !teams[ti+1].Meta.SnapshotAt.After(u.Meta.SnapshotAt) {
			ti++
		}
		if teams[ti].Meta.SnapshotAt.After(u.Meta.SnapshotAt) {
			continue // nothing published before this user snapshot yet
		}
		// A team snapshot is deliberately not consumed by the pairing. When the
		// team feed misses a publish but the user feed does not, reusing the
		// previous one costs a zero team delta that the next cycle absorbs —
		// whereas skipping the cycle would throw away an hour of donor production,
		// unrecoverably.
		out = append(out, snapshotPair{
			at:   u.Meta.SnapshotAt,
			team: teams[ti],
			user: u,
		})
	}
	return out
}

// Ingest applies every archived cycle that has not been applied yet, oldest first,
// and publishes the resulting view. Returns the number of cycles applied.
func (s *Service) Ingest(ctx context.Context) (int, error) {
	if s.ingestErr != nil {
		return 0, s.ingestErr
	}
	// Only look at snapshots we could still need. Listing the whole archive would
	// mean parsing every sidecar ever written — ~17k a year — on every hourly pass.
	// The window reaches back past the newest applied cycle so a team snapshot
	// published just before it is still available to pair against.
	since := time.Time{}
	if !s.state.At.IsZero() {
		since = s.state.At.Add(-2 * nominalInterval)
	}
	teams, err := s.Archive.ListSince(feed.Teams, since)
	if err != nil {
		return 0, err
	}
	users, err := s.Archive.ListSince(feed.Users, since)
	if err != nil {
		return 0, err
	}

	var n int
	for _, p := range pairSnapshots(teams, users) {
		if s.applied[p.at.Unix()] {
			continue
		}
		if err := s.applyCycle(ctx, p); err != nil {
			// One bad snapshot must not block the ones after it, but it also must
			// not be silently forgotten.
			s.Log.Error("cycle failed", "at", p.at.Format(time.RFC3339), "err", err)
			if s.ingestErr != nil {
				return n, s.ingestErr
			}
			continue
		}
		s.applied[p.at.Unix()] = true
		n++
	}
	if n > 0 {
		// Each new cycle is another interval observed, so the prediction sharpens as
		// the drift moves rather than being fixed at startup.
		s.observeCadence(ctx)
		s.refreshMonthTotals(ctx)
	}
	if n > 0 || s.Server.Current() == nil {
		s.publish()
	}
	return n, nil
}

func (s *Service) applyCycle(ctx context.Context, p snapshotPair) error {
	start := time.Now()

	teamRows, teamStats, err := readTeams(p.team)
	if err != nil {
		return err
	}
	userRows, userStats, err := readUsers(p.user)
	if err != nil {
		return err
	}
	if err := model.ValidateSnapshot(teamRows, userRows); err != nil {
		return fmt.Errorf("rejecting unsafe snapshot: %w", err)
	}

	s.guard.Lock()
	cycle := s.state.Apply(p.at, teamRows, userRows)
	s.guard.Unlock()

	// The commit deliberately outlives a cancelled parent.
	//
	// state.Apply above is destructive — it advances every touched total to the new
	// snapshot — and there is no per-cycle undo. So an aborted write is not a clean
	// rollback: the database rolls back, the model does not, and the two disagree
	// permanently. The hour's production exists only as the diff that Apply just
	// consumed, so replaying the same snapshot produces an empty cycle, and the slots
	// Apply assigned are never persisted, which makes the next LoadIdentity fail
	// outright.
	//
	// Shutdown cancels ctx while this goroutine may be exactly here, so the write
	// gets its own deadline instead of the caller's cancellation. Bounded rather than
	// unbounded: a wedged write must not hold shutdown open forever, and a cycle
	// commit is ~50ms against 2.7M members.
	writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), cycleWriteTimeout)
	defer cancelWrite()
	if err := s.Store.WriteCycle(writeCtx, s.state, cycle, store.CycleMeta{
		TeamSnapshotAt: p.team.Meta.SnapshotAt,
		UserSnapshotAt: p.user.Meta.SnapshotAt,
		TeamRows:       teamStats.Rows,
		UserRows:       userStats.Rows,
		Malformed:      teamStats.Malformed + userStats.Malformed,
		Duration:       time.Since(start),
	}); err != nil {
		s.ingestErr = fmt.Errorf("cycle commit failed after in-memory apply; restart required: %w", err)
		return s.ingestErr
	}

	s.guard.Lock()
	s.memberWin.Grow(len(s.state.Members))
	s.memberWin.Push(p.at, cycle.MemberDeltas)
	s.teamWin.Grow(len(s.state.Teams))
	s.teamWin.Push(p.at, cycle.TeamDeltas)
	s.guard.Unlock()

	s.Log.Info("cycle applied",
		"at", p.at.Format(time.RFC3339),
		"members", len(s.state.Members),
		"member_deltas", len(cycle.MemberDeltas),
		"new_members", len(cycle.NewMembers),
		"regressions", cycle.Regressions,
		"took", time.Since(start).Round(time.Millisecond))
	s.warnRegressions(cycle)
	return nil
}

// warnRegressions raises the alarm when a cumulative total went down.
//
// Points are only ever awarded, so a lifetime score can only rise. A fall means the
// feed contradicted itself, or a name was reused for a different entity, or we joined
// two rows that are not the same donor — and in every one of those cases the deltas
// derived from that cycle are wrong. It carries the same weight as an ingest failure
// and reads as one, rather than sitting as a field on a routine line nobody greps.
func (s *Service) warnRegressions(c *model.Cycle) {
	if c.Regressions == 0 {
		return
	}
	name := func(id int32, team bool) string {
		if team {
			return fmt.Sprintf("team %d", s.state.Teams[id].ID)
		}
		m := s.state.Members[id]
		return fmt.Sprintf("%s on team %d", s.state.Names.Name(m.NameID), m.TeamID)
	}
	var who []string
	for _, d := range c.RegressedMembers {
		who = append(who, fmt.Sprintf("%s (%d)", name(d.ID, false), d.DScore))
	}
	for _, d := range c.RegressedTeams {
		who = append(who, fmt.Sprintf("%s (%d)", name(d.ID, true), d.DScore))
	}
	s.Log.Warn("cumulative scores decreased upstream",
		"at", c.At.Format(time.RFC3339),
		"count", c.Regressions,
		"sample", strings.Join(who, ", "))
}

// publish rebuilds the ranked view when the corpus has moved, and swaps it in.
//
// Only ingest calls this, and only after the cycle's deltas have reached the windows.
// That ordering is load-bearing rather than incidental: applyCycle advances state.At
// under one lock and pushes the deltas under a later one, so anything publishing
// between the two builds a table from windows that do not yet hold the cycle — and
// caches it under the new state.At, so ingest's own publish then finds a matching key
// and keeps the stale table for the rest of the cycle. Every rate ordering and every
// rank_change_24h would be a cycle behind.
//
// A periodic Refresh used to do exactly that. It was removed rather than fixed: it
// republished a byte-identical snapshot every five minutes, because everything it
// could have changed — staleness above all — is computed per request from the live
// structures instead. If a future field ever does need to age, compute it in the
// handler the way Stale is, rather than reintroducing a second publisher here.
func (s *Service) publish() {
	start := time.Now()

	s.publishMu.Lock()
	defer s.publishMu.Unlock()

	// Read lock: building the ranked view only reads state, but it must not run
	// while a cycle is mutating it.
	s.guard.RLock()
	defer s.guard.RUnlock()

	// Bracket the build: the old table is still referenced by s.tbl throughout, so
	// this is the one window where two of them are reachable at once.
	rebuilt := s.tbl == nil || !s.tblAt.Equal(s.state.At)
	var memBefore, memBuilt memSample
	if rebuilt {
		// Collected, so it is comparable with the peak that follows. An uncollected
		// baseline carries an hour of accumulated request garbage, which made
		// table_cost come out *negative* — the build appeared to free memory,
		// because the collection that ran during it swept more than the new table
		// added. Collecting first also hands the build a clean heap to allocate
		// into, which lowers the peak rather than only describing it.
		memBefore = readMemSettled()
		tbl := rank.Build(s.state, s.state.At, rank.DefaultConfig)
		tbl.BuildChange24h(s.state, s.memberWin, s.teamWin)
		tbl.BuildOrders(s.state, s.memberWin, s.teamWin, s.teamMonth, s.memberMonth)
		memBuilt = readMem()
		s.tbl, s.tblAt = tbl, s.state.At
	}
	tbl := s.tbl

	next := s.cadence.NextAfter(s.state.At)
	// The ETag identifies the snapshot; the cycle time is exactly that identity.
	etag := fmt.Sprintf("%d", s.state.At.Unix())

	snap := api.Build(s.state, s.memberWin, s.teamWin, tbl, s.Store,
		s.state.At, next, etag)
	snap.Guard = &s.guard
	snap.TeamMonth, snap.MemberMonth = s.teamMonth, s.memberMonth
	// Where the record begins, which bounds every streak. Read once and cached for the
	// life of the process: it is the minimum of a table nothing ever deletes from the
	// front of, so it cannot change while we are running.
	if s.firstCycle.IsZero() {
		if at, err := s.Store.FirstCycle(context.Background()); err != nil {
			s.Log.Warn("could not read the first cycle; streaks will not be marked as bounded", "err", err)
		} else {
			s.firstCycle = at
		}
	}
	snap.CollectionStart = s.firstCycle
	// Team totals are authoritative for the project, and summing 130k of them once a
	// cycle beats doing it per request on a cached endpoint.
	for _, v := range s.teamMonth {
		snap.Totals.PointsThisMonth += v
	}
	snap.StaleAfter = next.Add(staleGrace)
	snap.Interval = s.cadence.Interval()
	snap.IntervalMeasured = s.cadence.Measured()

	// Aggregate the project history now rather than per request. This runs under the
	// read lock, which the queries do not need — but it costs about 5ms once an hour
	// against an ingest that already holds the write lock for ~1.3s, so splitting the
	// lock to save it would buy nothing and add a second place state can be read.
	//
	// Failure is logged, not fatal: the snapshot serves correctly without the cache,
	// and refusing to publish a good snapshot because an optimisation failed would
	// turn a slow endpoint into an outage.
	// Bounded, because this holds the read lock: a query that never returns would
	// block every future ingest rather than just this optimisation.
	warmCtx, cancelWarm := context.WithTimeout(context.Background(), cycleWriteTimeout)
	if err := snap.WarmProjectHistory(warmCtx); err != nil {
		s.Log.Warn("project history precompute failed; serving it from the query path",
			"err", err)
	}
	cancelWarm()

	s.Server.Publish(snap)
	// After publishing, never before: a purge that landed first would have the edge
	// refetch the snapshot this one replaces.
	s.purge.Purge(s.state.At)
	s.Log.Info("published",
		"at", s.state.At.Format(time.RFC3339),
		"teams", len(s.state.Teams), "donors", len(tbl.Donors),
		"stale_after", snap.StaleAfter.Format(time.RFC3339),
		"took", time.Since(start).Round(time.Millisecond))

	// Only on a rebuild: a republish without one says nothing about the cost of the
	// table, and logging it anyway would put figures in the series that are not
	// comparable with the rest.
	if rebuilt {
		logMemory(s.Log, memBefore, memBuilt, readMemSettled(), len(tbl.Donors), len(s.state.Members))
	}
}

func readTeams(s feed.Snapshot) ([]parse.TeamRow, parse.Stats, error) {
	rc, err := s.Open()
	if err != nil {
		return nil, parse.Stats{}, err
	}
	defer rc.Close()
	sc := parse.NewTeamScanner(rc)
	var rows []parse.TeamRow
	for sc.Scan() {
		rows = append(rows, sc.Row())
		if len(rows) > maxTeamRows {
			return nil, sc.Stats(), fmt.Errorf("team feed exceeds %d rows", maxTeamRows)
		}
	}
	return rows, sc.Stats(), sc.Err()
}

func readUsers(s feed.Snapshot) ([]parse.UserRow, parse.Stats, error) {
	rc, err := s.Open()
	if err != nil {
		return nil, parse.Stats{}, err
	}
	defer rc.Close()
	sc := parse.NewUserScanner(rc)
	var rows []parse.UserRow
	for sc.Scan() {
		rows = append(rows, sc.Row())
		if len(rows) > maxUserRows {
			return nil, sc.Stats(), fmt.Errorf("user feed exceeds %d rows", maxUserRows)
		}
	}
	return rows, sc.Stats(), sc.Err()
}

// refreshMonthTotals reloads month-to-date production for the leaderboards.
//
// Like the cadence estimate, this is an enrichment over a working fallback: a failed
// read leaves the previous month totals in place and logs, rather than failing an
// ingest that has already been written. The Monthly tab then lags by a cycle, which
// is a far better outcome than a cycle that did not land.
func (s *Service) refreshMonthTotals(ctx context.Context) {
	if s.state.At.IsZero() {
		return
	}
	teamMonth, err := s.Store.MonthTotals(ctx, "team", s.state.At, len(s.state.Teams))
	if err != nil {
		s.Log.Warn("month totals: teams", "err", err)
		return
	}
	memberMonth, err := s.Store.MonthTotals(ctx, "member", s.state.At, len(s.state.Members))
	if err != nil {
		s.Log.Warn("month totals: members", "err", err)
		return
	}
	s.guard.Lock()
	s.teamMonth, s.memberMonth = teamMonth, memberMonth
	s.guard.Unlock()
}

// observeCadence refreshes the publish-interval estimate from the stored cycles.
func (s *Service) observeCadence(ctx context.Context) {
	times, err := s.Store.RecentCycles(ctx, cadenceSamples+1)
	if err != nil {
		// The estimate is an optimisation over a working fallback, so a failed read
		// is logged and ignored rather than allowed to fail a startup or an ingest.
		s.Log.Warn("cadence: reading recent cycles", "err", err)
		return
	}
	s.cadence.Observe(times)
}

// PollDelay is how long the caller should wait before polling upstream again.
func (s *Service) PollDelay(now time.Time, idle time.Duration) time.Duration {
	s.guard.RLock()
	at := s.state.At
	s.guard.RUnlock()
	return s.cadence.PollDelay(now, at, idle)
}
