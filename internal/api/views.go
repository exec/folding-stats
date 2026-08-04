package api

import (
	"sort"
	"time"

	"folding/internal/metrics"
	"folding/internal/model"
)

// View builders translate internal state into the wire types. They are the only
// place that knows both representations, which keeps the naming decisions in R7
// enforced in exactly one location.

// rollup reads a per-slot total defensively: the month figures are sized when a cycle
// is ingested, so a view built between a corpus growing and the next refresh would
// otherwise index past the end.
func rollup(s []int64, slot int32) int64 {
	if slot < 0 || int(slot) >= len(s) {
		return 0
	}
	return s[slot]
}

// change turns the table's (value, known) pair into a field that marshals to null-by-
// omission when there is nothing to report.
func change(v int32, ok bool) *int32 {
	if !ok {
		return nil
	}
	return &v
}

func (s *Snapshot) teamView(slot int32) Team {
	t := s.State.Teams[slot]
	total, active := s.TeamMemberCounts(t.ID)
	return Team{
		TeamID:        t.ID,
		Name:          s.State.Names.Name(t.NameID),
		Rank:          s.Ranks.TeamRankOf(slot),
		RankChange24h: change(s.Ranks.TeamChange24h(slot)),
		MembersTotal:  total,
		MembersActive: active,
		Production: Production{
			PointsTotal:        t.Score,
			WUsTotal:           t.WUs,
			PointsLastUpdate:   s.Teams.LastUpdate(slot),
			PointsLast24h:      s.Teams.Last24h(slot),
			PointsLast7d:       s.Teams.Last7d(slot),
			PointsTodayUTC:     s.Teams.Today(slot),
			PointsThisWeekUTC:  s.Teams.ThisWeek(slot),
			PointsThisMonthUTC: rollup(s.TeamMonth, slot),
			PointsPerDay7dAvg:  s.Teams.PointsPerDay(slot),
		},
	}
}

func (s *Snapshot) memberView(slot int32, withTeamName bool) Member {
	m := s.State.Members[slot]
	out := Member{
		Name:          s.State.Names.Name(m.NameID),
		TeamID:        m.TeamID,
		RankGlobal:    s.Ranks.MemberRankOf(slot),
		RankInTeam:    s.Ranks.InTeamRankOf(slot),
		RankChange24h: change(s.Ranks.MemberChange24h(slot)),
		Production: Production{
			PointsTotal:        m.Score,
			WUsTotal:           m.WUs,
			PointsLastUpdate:   s.Members.LastUpdate(slot),
			PointsLast24h:      s.Members.Last24h(slot),
			PointsLast7d:       s.Members.Last7d(slot),
			PointsTodayUTC:     s.Members.Today(slot),
			PointsThisWeekUTC:  s.Members.ThisWeek(slot),
			PointsThisMonthUTC: rollup(s.MemberMonth, slot),
			PointsPerDay7dAvg:  s.Members.PointsPerDay(slot),
		},
	}
	// On a donor's breakdown the team name is what identifies each row, but on a
	// team's own member list it would repeat on every row for no benefit.
	if withTeamName {
		if ts, ok := s.State.TeamSlot(m.TeamID); ok {
			out.TeamName = s.State.Names.Name(s.State.Teams[ts].NameID)
		}
	}
	return out
}

// donorView aggregates a name across its teams. Rates are summed on demand because a
// donor is a read-time view over member state (R1), never stored identity.
func (s *Snapshot) donorView(idx int32, withTeams bool) Donor {
	d := s.Ranks.Donors[idx]
	members := s.Ranks.DonorMembers(idx)

	out := Donor{
		Name:             s.State.Names.Name(d.NameID),
		Rank:             idx + 1,
		RankChange24h:    change(s.Ranks.DonorChange24h(idx)),
		TeamCount:        d.TeamCount,
		LikelyNotAPerson: d.LikelyNotAPerson,
		Production: Production{
			PointsTotal: d.Score,
			WUsTotal:    d.WUs,
		},
	}
	// A donor has been observed since the first of their members was, so the longest
	// member span is the donor's. Taking the shortest would restart the clock every
	// time an existing donor joined another team, collapsing a long-standing donor's
	// average onto whatever their newest membership has produced.
	var observed time.Duration
	for _, slot := range members {
		out.PointsLastUpdate += s.Members.LastUpdate(slot)
		out.PointsLast24h += s.Members.Last24h(slot)
		out.PointsLast7d += s.Members.Last7d(slot)
		out.PointsTodayUTC += s.Members.Today(slot)
		out.PointsThisWeekUTC += s.Members.ThisWeek(slot)
		if span := s.Members.ObservedSpan(slot); span > observed {
			observed = span
		}
	}
	// Already summed across this donor's members when the periods were built.
	out.PointsThisMonthUTC = s.Ranks.DonorMonth(idx)
	// Averaging the summed week, not summing per-member averages: rounding each
	// member separately then adding would drift by up to half a point per team.
	out.PointsPerDay7dAvg = metrics.PerDay(out.PointsLast7d, observed)

	if withTeams {
		out.Teams, out.TeamsTruncated = s.breakdown(members, maxEmbeddedTeams)
	}
	return out
}

// maxEmbeddedTeams caps the breakdown carried inline on a donor. Real people fold
// for a handful of teams; only shared placeholder names run to thousands, and those
// would otherwise produce multi-megabyte responses.
const maxEmbeddedTeams = 100

// breakdown returns a donor's per-team rows ordered by lifetime points, capped at
// limit. A limit of 0 means no cap.
func (s *Snapshot) breakdown(members []int32, limit int) ([]Member, bool) {
	return s.breakdownSorted(members, limit, false)
}

// breakdownSorted orders by recent production when byProduction is set, and by
// lifetime points otherwise.
//
// The distinction matters more than it looks: a donor's largest teams by lifetime
// total are routinely dormant, so ordering a production view by lifetime points
// selects exactly the teams with nothing to plot.
func (s *Snapshot) breakdownSorted(members []int32, limit int, byProduction bool) ([]Member, bool) {
	ordered := append([]int32(nil), members...)
	if byProduction {
		sort.Slice(ordered, func(a, b int) bool {
			return s.Members.Last7d(ordered[a]) > s.Members.Last7d(ordered[b])
		})
	} else {
		// With a cap, the rows dropped should be the ones that matter least.
		sortSlotsByScoreDesc(ordered, s.State.Members)
	}

	truncated := false
	if limit > 0 && len(ordered) > limit {
		ordered, truncated = ordered[:limit], true
	}
	out := make([]Member, 0, len(ordered))
	for _, slot := range ordered {
		out = append(out, s.memberView(slot, true))
	}
	return out, truncated
}

// memberSlot resolves a (name, team) pair back to its slot.
func (s *Snapshot) memberSlot(name string, teamID int32) (int32, bool) {
	nameID, ok := s.State.Names.Lookup(name)
	if !ok {
		return 0, false
	}
	return s.State.MemberSlot(nameID, teamID)
}

// donorIndexByName resolves a donor by exact name.
func (s *Snapshot) donorIndexByName(name string) (int32, bool) {
	nameID, ok := s.State.Names.Lookup(name)
	if !ok {
		return 0, false
	}
	idx := s.Ranks.DonorIndexOf(nameID)
	if idx < 0 {
		return 0, false
	}
	return idx, true
}

// sortSlotsByScoreDesc orders member slots by cumulative score, highest first.
//
// The cap is applied after this sort, not before, so the widest donors are sorted in
// full: "PS3" spans 10,426 teams. An insertion sort here is O(n²) over random offsets
// into a 160 MB array — measured at a 318 ms tail on the donor history endpoint while
// every other endpoint stayed under a millisecond.
func sortSlotsByScoreDesc(slots []int32, members []model.Member) {
	sort.Slice(slots, func(a, b int) bool {
		return members[slots[a]].Score > members[slots[b]].Score
	})
}
