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
	// cadence learns the upstream publish interval from observation. It drives both
	// the poll schedule and the next_expected_at the API advertises.
	cadence *cadence

	// guard serialises mutation of state and the windows against API reads, which
	// serve directly from those structures rather than from a copy.
	guard sync.RWMutex
}

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
	s.memberWin.Grow(len(s.state.Members))
	s.teamWin.Grow(len(s.state.Teams))
	for _, c := range cycles {
		s.memberWin.Push(c.At, c.Members)
		s.teamWin.Push(c.At, c.Teams)
	}
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
			continue
		}
		s.applied[p.at.Unix()] = true
		n++
	}
	if n > 0 {
		// Each new cycle is another interval observed, so the prediction sharpens as
		// the drift moves rather than being fixed at startup.
		s.observeCadence(ctx)
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

	s.guard.Lock()
	cycle := s.state.Apply(p.at, teamRows, userRows)
	s.guard.Unlock()

	if err := s.Store.WriteCycle(ctx, s.state, cycle, store.CycleMeta{
		TeamSnapshotAt: p.team.Meta.SnapshotAt,
		UserSnapshotAt: p.user.Meta.SnapshotAt,
		TeamRows:       teamStats.Rows,
		UserRows:       userStats.Rows,
		Malformed:      teamStats.Malformed + userStats.Malformed,
		Duration:       time.Since(start),
	}); err != nil {
		return err
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
	return nil
}

// publish rebuilds the ranked view and swaps it in.
func (s *Service) publish() {
	start := time.Now()

	// Read lock: building the ranked view only reads state, but it must not run
	// while a cycle is mutating it. Refresh calls this from a different goroutine
	// than Ingest does.
	s.guard.RLock()
	defer s.guard.RUnlock()

	tbl := rank.Build(s.state, s.state.At, rank.DefaultConfig)

	next := s.cadence.NextAfter(s.state.At)
	// The ETag identifies the snapshot; the cycle time is exactly that identity.
	etag := fmt.Sprintf("%d", s.state.At.Unix())

	snap := api.Build(s.state, s.memberWin, s.teamWin, tbl, s.Store,
		s.state.At, next, etag)
	snap.Guard = &s.guard
	snap.StaleAfter = next.Add(staleGrace)
	snap.Interval = s.cadence.Interval()
	snap.IntervalMeasured = s.cadence.Measured()

	s.Server.Publish(snap)
	s.Log.Info("published",
		"at", s.state.At.Format(time.RFC3339),
		"teams", len(s.state.Teams), "donors", len(tbl.Donors),
		"stale_after", snap.StaleAfter.Format(time.RFC3339),
		"took", time.Since(start).Round(time.Millisecond))
}

// Refresh republishes without ingesting, so the stale flag reflects the passage of
// time even when upstream has gone quiet.
func (s *Service) Refresh() {
	if s.Server.Current() != nil {
		s.publish()
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
	}
	return rows, sc.Stats(), sc.Err()
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
