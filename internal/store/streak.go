package store

// Which days an entity produced on.
//
// A streak is a question about whether anything happened on a day, not about how much,
// so these read the bucket column alone. The daily rollup already stores exactly one
// row per day an entity produced on, which makes "which days" a scan of the clustered
// key rather than an aggregation: (member_id, bucket) and (slot, bucket) are the
// primary keys of WITHOUT ROWID tables, so one seek lands on the entity and the rest is
// a sequential read of its own rows.
//
// The whole retained range is read rather than a trailing window, because the longest
// streak is as interesting as the current one and cannot be found from the tail. That
// is bounded by daily retention — two years by default, so at most ~730 integers for
// an entity that has never missed a day.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// TeamActiveDays returns the UTC day buckets a team produced on, oldest first.
func (s *Store) TeamActiveDays(ctx context.Context, slot int32) ([]int64, error) {
	return s.activeDays(ctx,
		`SELECT bucket FROM team_daily WHERE slot = ? AND points > 0 ORDER BY bucket`, slot)
}

// MemberActiveDays returns the day buckets on which any of the given members produced.
//
// DISTINCT because these are one donor's memberships: folding for two teams on the same
// day is one day of folding, and counting it twice would report streaks longer than the
// calendar.
func (s *Store) MemberActiveDays(ctx context.Context, ids []int32) ([]int64, error) {
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
	return s.activeDays(ctx, fmt.Sprintf(
		`SELECT DISTINCT bucket FROM member_daily WHERE member_id IN (%s) AND points > 0 ORDER BY bucket`,
		strings.Repeat("?,", n-1)+"?"), args...)
}

func (s *Store) activeDays(ctx context.Context, query string, args ...any) ([]int64, error) {
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var b int64
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
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
