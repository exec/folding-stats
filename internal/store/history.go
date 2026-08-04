package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"folding/internal/model"
)

// Granularity selects which table a history query reads from.
type Granularity string

const (
	// Hourly is raw per-publish production, retained only for the recent window.
	//
	// A bucket is one upstream publish rather than a clock hour. Upstream has
	// published hourly for as long as anyone has watched it, so "hourly" is what
	// the API says; if a publish is ever missed, the gap is visible in the data
	// rather than papered over.
	Hourly Granularity = "hourly"
	// Cycle is the original name for Hourly, still accepted on input.
	Cycle Granularity = "cycle"
	// Daily and Monthly read pre-aggregated rollups, which survive after raw
	// deltas are pruned.
	Daily   Granularity = "daily"
	Monthly Granularity = "monthly"
	// Weekly is summed from the daily rollup on read rather than materialised.
	//
	// A week is exactly seven day buckets, so there is nothing a weekly table would
	// hold that daily does not already — it would be a second copy to keep correct
	// through replay and compaction for no new information. Monthly is materialised
	// only because it has to outlive daily once the retention window prunes it.
	Weekly Granularity = "weekly"
)

// Point is one bucket of production.
type Point struct {
	At     time.Time `json:"at"`
	Points int64     `json:"points"`
	WUs    int64     `json:"wus"`
}

const secondsPerDay = 86400

// DayBucket returns the UTC day number for t.
func DayBucket(t time.Time) int64 { return t.UTC().Unix() / secondsPerDay }

// MonthBucket returns year*12+month, a dense monotonic month index.
func MonthBucket(t time.Time) int64 {
	t = t.UTC()
	return int64(t.Year())*12 + int64(t.Month()) - 1
}

// firstSundayDay is the day bucket of 1970-01-04, the first Sunday of the epoch.
//
// Day 0 is a Thursday, so week boundaries are offset by three days rather than
// falling on a multiple of seven. Getting this wrong shifts every weekly bucket by
// half a week, which looks like plausible data rather than like a bug.
const firstSundayDay = 3

// WeekBucket returns a dense index for the Sunday-start UTC week containing t.
func WeekBucket(t time.Time) int64 { return weekOfDay(DayBucket(t)) }

func weekOfDay(day int64) int64 {
	d := day - firstSundayDay
	// Go truncates integer division toward zero, so a plain d/7 would map the days
	// before the epoch's first Sunday onto week 0 alongside the days after it.
	if d < 0 {
		return -((-d + 6) / 7)
	}
	return d / 7
}

func weekBucketTime(b int64) time.Time {
	return time.Unix((b*7+firstSundayDay)*secondsPerDay, 0).UTC()
}

// startOfWeekUTC returns 00:00 UTC on the Sunday of t's week, matching
// metrics.startOfWeek. Derived from the weekday rather than from firstSundayDay so
// the two independent routes to the same boundary have to agree — which
// TestWeekBucketsAlignToSunday checks.
func startOfWeekUTC(t time.Time) time.Time {
	t = t.UTC()
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return d.AddDate(0, 0, -int(d.Weekday())) // time.Sunday == 0
}

// endOfWeekUTC returns the last day of t's week, so DayBucket of it is the inclusive
// upper day bucket for that week.
func endOfWeekUTC(t time.Time) time.Time { return startOfWeekUTC(t).AddDate(0, 0, 6) }

func monthBucketTime(b int64) time.Time {
	return time.Date(int(b/12), time.Month(b%12+1), 1, 0, 0, 0, 0, time.UTC)
}

// MemberHistory returns production for one member over [from, to).
func (s *Store) MemberHistory(ctx context.Context, memberID int32, from, to time.Time, g Granularity) ([]Point, error) {
	return s.history(ctx, "member", int64(memberID), from, to, g)
}

// TeamHistory returns production for one team slot over [from, to).
func (s *Store) TeamHistory(ctx context.Context, slot int32, from, to time.Time, g Granularity) ([]Point, error) {
	return s.history(ctx, "team", int64(slot), from, to, g)
}

// Normalize resolves input aliases to the canonical granularity.
func (g Granularity) Normalize() Granularity {
	if g == Cycle {
		return Hourly
	}
	return g
}

func (s *Store) history(ctx context.Context, kind string, id int64, from, to time.Time, g Granularity) ([]Point, error) {
	g = g.Normalize()
	var query string
	var lo, hi int64

	idCol := "member_id"
	if kind == "team" {
		idCol = "slot"
	}

	switch g {
	case Hourly:
		query = fmt.Sprintf(
			`SELECT ts, d_score, d_wu FROM %s_deltas WHERE %s = ? AND ts >= ? AND ts < ? ORDER BY ts`,
			kind, idCol)
		lo, hi = from.UTC().Unix(), to.UTC().Unix()
	// Bucketed granularities use an inclusive upper bound: a bucket is a period, and
	// it belongs in the result if it overlaps the requested range at all. Excluding
	// the bucket containing `to` would make a request for a single month — or any
	// range narrower than one bucket — return nothing.
	case Daily:
		query = fmt.Sprintf(
			`SELECT bucket, points, wus FROM %s_daily WHERE %s = ? AND bucket >= ? AND bucket <= ? ORDER BY bucket`,
			kind, idCol)
		lo, hi = DayBucket(from), DayBucket(to)
	case Monthly:
		query = fmt.Sprintf(
			`SELECT bucket, points, wus FROM %s_monthly WHERE %s = ? AND bucket >= ? AND bucket <= ? ORDER BY bucket`,
			kind, idCol)
		lo, hi = MonthBucket(from), MonthBucket(to)
	case Weekly:
		// Summed from daily on read. The filter stays on the raw day bucket so the
		// clustered (entity, bucket) key still drives the scan — grouping on the
		// derived week expression alone would force a full per-entity scan.
		query = fmt.Sprintf(
			`SELECT (bucket - %d) / 7 AS wk, SUM(points), SUM(wus)
			   FROM %s_daily
			  WHERE %s = ? AND bucket >= ? AND bucket <= ?
			  GROUP BY wk ORDER BY wk`,
			firstSundayDay, kind, idCol)
		// Widened to whole weeks: a range starting mid-week still belongs to that
		// week's bucket, and reporting a partial sum under a full week's label would
		// be a quietly wrong number rather than a missing one.
		lo, hi = DayBucket(startOfWeekUTC(from)), DayBucket(endOfWeekUTC(to))
	default:
		return nil, fmt.Errorf("store: unknown granularity %q", g)
	}

	rows, err := s.query(ctx, query, id, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Point
	for rows.Next() {
		var bucket, pts, wus int64
		if err := rows.Scan(&bucket, &pts, &wus); err != nil {
			return nil, err
		}
		var at time.Time
		switch g {
		case Hourly:
			at = time.Unix(bucket, 0).UTC()
		case Daily:
			at = time.Unix(bucket*secondsPerDay, 0).UTC()
		case Weekly:
			at = weekBucketTime(bucket)
		case Monthly:
			at = monthBucketTime(bucket)
		}
		out = append(out, Point{At: at, Points: pts, WUs: wus})
	}
	return out, rows.Err()
}

// CompactPolicy bounds a compaction pass.
type CompactPolicy struct {
	// RawBefore: per-cycle deltas older than this are rolled up into daily and
	// monthly buckets, then deleted.
	RawBefore time.Time
	// DailyBefore: daily buckets older than this are deleted once their months are
	// recorded. Zero keeps daily data forever.
	DailyBefore time.Time
}

// RollupResult reports what a compaction pass did.
type RollupResult struct {
	DailyRows   int64
	MonthlyRows int64
	PrunedRaw   int64
	PrunedDaily int64
}

// Compact removes raw deltas and daily buckets that have aged past retention.
//
// It no longer builds rollups: those are maintained on every cycle by rollupCycle,
// because deriving them here meant the daily and monthly views stayed empty until
// data was old enough to compact — 90 days of a site showing "no production
// recorded" for every entity while the raw data sat right there.
//
// The cutoff is floored to a UTC day boundary so a partial day is never pruned out
// from under the bucket that summarises it.
func (s *Store) Compact(ctx context.Context, p CompactPolicy) (RollupResult, error) {
	var res RollupResult

	cutoff := DayBucket(p.RawBefore) * secondsPerDay

	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	for _, e := range []struct{ table, idCol string }{
		{"member", "member_id"},
		{"team", "slot"},
	} {

		// Raw deltas go first. Their daily buckets were written when the cycle was
		// ingested, so nothing is lost by removing them.
		r, err := tx.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM %s_deltas WHERE ts < ?`, e.table), cutoff)
		if err != nil {
			return res, fmt.Errorf("store: pruning %s_deltas: %w", e.table, err)
		}
		n, _ := r.RowsAffected()
		res.PrunedRaw += n

		// Daily buckets go last, and only after their months are recorded — the
		// same ordering hazard as raw deltas, one level up. Months already written
		// keep their values because the recompute above never revisits them.
		if !p.DailyBefore.IsZero() {
			r, err = tx.ExecContext(ctx,
				fmt.Sprintf(`DELETE FROM %s_daily WHERE bucket < ?`, e.table),
				DayBucket(p.DailyBefore))
			if err != nil {
				return res, fmt.Errorf("store: pruning %s_daily: %w", e.table, err)
			}
			n, _ = r.RowsAffected()
			res.PrunedDaily += n
		}
	}

	return res, tx.Commit()
}

// CycleDeltas is one cycle's production, as replayed from storage.
type CycleDeltas struct {
	At      time.Time
	Members []model.Delta
	Teams   []model.Delta

	// NewMembers and NewTeams are how many entities were seen for the first time in
	// this cycle. Replay needs them to reconstruct how large the corpus was at each
	// point, which is what separates an entity that has been watched all week from
	// one that appeared an hour ago.
	NewMembers int32
	NewTeams   int32
}

// DeltasSince returns every stored cycle at or after since, oldest first.
//
// This is what rebuilds the rolling windows after a restart. Without it the service
// comes back with correct cumulative totals but every rate at zero, and would take a
// full week to look right again — which is worse than an obvious failure, because
// nothing errors.
func (s *Store) DeltasSince(ctx context.Context, since time.Time) ([]CycleDeltas, error) {
	byTS := map[int64]*CycleDeltas{}

	load := func(query string, assign func(*CycleDeltas, model.Delta)) error {
		rows, err := s.r.QueryContext(ctx, query, since.UTC().Unix())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int32
			var ts, dScore, dWU int64
			if err := rows.Scan(&id, &ts, &dScore, &dWU); err != nil {
				return err
			}
			c := byTS[ts]
			if c == nil {
				c = &CycleDeltas{At: time.Unix(ts, 0).UTC()}
				byTS[ts] = c
			}
			assign(c, model.Delta{ID: id, DScore: dScore, DWUs: dWU})
		}
		return rows.Err()
	}

	if err := load(
		`SELECT member_id, ts, d_score, d_wu FROM member_deltas WHERE ts >= ? ORDER BY ts`,
		func(c *CycleDeltas, d model.Delta) { c.Members = append(c.Members, d) },
	); err != nil {
		return nil, err
	}
	if err := load(
		`SELECT slot, ts, d_score, d_wu FROM team_deltas WHERE ts >= ? ORDER BY ts`,
		func(c *CycleDeltas, d model.Delta) { c.Teams = append(c.Teams, d) },
	); err != nil {
		return nil, err
	}

	// The audit log carries the first-sighting counts, and it has a row for every
	// cycle — including ones where nothing produced and no delta was written. A
	// cycle that added members but recorded no production still moved the corpus
	// size, so it has to appear in the replay.
	crows, err := s.r.QueryContext(ctx,
		`SELECT ts, new_members, new_teams FROM cycles WHERE ts >= ? ORDER BY ts`,
		since.UTC().Unix())
	if err != nil {
		return nil, err
	}
	defer crows.Close()
	for crows.Next() {
		var ts int64
		var newMembers, newTeams int32
		if err := crows.Scan(&ts, &newMembers, &newTeams); err != nil {
			return nil, err
		}
		c := byTS[ts]
		if c == nil {
			c = &CycleDeltas{At: time.Unix(ts, 0).UTC()}
			byTS[ts] = c
		}
		c.NewMembers, c.NewTeams = newMembers, newTeams
	}
	if err := crows.Err(); err != nil {
		return nil, err
	}

	out := make([]CycleDeltas, 0, len(byTS))
	for _, c := range byTS {
		out = append(out, *c)
	}
	// The windows reject out-of-order cycles outright, so ordering here is required
	// rather than cosmetic.
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// MembersHistory returns the combined production of several members, aggregated by
// bucket in one query.
//
// A donor's series is the sum of their members', and issuing one query per member
// does not scale: the shared placeholder name "PS3" spans 10,426 teams, so the naive
// loop is 10,426 round trips for a single API request. Summing in SQL turns that into
// one.
func (s *Store) MembersHistory(ctx context.Context, ids []int32, from, to time.Time, g Granularity) ([]Point, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	g = g.Normalize()

	var table, bucketCol, groupCol, pointsCol, wusCol string
	var lo, hi int64
	switch g {
	case Hourly:
		table, bucketCol, pointsCol, wusCol = "member_deltas", "ts", "d_score", "d_wu"
		lo, hi = from.UTC().Unix(), to.UTC().Unix()
	case Daily:
		table, bucketCol, pointsCol, wusCol = "member_daily", "bucket", "points", "wus"
		lo, hi = DayBucket(from), DayBucket(to)
	case Weekly:
		// Summed from daily on read, filtered on the raw day bucket so the clustered
		// key still drives the scan — see the matching note in history().
		table, bucketCol, pointsCol, wusCol = "member_daily", "bucket", "points", "wus"
		groupCol = fmt.Sprintf("(bucket - %d) / 7", firstSundayDay)
		lo, hi = DayBucket(startOfWeekUTC(from)), DayBucket(endOfWeekUTC(to))
	case Monthly:
		table, bucketCol, pointsCol, wusCol = "member_monthly", "bucket", "points", "wus"
		lo, hi = MonthBucket(from), MonthBucket(to)
	default:
		return nil, fmt.Errorf("store: unknown granularity %q", g)
	}
	if groupCol == "" {
		groupCol = bucketCol
	}

	// Cycle timestamps are instants so the upper bound is exclusive; bucketed
	// granularities are periods and include the bucket containing `to`.
	upper := "<="
	if g == Hourly {
		upper = "<"
	}
	// Pad the id list to a power of two so the statement cache sees a handful of
	// distinct query texts rather than one per distinct donor width. The padding id
	// is negative and therefore matches nothing.
	n := 1
	for n < len(ids) {
		n *= 2
	}
	placeholders := strings.Repeat("?,", n-1) + "?"
	query := fmt.Sprintf(
		`SELECT %[1]s AS b, SUM(%[2]s), SUM(%[3]s) FROM %[4]s
		  WHERE member_id IN (%[5]s) AND %[7]s >= ? AND %[7]s %[6]s ?
		  GROUP BY b ORDER BY b`,
		groupCol, pointsCol, wusCol, table, placeholders, upper, bucketCol)

	args := make([]any, 0, n+2)
	for _, id := range ids {
		args = append(args, id)
	}
	for i := len(ids); i < n; i++ {
		args = append(args, -1)
	}
	args = append(args, lo, hi)

	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Point
	for rows.Next() {
		var bucket, pts, wus int64
		if err := rows.Scan(&bucket, &pts, &wus); err != nil {
			return nil, err
		}
		out = append(out, Point{At: bucketTime(bucket, g), Points: pts, WUs: wus})
	}
	return out, rows.Err()
}

func bucketTime(b int64, g Granularity) time.Time {
	switch g {
	case Daily:
		return time.Unix(b*secondsPerDay, 0).UTC()
	case Weekly:
		return weekBucketTime(b)
	case Monthly:
		return monthBucketTime(b)
	default:
		return time.Unix(b, 0).UTC()
	}
}

// MonthTotals returns production so far in the UTC calendar month containing at,
// indexed by member id or team slot, sized to n.
//
// This is what the Monthly leaderboard ranks by, and the only period figure that
// cannot come from memory: the rolling windows span seven days, so a month is beyond
// them by design. The rollup table already holds it — rollupCycle refreshes the
// current month's bucket on every cycle — so this is a read of one bucket rather
// than an aggregation.
//
// Only entities that produced this month have a row, which is the point: the result
// is dense for indexing but the scan is proportional to who was active, not to the
// 2.7M members who were not.
func (s *Store) MonthTotals(ctx context.Context, kind string, at time.Time, n int) ([]int64, error) {
	idCol := "member_id"
	if kind == "team" {
		idCol = "slot"
	}
	out := make([]int64, n)
	if n == 0 {
		return out, nil
	}
	rows, err := s.query(ctx, fmt.Sprintf(
		`SELECT %s, points FROM %s_monthly WHERE bucket = ?`, idCol, kind), MonthBucket(at))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var pts int64
		if err := rows.Scan(&id, &pts); err != nil {
			return nil, err
		}
		// A compaction or replay could in principle name an id beyond the corpus the
		// caller sized for; skip rather than panic on a bounds check.
		if id >= 0 && int(id) < n {
			out[id] = pts
		}
	}
	return out, rows.Err()
}

// rollupCycle refreshes the daily and monthly buckets touched by a cycle.
//
// Rollups are maintained as cycles arrive rather than at compaction time. Deriving
// them only during compaction meant they did not exist until data aged past the
// 90-day retention window, so the daily and monthly views were empty for every
// entity on a new deployment — with the raw data sitting right there, unaggregated.
//
// Each affected bucket is recomputed from scratch rather than incremented, so
// re-applying a cycle (which replay does routinely) converges instead of
// double-counting.
func rollupCycle(ctx context.Context, tx *sql.Tx, at time.Time) error {
	day := DayBucket(at)
	dayStart := day * secondsPerDay
	dayEnd := dayStart + secondsPerDay

	month := MonthBucket(at)
	monthStart := time.Date(at.UTC().Year(), at.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	monthFirstDay := DayBucket(monthStart)
	monthLastDay := DayBucket(monthStart.AddDate(0, 1, 0))

	for _, e := range []struct{ table, idCol string }{
		{"member", "member_id"},
		{"team", "slot"},
	} {
		// Only this cycle's day can have changed, so the range scan is bounded by
		// the ts index rather than walking all history.
		daily := fmt.Sprintf(`
            INSERT INTO %[1]s_daily(%[2]s, bucket, points, wus)
            SELECT %[2]s, ?, SUM(d_score), SUM(d_wu)
              FROM %[1]s_deltas
             WHERE ts >= ? AND ts < ?
             GROUP BY %[2]s
            ON CONFLICT(%[2]s, bucket) DO UPDATE SET
                points = excluded.points,
                wus    = excluded.wus`, e.table, e.idCol)
		if _, err := tx.ExecContext(ctx, daily, day, dayStart, dayEnd); err != nil {
			return fmt.Errorf("store: daily rollup for %s: %w", e.table, err)
		}

		// Monthly derives from daily, so it stays correct after raw deltas are
		// pruned — and only the current month is in play.
		monthly := fmt.Sprintf(`
            INSERT INTO %[1]s_monthly(%[2]s, bucket, points, wus)
            SELECT %[2]s, ?, SUM(points), SUM(wus)
              FROM %[1]s_daily
             WHERE bucket >= ? AND bucket < ?
             GROUP BY %[2]s
            ON CONFLICT(%[2]s, bucket) DO UPDATE SET
                points = excluded.points,
                wus    = excluded.wus`, e.table, e.idCol)
		if _, err := tx.ExecContext(ctx, monthly, month, monthFirstDay, monthLastDay); err != nil {
			return fmt.Errorf("store: monthly rollup for %s: %w", e.table, err)
		}
	}

	// The project's own buckets, summed from the team tables in the same
	// transaction. Teams are authoritative for the project rather than members: the
	// team feed exceeds the sum of its member rows for a fraction of teams, because
	// the two feeds publish a minute apart and some production is not attributable
	// to any listed donor.
	//
	// Only the current cycle, day and month are touched, so each of these is a
	// bounded range scan rather than a pass over history.
	for _, q := range []struct {
		what string
		sql  string
		args []any
	}{
		// GROUP BY, not a bare aggregate. A bare SUM over an empty range still returns
		// one row — NULL, or 0 once coalesced — and inserting that writes a bucket
		// claiming the project produced nothing, where the old query returned no
		// bucket at all. Every entity's first cycle is such a range, because a first
		// sighting has no delta, so the difference is a phantom zero at the start of
		// every chart. HAVING does the same job where the group is not the key.
		{"cycle", `
            INSERT INTO project_deltas(ts, d_score, d_wu)
            SELECT ts, SUM(d_score), SUM(d_wu)
              FROM team_deltas WHERE ts = ? GROUP BY ts
            ON CONFLICT(ts) DO UPDATE SET
                d_score = excluded.d_score, d_wu = excluded.d_wu`,
			[]any{at.UTC().Unix()}},
		{"daily", `
            INSERT INTO project_daily(bucket, points, wus)
            SELECT bucket, SUM(points), SUM(wus)
              FROM team_daily WHERE bucket = ? GROUP BY bucket
            ON CONFLICT(bucket) DO UPDATE SET
                points = excluded.points, wus = excluded.wus`,
			[]any{day}},
		// Monthly derives from project_daily, matching the per-entity path, so it
		// stays correct once raw deltas are pruned.
		{"monthly", `
            INSERT INTO project_monthly(bucket, points, wus)
            SELECT ?, SUM(points), SUM(wus)
              FROM project_daily WHERE bucket >= ? AND bucket < ?
            HAVING COUNT(*) > 0
            ON CONFLICT(bucket) DO UPDATE SET
                points = excluded.points, wus = excluded.wus`,
			[]any{month, monthFirstDay, monthLastDay}},
	} {
		if _, err := tx.ExecContext(ctx, q.sql, q.args...); err != nil {
			return fmt.Errorf("store: project %s rollup: %w", q.what, err)
		}
	}
	return nil
}

// ProjectHistory returns production summed across every team.
//
// The overview needs the project's own series, which is not any single team's. The
// obvious stand-in — team 0, the "no team specified" bucket — is a seventh of the
// project and would understate it by that much while claiming to be the whole.
func (s *Store) ProjectHistory(ctx context.Context, from, to time.Time, g Granularity) ([]Point, error) {
	g = g.Normalize()

	// Read from the project rollups, which hold one row per period rather than one
	// per team per period. Only weekly still aggregates, and it groups 1,825 daily
	// rows over the five-year cap rather than the quarter-billion the same range
	// costs across team_daily.
	var table, bucketCol, groupCol, pointsCol, wusCol string
	var lo, hi int64
	upper := "<="
	switch g {
	case Hourly:
		table, bucketCol, pointsCol, wusCol = "project_deltas", "ts", "d_score", "d_wu"
		lo, hi = from.UTC().Unix(), to.UTC().Unix()
		upper = "<"
	case Daily:
		table, bucketCol, pointsCol, wusCol = "project_daily", "bucket", "points", "wus"
		lo, hi = DayBucket(from), DayBucket(to)
	case Weekly:
		// Filtered on the day bucket, grouped by the week it falls in — see the
		// matching note in history().
		table, bucketCol, pointsCol, wusCol = "project_daily", "bucket", "points", "wus"
		groupCol = fmt.Sprintf("(bucket - %d) / 7", firstSundayDay)
		lo, hi = DayBucket(startOfWeekUTC(from)), DayBucket(endOfWeekUTC(to))
	case Monthly:
		table, bucketCol, pointsCol, wusCol = "project_monthly", "bucket", "points", "wus"
		lo, hi = MonthBucket(from), MonthBucket(to)
	default:
		return nil, fmt.Errorf("store: unknown granularity %q", g)
	}
	if groupCol == "" {
		groupCol = bucketCol
	}

	rows, err := s.query(ctx, fmt.Sprintf(
		`SELECT %[1]s AS b, SUM(%[2]s), SUM(%[3]s) FROM %[4]s
		  WHERE %[6]s >= ? AND %[6]s %[5]s ?
		  GROUP BY b ORDER BY b`,
		groupCol, pointsCol, wusCol, table, upper, bucketCol), lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Point
	for rows.Next() {
		var bucket, pts, wus int64
		if err := rows.Scan(&bucket, &pts, &wus); err != nil {
			return nil, err
		}
		out = append(out, Point{At: bucketTime(bucket, g), Points: pts, WUs: wus})
	}
	return out, rows.Err()
}

// projectBackfillKey marks the database as having had its project rollups built from
// the team tables. The rollups are maintained per cycle from then on.
const projectBackfillKey = "project_rollups_backfilled"

// backfillProjectRollups populates the project tables from the team tables, once.
//
// The project rollups were added after the database already held history, so an
// existing file has the team tables fully populated and the project ones empty.
// Without this the endpoint would answer "no production" for everything before the
// upgrade, which is worse than the slow query it replaces: it is confidently wrong.
//
// Guarded by a marker rather than by emptiness. An empty project_deltas is the
// correct state for a database whose team tables are also empty, and re-deriving on
// every open would scan every delta ever recorded at each restart.
//
// The whole thing is one transaction: a partial backfill marked as complete would
// leave a permanent hole no later cycle ever revisits.
func backfillProjectRollups(w *sql.DB) error {
	var done string
	err := w.QueryRow(`SELECT value FROM meta WHERE key = ?`, projectBackfillKey).Scan(&done)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: checking project backfill: %w", err)
	}

	tx, err := w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, q := range []struct{ what, sql string }{
		{"deltas", `INSERT INTO project_deltas(ts, d_score, d_wu)
                    SELECT ts, SUM(d_score), SUM(d_wu) FROM team_deltas GROUP BY ts
                    ON CONFLICT(ts) DO UPDATE SET
                        d_score = excluded.d_score, d_wu = excluded.d_wu`},
		{"daily", `INSERT INTO project_daily(bucket, points, wus)
                   SELECT bucket, SUM(points), SUM(wus) FROM team_daily GROUP BY bucket
                   ON CONFLICT(bucket) DO UPDATE SET
                       points = excluded.points, wus = excluded.wus`},
		// From team_monthly rather than from the daily rollup just written: months
		// whose days have already been pruned exist only in the monthly table, and
		// deriving from daily would silently drop them.
		{"monthly", `INSERT INTO project_monthly(bucket, points, wus)
                     SELECT bucket, SUM(points), SUM(wus) FROM team_monthly GROUP BY bucket
                     ON CONFLICT(bucket) DO UPDATE SET
                         points = excluded.points, wus = excluded.wus`},
	} {
		if _, err := tx.Exec(q.sql); err != nil {
			return fmt.Errorf("store: backfilling project %s: %w", q.what, err)
		}
	}

	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO meta(key, value) VALUES(?, ?)`,
		projectBackfillKey, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("store: marking project backfill: %w", err)
	}
	return tx.Commit()
}
