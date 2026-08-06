package api

// What one entity's days look like: the streak, and the recent work-unit ratio.
//
// Both come out of the same read. The daily rollup stores one row per day an entity
// produced on, carrying the bucket, the points and the work units — so the streak is a
// run-length scan over the buckets and the ratio is a sum over the other two columns,
// and neither costs a second query.
//
// They answer different halves of a question the rest of the page cannot. Every other
// figure is a magnitude; the streak is about persistence, which is what distributed
// computing actually runs on, and the ratio is the only signal here about what kind of
// machine is doing the work.

import (
	"context"
	"math"
	"time"

	"folding/internal/store"
)

// streakOf reads runs out of the days an entity produced on, which must be sorted
// ascending and free of duplicates — as both queries that feed it guarantee.
//
// today is the day the snapshot belongs to, not the wall clock: everything else in a
// response describes the snapshot, and a streak that quietly used a different clock
// would break at midnight for readers and not for the data.
func streakOf(days []int64, today int64) Streak {
	out := Streak{ActiveDays: len(days)}
	if len(days) == 0 {
		return out
	}

	run, start := 1, days[0]
	out.Longest = 1
	for i := 1; i < len(days); i++ {
		if days[i] == days[i-1]+1 {
			run++
		} else {
			run, start = 1, days[i]
		}
		if run > out.Longest {
			out.Longest = run
		}
	}

	// The final run is the current one only if it reaches today — or yesterday, since
	// today is still in progress and an unfinished day cannot have been missed.
	if last := days[len(days)-1]; last == today || last == today-1 {
		at := dayTime(start)
		out.Current, out.Since = run, &at
	}
	return out
}

// dayTime turns a UTC day bucket back into the instant it starts at.
func dayTime(bucket int64) time.Time {
	return time.Unix(bucket*86400, 0).UTC()
}

// recentWindowDays is how far back the current-hardware ratio looks.
//
// A month is long enough that a couple of idle days do not swing it, and short enough
// that it still describes the machine running today rather than the one before it.
const recentWindowDays = 30

// recentOf sums a trailing window out of the same rows the streak was read from.
//
// The window is stated rather than assumed, because it is bounded by the record: this
// service began collecting on 2 August 2026, so for its first month "the last thirty
// days" and "everything we have" are the same range, and a reader deserves to know
// which one they are looking at.
func recentOf(days []store.Day, today, floor int64) *Recent {
	from := today - recentWindowDays + 1
	if floor > from {
		from = floor
	}
	var pts, wus int64
	for _, d := range days {
		if d.Bucket >= from {
			pts += d.Points
			wus += d.WUs
		}
	}
	if wus <= 0 {
		// No work units in the window means no ratio. Falling back to the lifetime
		// figure would silently change what the field means.
		return nil
	}
	return &Recent{
		Days:        int(today-from) + 1,
		Points:      pts,
		WUs:         wus,
		PointsPerWU: perWU(pts, wus),
	}
}

// streak assembles the wire figure, marking a run that reaches the start of the record.
//
// The floor matters more than it looks. This service began collecting on 2 August 2026;
// a donor who has folded every day since the nineties reports a streak measured from
// then, and a reader with no way to tell that apart from a genuinely new habit is being
// misinformed by a number that is arithmetically correct.
func (s *Snapshot) streak(days []store.Day) *Streak {
	today := store.DayBucket(s.At)
	buckets := make([]int64, len(days))
	for i, d := range days {
		buckets[i] = d.Bucket
	}
	out := streakOf(buckets, today)
	if out.Since != nil && !s.CollectionStart.IsZero() {
		out.AtCollectionFloor = store.DayBucket(*out.Since) <= store.DayBucket(s.CollectionStart)
	}
	return &out
}

// daily reads one entity's per-day production, and turns it into both figures derived
// from it. One query serves both: the rows the streak needs already carry the points
// and work units the recent ratio needs, in the same row the clustered key lands on.
//
// Failure is not an error to the caller. These are ornaments on a page whose real
// content is already assembled, and refusing to serve a team because an ornament could
// not be read would turn a slow query into an outage.
func (s *Snapshot) daily(days []store.Day) (*Streak, *Recent) {
	today := store.DayBucket(s.At)
	floor := int64(math.MinInt64)
	if !s.CollectionStart.IsZero() {
		floor = store.DayBucket(s.CollectionStart)
	}
	return s.streak(days), recentOf(days, today, floor)
}

func (s *Snapshot) teamDaily(ctx context.Context, slot int32) (*Streak, *Recent) {
	if s.Store == nil {
		return nil, nil
	}
	days, err := s.Store.TeamDays(ctx, slot)
	if err != nil {
		return nil, nil
	}
	return s.daily(days)
}

func (s *Snapshot) donorDaily(ctx context.Context, members []int32) (*Streak, *Recent) {
	if s.Store == nil {
		return nil, nil
	}
	// Capped like the donor's history is, and for the same reason: a shared
	// placeholder name spans thousands of teams, and none of them change the answer to
	// "did this donor fold today" that the largest hundred have not already settled.
	if len(members) > maxHistoryTeams {
		members = members[:maxHistoryTeams]
	}
	days, err := s.Store.MemberDays(ctx, members)
	if err != nil {
		return nil, nil
	}
	return s.daily(days)
}

// teamDetailView is teamView plus the figures only a team's own page needs.
//
// Separate from teamView rather than a flag on it, because teamView also builds every
// row of every listing and of the rivals neighbourhood — and these cost a search over
// the month ordering and a database read, both worth paying once for the subject and
// never fifty times for a page.
func (s *Snapshot) teamDetailView(ctx context.Context, slot int32) Team {
	t := s.teamView(slot)
	t.Standing = s.teamStandings(slot, t.Rank)
	t.Streak, t.Recent = s.teamDaily(ctx, slot)
	return t
}

// donorDetailView is donorView with the per-team breakdown, the standings, the streak
// and the recent window — the donor's own page rather than a row in a listing.
func (s *Snapshot) donorDetailView(ctx context.Context, idx int32) Donor {
	d := s.donorView(idx, true)
	d.Streak, d.Recent = s.donorDaily(ctx, s.Ranks.DonorMembers(idx))
	return d
}
