package api

import (
	"sort"

	"folding/internal/model"
)

// View builders translate internal state into the wire types. They are the only
// place that knows both representations, which keeps the naming decisions in R7
// enforced in exactly one location.

// roundDiv7 divides by 7 rounding to nearest, matching the published figures on EOC
// (truncation reproduces none of their three captured values, rounding reproduces
// all three).
func roundDiv7(v int64) int64 {
	if v < 0 {
		return (v - 3) / 7
	}
	return (v + 3) / 7
}

func (s *Snapshot) teamView(slot int32) Team {
	t := s.State.Teams[slot]
	total, active := s.TeamMemberCounts(t.ID)
	return Team{
		TeamID:        t.ID,
		Name:          s.State.Names.Name(t.NameID),
		Rank:          s.Ranks.TeamRank[slot],
		MembersTotal:  total,
		MembersActive: active,
		Production: Production{
			PointsTotal:       t.Score,
			WUsTotal:          t.WUs,
			PointsLastUpdate:  s.Teams.LastUpdate(slot),
			PointsLast24h:     s.Teams.Last24h(slot),
			PointsLast7d:      s.Teams.Last7d(slot),
			PointsTodayUTC:    s.Teams.Today(slot),
			PointsThisWeekUTC: s.Teams.ThisWeek(slot),
			PointsPerDay7dAvg: s.Teams.PointsPerDay(slot),
		},
	}
}

func (s *Snapshot) memberView(slot int32, withTeamName bool) Member {
	m := s.State.Members[slot]
	out := Member{
		Name:       s.State.Names.Name(m.NameID),
		TeamID:     m.TeamID,
		RankGlobal: s.Ranks.MemberRank[slot],
		RankInTeam: s.Ranks.InTeamRank[slot],
		Production: Production{
			PointsTotal:       m.Score,
			WUsTotal:          m.WUs,
			PointsLastUpdate:  s.Members.LastUpdate(slot),
			PointsLast24h:     s.Members.Last24h(slot),
			PointsLast7d:      s.Members.Last7d(slot),
			PointsTodayUTC:    s.Members.Today(slot),
			PointsThisWeekUTC: s.Members.ThisWeek(slot),
			PointsPerDay7dAvg: s.Members.PointsPerDay(slot),
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
		TeamCount:        d.TeamCount,
		LikelyNotAPerson: d.LikelyNotAPerson,
		Production: Production{
			PointsTotal: d.Score,
			WUsTotal:    d.WUs,
		},
	}
	for _, slot := range members {
		out.PointsLastUpdate += s.Members.LastUpdate(slot)
		out.PointsLast24h += s.Members.Last24h(slot)
		out.PointsLast7d += s.Members.Last7d(slot)
		out.PointsTodayUTC += s.Members.Today(slot)
		out.PointsThisWeekUTC += s.Members.ThisWeek(slot)
	}
	// Averaging the summed week, not summing per-member averages: rounding each
	// member separately then adding would drift by up to half a point per team.
	out.PointsPerDay7dAvg = roundDiv7(out.PointsLast7d)

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
	idx := s.Ranks.DonorIndex[nameID]
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
