package api

// Consecutive days of production.
//
// Everything else on an entity's page is a magnitude — how many points, how fast, how
// far up the table. A streak is the only figure about persistence, which is the thing
// distributed computing actually runs on: a modest machine left on every day beats a
// fast one switched on twice a month, and nothing else here says so.
//
// The days come from the daily rollup, which stores one row per day an entity produced
// on. So the whole question is a run-length scan over a short list of integers, and the
// only work is deciding what counts as unbroken.

import (
	"context"
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

// streak assembles the wire figure, marking a run that reaches the start of the record.
//
// The floor matters more than it looks. This service began collecting on 3 August 2026;
// a donor who has folded every day since the nineties reports a streak measured from
// then, and a reader with no way to tell that apart from a genuinely new habit is being
// misinformed by a number that is arithmetically correct.
func (s *Snapshot) streak(days []int64) *Streak {
	today := store.DayBucket(s.At)
	out := streakOf(days, today)
	if out.Since != nil && !s.CollectionStart.IsZero() {
		out.AtCollectionFloor = store.DayBucket(*out.Since) <= store.DayBucket(s.CollectionStart)
	}
	return &out
}

// teamStreak and donorStreak are the two entry points, each a single clustered read.
//
// Failure is not an error to the caller. A streak is an ornament on a page whose real
// content is already assembled, and refusing to serve a team because an ornament could
// not be read would turn a slow query into an outage.
func (s *Snapshot) teamStreak(ctx context.Context, slot int32) *Streak {
	if s.Store == nil {
		return nil
	}
	days, err := s.Store.TeamActiveDays(ctx, slot)
	if err != nil {
		return nil
	}
	return s.streak(days)
}

func (s *Snapshot) donorStreak(ctx context.Context, members []int32) *Streak {
	if s.Store == nil {
		return nil
	}
	// Capped like the donor's history is, and for the same reason: a shared
	// placeholder name spans thousands of teams, and none of them change the answer to
	// "did this donor fold today" that the largest hundred have not already settled.
	if len(members) > maxHistoryTeams {
		members = members[:maxHistoryTeams]
	}
	days, err := s.Store.MemberActiveDays(ctx, members)
	if err != nil {
		return nil
	}
	return s.streak(days)
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
	t.Streak = s.teamStreak(ctx, slot)
	return t
}

// donorDetailView is donorView with the per-team breakdown, the standings and the
// streak — the donor's own page rather than a row in a listing.
func (s *Snapshot) donorDetailView(ctx context.Context, idx int32) Donor {
	d := s.donorView(idx, true)
	d.Streak = s.donorStreak(ctx, s.Ranks.DonorMembers(idx))
	return d
}
