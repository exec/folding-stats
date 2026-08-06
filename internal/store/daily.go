package store

// One entity's production, day by day.
//
// The daily rollup already stores exactly one row per day an entity produced on, and
// (member_id, bucket) and (slot, bucket) are the primary keys of WITHOUT ROWID tables —
// so this is one seek onto the entity followed by a sequential read of its own rows,
// with points and work units already in the row the key lands on. Reading them costs
// nothing beyond reading the bucket alone.
//
// The whole retained range is read rather than a trailing window, because the longest
// streak is as interesting as the current one and cannot be found from the tail. That
// is bounded by daily retention — two years by default, so at most ~730 rows for an
// entity that has never missed a day.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Day is one UTC day on which an entity produced.
type Day struct {
	// Bucket is the UTC day number, unix/86400.
	Bucket int64
	Points int64
	WUs    int64
}

// TeamDays returns the days a team produced on, oldest first.
func (s *Store) TeamDays(ctx context.Context, slot int32) ([]Day, error) {
	return s.days(ctx,
		`SELECT bucket, points, wus FROM team_daily WHERE slot = ? AND points > 0 ORDER BY bucket`, slot)
}

// MemberDays returns the days on which any of the given members produced, with their
// production summed across memberships.
//
// Grouped rather than listed: these are one donor's memberships, so folding for two
// teams on the same day is one day of folding. Counting it twice would report streaks
// longer than the calendar.
func (s *Store) MemberDays(ctx context.Context, ids []int32) ([]Day, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// Padded to a power of two for the same reason as MembersHistory: it keeps the
	// statement cache to a handful of query texts instead of one per donor width. The
	// padding id is negative and matches nothing.
	n := 1
	for n < len(ids) {
		n *= 2
	}
	args := make([]any, 0, n)
	for _, id := range ids {
		args = append(args, id)
	}
	for i := len(ids); i < n; i++ {
		args = append(args, -1)
	}
	return s.days(ctx, fmt.Sprintf(
		`SELECT bucket, SUM(points), SUM(wus) FROM member_daily WHERE member_id IN (%s) AND points > 0
		  GROUP BY bucket ORDER BY bucket`,
		strings.Repeat("?,", n-1)+"?"), args...)
}

func (s *Store) days(ctx context.Context, query string, args ...any) ([]Day, error) {
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Day
	for rows.Next() {
		var d Day
		if err := rows.Scan(&d.Bucket, &d.Points, &d.WUs); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ChangedTeams returns the slots of teams that produced after since, exclusive.
func (s *Store) ChangedTeams(ctx context.Context, since time.Time) ([]int32, error) {
	return s.changed(ctx, `SELECT DISTINCT slot FROM team_deltas WHERE ts > ?`, since)
}

// ChangedMembers returns the slots of members that produced after since, exclusive.
//
// Exclusive because the natural cursor is the snapshot time of the last response a
// client held, and an inclusive bound would hand back that whole cycle every poll.
//
// Both of these ride the covering (ts, d_score, d_wu) index, whose entries carry the
// primary key along — so the distinct set of ids in a time range never touches the
// table. That matters more here than anywhere: this is the query that exists to keep a
// mirror off the full collections, and it would be a poor trade if it cost more than
// the crawl it replaces.
func (s *Store) ChangedMembers(ctx context.Context, since time.Time) ([]int32, error) {
	return s.changed(ctx, `SELECT DISTINCT member_id FROM member_deltas WHERE ts > ?`, since)
}

func (s *Store) changed(ctx context.Context, query string, since time.Time) ([]int32, error) {
	rows, err := s.query(ctx, query, since.UTC().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []int32
	for rows.Next() {
		var id int32
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// FirstCycle is when collection started: the earliest snapshot ever applied.
//
// It bounds every streak. An entity that has produced every day we have watched has a
// streak equal to the age of this service, which says nothing about the entity — so the
// figure has to be reported as the lower bound it is, and that needs to know where the
// record begins.
//
// Zero time when nothing has been ingested.
func (s *Store) FirstCycle(ctx context.Context) (time.Time, error) {
	rows, err := s.query(ctx, `SELECT MIN(ts) FROM cycles`)
	if err != nil {
		return time.Time{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return time.Time{}, rows.Err()
	}
	// MIN over an empty table is one row of NULL, not no rows.
	var ts *int64
	if err := rows.Scan(&ts); err != nil {
		return time.Time{}, err
	}
	if ts == nil {
		return time.Time{}, rows.Err()
	}
	return time.Unix(*ts, 0).UTC(), rows.Err()
}
