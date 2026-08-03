// Package rank builds the ordered views served by the leaderboards.
//
// Every ranking is recomputed from scratch once per cycle and then read many
// thousands of times, so the work is arranged to make reads trivial: after Build,
// answering "what rank is this member?" or "who is 25 places above this team?" is an
// array index or a slice, with no query and no lock.
//
// # Donors versus members
//
// A *member* is a (name, team) pair — the grain of the upstream feed. A *donor* is a
// name, aggregated across every team it folds for. EOC has no donor concept at all:
// it keys users per team, so one person on three teams appears as three unrelated
// users with three separate ranks. Aggregating is deliberate (R1) and matches how
// stats.foldingathome.org presents donors.
//
// The aggregation is a read-time view over member state, never a stored identity, so
// the merge policy can change without re-ingesting anything.
package rank

import (
	"sort"
	"time"

	"folding/internal/model"
)

// Config tunes how the tables are built.
type Config struct {
	// PseudoIdentityTeams is the team count above which a donor name is flagged as
	// almost certainly not one person. Generic and default names are shared by
	// thousands of unrelated people — "PS3" appears on 10,426 teams, "Anonymous" on
	// 5,993 — and aggregating them produces a fictional mega-donor that would
	// otherwise top the leaderboard. Flagged, never hidden: the points are real
	// even though the person is not.
	PseudoIdentityTeams int32
}

// DefaultConfig is tuned against the reference corpus. Legitimate multi-team folders
// sit in the low tens at most; the pseudo-identities are three orders of magnitude
// above that, so the threshold is not sensitive.
var DefaultConfig = Config{PseudoIdentityTeams: 50}

// Donor is a name aggregated across every team it appears on.
type Donor struct {
	NameID    int32
	Score     int64
	WUs       int64
	TeamCount int32
	// LikelyNotAPerson marks names shared by implausibly many teams.
	LikelyNotAPerson bool
}

// Table is the complete set of ranked views for one snapshot. All slices are
// read-only once Build returns.
type Table struct {
	At time.Time

	// MemberOrder lists member slots best-first. MemberRank and InTeamRank are
	// indexed by member slot and hold 1-based ranks.
	MemberOrder []int32
	MemberRank  []int32
	InTeamRank  []int32

	// TeamOrder lists team slots best-first; TeamRank is indexed by team slot.
	TeamOrder []int32
	TeamRank  []int32

	// Donors is ordered best-first, so a donor's rank is its index plus one.
	// DonorIndex maps a name id to its position, or -1 for names with no members
	// (team names share the arena).
	Donors     []Donor
	DonorIndex []int32

	// donorMembers is a CSR-style flattening: the member slots for donor i are
	// donorMembers[donorOffsets[i]:donorOffsets[i+1]]. One flat array rather than
	// 2.1M individual slices.
	donorMembers []int32
	donorOffsets []int32

	// teamMembers is the same flattening keyed by upstream team number, holding
	// each team's roster in global rank order.
	teamMembers []int32
	teamOffsets []int32

	// nameOrder indexes into Donors, sorted case-insensitively by name, for
	// prefix search.
	nameOrder []int32

	buf sortBuf
}

// Build computes every ranked view from the current state.
func Build(st *model.State, at time.Time, cfg Config) *Table {
	t := &Table{At: at}
	t.buildTeams(st)
	t.buildMembers(st)
	t.buildDonors(st, cfg)
	t.buildSearchIndex(st)
	// The sort scratch is ~22 MB at corpus scale and is useless once the orders are
	// materialised. Tables outlive their build (the previous one is still serving
	// until the pointer swap), so holding two copies is pure waste.
	t.buf = sortBuf{}
	return t
}

func (t *Table) buildTeams(st *model.State) {
	scores := make([]int64, len(st.Teams))
	for i, tm := range st.Teams {
		scores[i] = tm.Score
	}
	t.TeamOrder = append([]int32(nil), sortDescByScore(scores, &t.buf)...)
	t.TeamRank = make([]int32, len(st.Teams))
	for i, slot := range t.TeamOrder {
		t.TeamRank[slot] = int32(i) + 1
	}
}

func (t *Table) buildMembers(st *model.State) {
	scores := make([]int64, len(st.Members))
	maxTeamID := int32(0)
	for i, m := range st.Members {
		scores[i] = m.Score
		if m.TeamID > maxTeamID {
			maxTeamID = m.TeamID
		}
	}
	t.MemberOrder = append([]int32(nil), sortDescByScore(scores, &t.buf)...)

	t.MemberRank = make([]int32, len(st.Members))
	t.InTeamRank = make([]int32, len(st.Members))

	// Counting in-team position by walking the global order keeps the two rankings
	// consistent by construction — a member ranked above another globally can never
	// rank below them within their shared team.
	//
	// Indexed directly by upstream team number rather than through a map: team ids
	// are bounded around 1.3M, so a dense array costs ~5 MB and turns 2.7M map
	// lookups per cycle into array increments.
	counters := make([]int32, maxTeamID+1)
	for i, slot := range t.MemberOrder {
		t.MemberRank[slot] = int32(i) + 1
		tid := st.Members[slot].TeamID
		counters[tid]++
		t.InTeamRank[slot] = counters[tid]
	}

	// Build each team's roster while the global order is already to hand. counters
	// now holds the per-team totals, so the offsets are a prefix sum over them and
	// a second pass scatters members in rank order.
	t.teamOffsets = make([]int32, maxTeamID+2)
	for tid, n := range counters {
		t.teamOffsets[tid+1] = t.teamOffsets[tid] + n
	}
	t.teamMembers = make([]int32, len(st.Members))
	cursor := append([]int32(nil), t.teamOffsets[:maxTeamID+1]...)
	for _, slot := range t.MemberOrder {
		tid := st.Members[slot].TeamID
		t.teamMembers[cursor[tid]] = slot
		cursor[tid]++
	}
}

// TeamMembers returns a team's member slots in global rank order. The result aliases
// internal storage and must not be modified.
func (t *Table) TeamMembers(teamID int32) []int32 {
	if teamID < 0 || int(teamID)+1 >= len(t.teamOffsets) {
		return nil
	}
	return t.teamMembers[t.teamOffsets[teamID]:t.teamOffsets[teamID+1]]
}

func (t *Table) buildDonors(st *model.State, cfg Config) {
	nNames := st.Names.Len()
	score := make([]int64, nNames)
	wus := make([]int64, nNames)
	count := make([]int32, nNames)

	for _, m := range st.Members {
		score[m.NameID] += m.Score
		wus[m.NameID] += m.WUs
		count[m.NameID]++
	}

	// Only names with at least one member become donors; the arena also holds team
	// names, which have no production of their own.
	present := make([]int32, 0, nNames)
	for id := int32(0); int(id) < nNames; id++ {
		if count[id] > 0 {
			present = append(present, id)
		}
	}

	dScores := make([]int64, len(present))
	for i, id := range present {
		dScores[i] = score[id]
	}
	order := sortDescByScore(dScores, &t.buf)

	t.Donors = make([]Donor, len(present))
	t.DonorIndex = make([]int32, nNames)
	for i := range t.DonorIndex {
		t.DonorIndex[i] = -1
	}
	for pos, oi := range order {
		nameID := present[oi]
		t.Donors[pos] = Donor{
			NameID:           nameID,
			Score:            score[nameID],
			WUs:              wus[nameID],
			TeamCount:        count[nameID],
			LikelyNotAPerson: count[nameID] > cfg.PseudoIdentityTeams,
		}
		t.DonorIndex[nameID] = int32(pos)
	}

	// CSR fill: count, prefix-sum, scatter.
	t.donorOffsets = make([]int32, len(t.Donors)+1)
	for _, m := range st.Members {
		t.donorOffsets[t.DonorIndex[m.NameID]+1]++
	}
	for i := 1; i < len(t.donorOffsets); i++ {
		t.donorOffsets[i] += t.donorOffsets[i-1]
	}
	t.donorMembers = make([]int32, len(st.Members))
	cursor := append([]int32(nil), t.donorOffsets[:len(t.Donors)]...)
	for slot, m := range st.Members {
		d := t.DonorIndex[m.NameID]
		t.donorMembers[cursor[d]] = int32(slot)
		cursor[d]++
	}
}

// DonorRank returns a donor's 1-based rank by name id, or 0 if the name has no
// members.
func (t *Table) DonorRank(nameID int32) int32 {
	if nameID < 0 || int(nameID) >= len(t.DonorIndex) {
		return 0
	}
	i := t.DonorIndex[nameID]
	if i < 0 {
		return 0
	}
	return i + 1
}

// DonorMembers returns the member slots belonging to donor i, ordered by slot. The
// result aliases internal storage and must not be modified.
func (t *Table) DonorMembers(i int32) []int32 {
	if i < 0 || int(i) >= len(t.Donors) {
		return nil
	}
	return t.donorMembers[t.donorOffsets[i]:t.donorOffsets[i+1]]
}

// MemberWindow returns the member slots within n places either side of rank, best
// first — the neighbourhood a leaderboard page or "who is near me" view needs. The
// window shifts to stay in bounds at the ends of the list.
func (t *Table) MemberWindow(rank int32, n int) []int32 {
	return window(t.MemberOrder, rank, n)
}

// TeamWindow is MemberWindow for teams.
func (t *Table) TeamWindow(rank int32, n int) []int32 {
	return window(t.TeamOrder, rank, n)
}

func window(order []int32, rank int32, n int) []int32 {
	if len(order) == 0 || rank < 1 {
		return nil
	}
	lo := int(rank) - 1 - n
	hi := int(rank) + n
	if lo < 0 {
		lo = 0
	}
	if hi > len(order) {
		hi = len(order)
	}
	return order[lo:hi]
}

// Change reports rank movement between two tables, positive meaning improved.
// Entities absent from the earlier table return 0 rather than a fabricated jump.
func Change(now, then []int32, id int32) int32 {
	if int(id) >= len(now) || int(id) >= len(then) {
		return 0
	}
	before, after := then[id], now[id]
	if before == 0 || after == 0 {
		return 0
	}
	return before - after
}

// ---------------------------------------------------------------- search ---

// buildSearchIndex orders donor name ids case-insensitively so a prefix lookup is
// a binary search plus a scan, rather than a pass over 2.1M names per keystroke.
//
// The sort costs a second or two and runs once per cycle alongside the rest of the
// table; searches then cost microseconds, which is what makes live results as you
// type affordable at all.
func (t *Table) buildSearchIndex(st *model.State) {
	t.nameOrder = make([]int32, len(t.Donors))
	for i := range t.Donors {
		t.nameOrder[i] = int32(i)
	}
	sort.Slice(t.nameOrder, func(a, b int) bool {
		return lessFold(
			st.Names.Bytes(t.Donors[t.nameOrder[a]].NameID),
			st.Names.Bytes(t.Donors[t.nameOrder[b]].NameID),
		)
	})
}

// lessFold compares ASCII case-insensitively. Donor names are arbitrary bytes, so
// this deliberately does not attempt Unicode case folding: it only needs to group
// "DH" with "dh" for lookup, and full folding would cost more than the search.
func lessFold(a, b []byte) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		x, y := lowerByte(a[i]), lowerByte(b[i])
		if x != y {
			return x < y
		}
	}
	return len(a) < len(b)
}

func lowerByte(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

func hasPrefixFold(s, prefix []byte) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := range prefix {
		if lowerByte(s[i]) != lowerByte(prefix[i]) {
			return false
		}
	}
	return true
}

// DonorPrefix returns up to limit donor indices whose names begin with q,
// case-insensitively, ordered by rank so the most significant match leads.
func (t *Table) DonorPrefix(st *model.State, q string, limit int) []int32 {
	if q == "" || limit <= 0 || len(t.nameOrder) == 0 {
		return nil
	}
	p := []byte(q)

	// First index whose name is not ordered before the prefix.
	lo := sort.Search(len(t.nameOrder), func(i int) bool {
		return !lessFold(st.Names.Bytes(t.Donors[t.nameOrder[i]].NameID), p)
	})

	var hits []int32
	for i := lo; i < len(t.nameOrder); i++ {
		d := t.nameOrder[i]
		if !hasPrefixFold(st.Names.Bytes(t.Donors[d].NameID), p) {
			break // sorted, so the prefix range has ended
		}
		hits = append(hits, d)
		// Scanning the whole range for a one-letter query would mean walking most
		// of the corpus, so stop once there is plenty to rank.
		if len(hits) >= limit*20 {
			break
		}
	}

	// Donors are stored in rank order, so a numeric sort on the index ranks them.
	sort.Slice(hits, func(a, b int) bool { return hits[a] < hits[b] })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}
