package api

import (
	"math"
	"sort"
	"time"

	"folding/internal/metrics"
	"folding/internal/model"
	"folding/internal/rank"
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
		Tie:           s.teamTie(s.Ranks.TeamRankOf(slot)),
		RankChange24h: change(s.Ranks.TeamChange24h(slot)),
		MembersTotal:  total,
		MembersActive: active,
		Production: Production{
			PointsTotal:        t.Score,
			WUsTotal:           t.WUs,
			PointsLastCycle:    s.Teams.LastUpdate(slot),
			PointsLast24h:      s.Teams.Last24h(slot),
			PointsLast7d:       s.Teams.Last7d(slot),
			PointsTodayUTC:     s.Teams.Today(slot),
			PointsThisWeekUTC:  s.Teams.ThisWeek(slot),
			PointsThisMonthUTC: rollup(s.TeamMonth, slot),
			PointsPerDay7dAvg:  s.Teams.PointsPerDay(slot),
			PointsPerDay24hAvg: s.Teams.PointsPerDay24h(slot),
			PointsPerWU:        perWU(t.Score, t.WUs),
		},
	}
}

func (s *Snapshot) memberView(slot int32, withTeamName bool) Member {
	m := s.State.Members[slot]
	out := Member{
		Name:          s.State.Names.Name(m.NameID),
		TeamID:        m.TeamID,
		RankGlobal:    s.Ranks.MemberRankOf(slot),
		TieGlobal:     s.memberTie(s.Ranks.MemberRankOf(slot)),
		RankInTeam:    s.Ranks.InTeamRankOf(slot),
		RankChange24h: change(s.Ranks.MemberChange24h(slot)),
		Production: Production{
			PointsTotal:        m.Score,
			WUsTotal:           m.WUs,
			PointsLastCycle:    s.Members.LastUpdate(slot),
			PointsLast24h:      s.Members.Last24h(slot),
			PointsLast7d:       s.Members.Last7d(slot),
			PointsTodayUTC:     s.Members.Today(slot),
			PointsThisWeekUTC:  s.Members.ThisWeek(slot),
			PointsThisMonthUTC: rollup(s.MemberMonth, slot),
			PointsPerDay7dAvg:  s.Members.PointsPerDay(slot),
			PointsPerDay24hAvg: s.Members.PointsPerDay24h(slot),
			PointsPerWU:        perWU(m.Score, m.WUs),
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
//
// detail selects the donor's own page: the per-team breakdown and the standings, both
// of which cost more than a listing row should and neither of which a listing shows.
func (s *Snapshot) donorView(idx int32, detail bool) Donor {
	d := s.Ranks.Donors[idx]
	members := s.Ranks.DonorMembers(idx)

	out := Donor{
		Name:             s.State.Names.Name(d.NameID),
		Rank:             idx + 1,
		Tie:              s.donorTie(idx + 1),
		RankChange24h:    change(s.Ranks.DonorChange24h(idx)),
		TeamCount:        d.TeamCount,
		LikelyNotAPerson: d.LikelyNotAPerson,
		Production: Production{
			PointsTotal: d.Score,
			WUsTotal:    d.WUs,
			PointsPerWU: perWU(d.Score, d.WUs),
		},
	}
	// Summed across this donor's members when the periods were built, because the
	// donors with the most members are precisely the ones a leaderboard sorted by
	// team count or work units puts on its first page — so the per-response walk was
	// slowest exactly where it ran most.
	if p, ok := s.Ranks.DonorTotals(idx); ok {
		out.PointsLastCycle = p.LastUpdate
		out.PointsLast24h = p.Last24h
		out.PointsLast7d = p.Last7d
		out.PointsTodayUTC = p.Today
		out.PointsThisWeekUTC = p.ThisWeek
		out.PointsThisMonthUTC = p.ThisMonth
		out.PointsPerDay7dAvg = p.PerDay
		out.PointsPerDay24hAvg = p.PerDay24h
	} else {
		// No prebuilt totals (a Table assembled without BuildOrders): sum them here.
		// A donor has been observed since the first of their members was, so the
		// longest member span is the donor's. Taking the shortest would restart the
		// clock every time an existing donor joined another team, collapsing a
		// long-standing donor's average onto whatever their newest membership has
		// produced.
		var observed time.Duration
		for _, slot := range members {
			out.PointsLastCycle += s.Members.LastUpdate(slot)
			out.PointsLast24h += s.Members.Last24h(slot)
			out.PointsLast7d += s.Members.Last7d(slot)
			out.PointsTodayUTC += s.Members.Today(slot)
			out.PointsThisWeekUTC += s.Members.ThisWeek(slot)
			if span := s.Members.ObservedSpan(slot); span > observed {
				observed = span
			}
		}
		out.PointsThisMonthUTC = s.Ranks.DonorMonth(idx)
		// Averaging the summed week, not summing per-member averages: rounding each
		// member separately then adding would drift by up to half a point per team.
		out.PointsPerDay7dAvg = metrics.PerDay(out.PointsLast7d, observed)
		day24 := observed
		if day24 > 24*time.Hour {
			day24 = 24 * time.Hour
		}
		out.PointsPerDay24hAvg = metrics.PerDay(out.PointsLast24h, day24)
	}

	if detail {
		out.Teams, out.TeamsTruncated = s.breakdown(members, maxEmbeddedTeams)
		out.Standing = s.donorStandings(idx)
	}
	return out
}

/* ------------------------------------------------------------- standings --- */

// shareOf turns a position and a field size into a top-N percentage.
//
// Rounded to six decimals rather than left at full float precision. Six rather than a
// friendlier two, because the smallest share a field of 2.1M donors can express is
// 0.0000471% — rounding to anything coarser reports the best donor in the world as
// being in the top 0%, which is not a rounding of the truth so much as a different
// claim. Presentation decides how much of that to show; the field carries it.
func shareOf(rank, of int) *Standing {
	if rank <= 0 || of <= 0 {
		return nil
	}
	p := float64(rank) / float64(of) * 100
	return &Standing{TopPercent: math.Round(p*1e6) / 1e6, Of: of}
}

// monthStanding places an entity within the month-to-date ordering.
//
// The ordering is best-first, so both questions it answers are binary searches rather
// than scans: the first position whose production falls below the subject's is the
// count ahead of it, and the first position at zero is where the field stops. That
// matters — the donor ordering is 2.1M long, and a linear pass over it to answer "top
// what percent" would cost more than every other figure on the page combined.
//
// The field deliberately excludes everyone who produced nothing this month. Counting
// them would put anyone with a single point in the top few percent of donors, which is
// arithmetic rather than standing.
func monthStanding(order []int32, val func(int32) int64, self int64) *Standing {
	if self <= 0 || len(order) == 0 {
		return nil
	}
	// Strictly greater, not "not less": entities tied with the subject are level with
	// it, not ahead of it, and counting them as ahead would rank three donors on the
	// same points differently according to where the sort happened to put them.
	ahead := sort.Search(len(order), func(i int) bool { return val(order[i]) <= self })
	of := sort.Search(len(order), func(i int) bool { return val(order[i]) <= 0 })
	return shareOf(ahead+1, of)
}

// teamStandings is a team's position by lifetime points and by this month's.
func (s *Snapshot) teamStandings(slot, pos int32) *Standings {
	return &Standings{
		// Lifetime needs no search: the rank is already known and the field is every
		// team tracked.
		Lifetime: shareOf(int(pos), s.Totals.Teams),
		ThisMonth: monthStanding(s.Ranks.TeamOrderFor(rank.ThisMonth),
			func(i int32) int64 { return rollup(s.TeamMonth, i) }, rollup(s.TeamMonth, slot)),
	}
}

// donorStandings is a donor's position by lifetime points and by this month's.
func (s *Snapshot) donorStandings(idx int32) *Standings {
	return &Standings{
		Lifetime: shareOf(int(idx)+1, s.Totals.Donors),
		ThisMonth: monthStanding(s.Ranks.DonorOrderFor(rank.ThisMonth),
			s.Ranks.DonorMonth, s.Ranks.DonorMonth(idx)),
	}
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
	ordered := s.orderMembers(members, byProduction)
	truncated := false
	if limit > 0 && len(ordered) > limit {
		// With a cap, the rows dropped should be the ones that matter least.
		ordered, truncated = ordered[:limit], true
	}
	return s.memberViews(ordered), truncated
}

// orderMembers puts a donor's memberships in the requested order without building a
// view for any of them, so a caller that only wants a page pays for a page.
//
// The lifetime order is free: rank.DonorMembers already returns them in global rank
// order, which is descending lifetime points. Only the production ordering needs
// work, and it is the rarer of the two.
//
// In the free case the caller's slice is handed straight back, so it may still alias
// the table's own storage. Callers slice and read it; nothing here may sort or write
// through it.
func (s *Snapshot) orderMembers(members []int32, byProduction bool) []int32 {
	if !byProduction {
		return members
	}
	ordered := append([]int32(nil), members...)
	// Stable, so equal production keeps the incoming order — which is now global rank
	// order, making the tiebreak "same output, better lifetime rank first" rather than
	// whatever the sort happened to do. Most of a wide donor's teams have produced
	// nothing at all, so ties are the common case, not the edge: with an unstable sort
	// their order was arbitrary and could reshuffle between cycles for no reason.
	sort.SliceStable(ordered, func(a, b int) bool {
		return s.Members.Last7d(ordered[a]) > s.Members.Last7d(ordered[b])
	})
	return ordered
}

// memberViews builds the wire rows for exactly the slots given. This is the expensive
// half — two arena copies, a team lookup and a window read apiece — so callers slice
// first and build second.
func (s *Snapshot) memberViews(slots []int32) []Member {
	out := make([]Member, 0, len(slots))
	for _, slot := range slots {
		out = append(out, s.memberView(slot, true))
	}
	return out
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

// rosterRanks reports whether a member has anything to rank on this column, using
// the cheapest test that answers it.
//
// This is not the same as computing the value and comparing it to zero, and the
// difference is the whole cost of the operation. PointsPerDay divides the seven-day
// total by the span that member has actually been observed for, and that span costs a
// binary search over the retained cycles — 882,940 of them to partition the largest
// roster, which measured 12.9ms against 2.3ms for the columns that are a plain array
// read. It is zero exactly when the seven-day total is zero, so the division is only
// needed for the members that survive the partition.
func (s *Snapshot) rosterRanks(slot int32, k rank.SortKey) bool {
	if k == rank.PerDay {
		return s.Members.Last7d(slot) > 0
	}
	return s.rosterValue(slot, k) > 0
}

// rosterValue is the figure a member is ranked by for a given column.
func (s *Snapshot) rosterValue(slot int32, k rank.SortKey) int64 {
	switch k {
	case rank.PerDay:
		return s.Members.PointsPerDay(slot)
	case rank.Last24h:
		return s.Members.Last24h(slot)
	case rank.ThisWeek:
		return s.Members.ThisWeek(slot)
	case rank.ThisMonth:
		return rollup(s.MemberMonth, slot)
	case rank.WUs:
		return s.State.Members[slot].WUs
	}
	return s.State.Members[slot].Score
}

// orderRoster returns the slots at positions [lo, hi) of a team's roster ordered by
// key, and how many members the ordering covers.
//
// Sorting a roster outright is not affordable on the largest team: 882,940 members
// costs 13ms, against a 0.11ms median, on a route anyone can request. But of 2.1M
// donors only ~6,600 produced anything in the last seven days, so for every column
// except lifetime almost the entire roster is tied at zero — and a tie needs no
// sorting. Only the members with something to rank get sorted; the rest keep the
// order they are already stored in, which is lifetime rank.
//
// The zero tail is therefore never materialised. A first page comes entirely from the
// sorted head and touches nothing else; only a page reaching past it walks the roster,
// and then only to skip what is already above.
func (s *Snapshot) orderRoster(slots []int32, k rank.SortKey, activeOnly bool, lo, hi int) []int32 {
	keep := func(slot int32) bool { return !activeOnly || s.Members.Last7d(slot) > 0 }

	// Lifetime is the order the roster is already stored in.
	if k == rank.Lifetime {
		out := make([]int32, 0, hi-lo)
		seen := 0
		for _, slot := range slots {
			if !keep(slot) {
				continue
			}
			if seen >= lo {
				out = append(out, slot)
			}
			if seen++; seen >= hi {
				break
			}
		}
		return out
	}

	type scored struct {
		slot int32
		v    int64
	}
	var head []scored
	for _, slot := range slots {
		if !keep(slot) {
			continue
		}
		if s.rosterRanks(slot, k) {
			head = append(head, scored{slot, s.rosterValue(slot, k)})
		}
	}
	// Stable, so members tied on the column keep lifetime order rather than an
	// arbitrary one that reshuffles between cycles for no reason.
	sort.SliceStable(head, func(a, b int) bool { return head[a].v > head[b].v })

	out := make([]int32, 0, hi-lo)
	for i := lo; i < hi && i < len(head); i++ {
		out = append(out, head[i].slot)
	}
	if hi <= len(head) {
		return out
	}
	// Into the tail: everything left produced nothing on this column, so they are
	// served in the order they are stored, skipping whatever the head already took.
	skip := lo - len(head)
	if skip < 0 {
		skip = 0
	}
	need := hi - len(head) - skip
	for _, slot := range slots {
		if !keep(slot) || s.rosterRanks(slot, k) {
			continue
		}
		if skip > 0 {
			skip--
			continue
		}
		out = append(out, slot)
		if need--; need <= 0 {
			break
		}
	}
	return out
}

// tieAt reports how many entities share the value that put this one at rank, counting
// itself. One means the rank is unique.
//
// Ranks are ordinal here, as they are on every site that publishes them: equal values
// still get consecutive positions, broken by which entity was seen first. That is
// deterministic, and it is also arbitrary — 2,139,090 of 2,710,286 members share a
// lifetime score with somebody, so for most of the corpus the number after "rank" is a
// tiebreak rather than a placing. Reporting the width of the tie keeps the integer
// people actually want while saying how much of it is real.
//
// The order is sorted by the value, so equal values are contiguous and the run is two
// binary searches: no per-entity precomputation and nothing to keep in memory, which
// matters when the largest run is most of the corpus.
func tieAt(n int, rank int32, score func(i int) int64) int32 {
	i := int(rank) - 1
	if rank <= 0 || i >= n {
		return 0
	}
	v := score(i)
	lo, hi := 0, i // first index holding v
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if score(mid) > v {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	first := lo
	lo, hi = i, n-1 // last index holding v
	for lo < hi {
		mid := int(uint(lo+hi+1) >> 1)
		if score(mid) < v {
			hi = mid - 1
		} else {
			lo = mid
		}
	}
	return int32(lo - first + 1)
}

func (s *Snapshot) teamTie(rank int32) int32 {
	return tieAt(len(s.Ranks.TeamOrder), rank, func(i int) int64 {
		return s.State.Teams[s.Ranks.TeamOrder[i]].Score
	})
}

func (s *Snapshot) memberTie(rank int32) int32 {
	return tieAt(len(s.Ranks.MemberOrder), rank, func(i int) int64 {
		return s.State.Members[s.Ranks.MemberOrder[i]].Score
	})
}

func (s *Snapshot) donorTie(rank int32) int32 {
	return tieAt(len(s.Ranks.Donors), rank, func(i int) int64 { return s.Ranks.Donors[i].Score })
}
