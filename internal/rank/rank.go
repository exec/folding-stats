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

	"folding/internal/metrics"
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

	// nameOrder indexes into Donors, and teamNameOrder into State.Teams, both sorted
	// case-insensitively by name so a prefix lookup is a binary search plus a short
	// scan rather than a pass over the whole corpus.
	nameOrder     []int32
	teamNameOrder []int32

	// 24-hour rank movement, positive meaning improved. Indexed by member slot, team
	// slot and donor position respectively, and nil until a 24h baseline exists.
	//
	// The baselines are entity counts, not timestamps: an id at or above one belongs
	// to an entity that did not exist a day ago, so it has no earlier rank. Storing
	// the cutoff rather than a parallel "known" array keeps this to one int per
	// dimension instead of a bitmap over 2.7M members.
	memberChange   []int32
	teamChange     []int32
	donorChange    []int32
	memberBaseline int32
	teamBaseline   int32
	donorBaseline  int32
	donorKnown     []bool

	// Period orders for the leaderboard tabs, best-first, parallel to TeamOrder and
	// to Donors. Nil until BuildPeriods runs.
	//
	// Only the ordering is kept, not a rank per entity: a leaderboard page is a slice
	// of the order, and a position is its index. Storing a rank array per period as
	// well would triple this for a number the page already knows.
	teamOrders  map[SortKey][]int32
	donorOrders map[SortKey][]int32

	// Each donor's production per period, summed from its members. Kept because they
	// are the sort keys the API also has to report — recomputing one per response
	// means re-walking every member of the donor, and the donors with the most
	// members are exactly the ones a "sort by teams" or "sort by WUs" page selects.
	//
	// Filling them costs nothing: BuildOrders already makes this pass to build the
	// orderings, and previously discarded every column but the month. Retaining them
	// adds 102 MB of live heap across 2.1M donors — but measured resident size grew
	// by 17 MB, because that memory was already being allocated on every publish and
	// only thrown away afterwards. The publish itself is where the full amount shows,
	// since the next set is built while this one is still being served.
	donorMonth    []int64
	donorDay      []int64
	donorWeek     []int64
	donorLast24   []int64
	donorLast7d   []int64
	donorUpdate   []int64
	donorPerDay   []int64
	donorPerDay24 []int64

	buf sortBuf
}

// SortKey names a leaderboard ordering. One per numeric column a reader can see, so
// "sort by what that column shows" is always available and always means what the
// column heading says.
type SortKey string

const (
	// Lifetime is cumulative score, the default and the only ordering here that is
	// not a rate.
	Lifetime SortKey = "lifetime"

	// PerDay is points_per_day_24h_avg — the rolling day. It is the column most
	// people actually rank by, and it is deliberately not called "daily": Today is a
	// calendar bucket and this is an average over a window, and conflating the two is
	// the mistake the field naming exists to prevent.
	//
	// It ordered by the seven-day average until 11 August 2026. The site's Per day
	// column shows this ordering's own figure, so the two have to be the same one: a
	// column ranked by a number it is not displaying reads as a rendering bug, and the
	// seven-day figure is smoothed enough that a machine switched on this morning does
	// not appear near the top of a board sorted by "per day" for most of a week.
	PerDay SortKey = "per_day"

	// Today, ThisWeek and ThisMonth are calendar buckets in UTC, not rolling windows —
	// the same buckets points_today_utc, points_this_week_utc and points_this_month_utc
	// report. Today therefore reads low just after 00:00 UTC, which is correct: it
	// answers "produced today", not "produced in the last 24 hours".
	Today     SortKey = "today"
	ThisWeek  SortKey = "this_week"
	ThisMonth SortKey = "this_month"

	// Last24h is the rolling twenty-four hours, the counterpart to Today.
	Last24h SortKey = "last_24h"

	// WUs is work units rather than points — a different measure of the same effort,
	// and the one people compare when point values across projects are in dispute.
	WUs SortKey = "wus"

	// Roster size. Members applies to teams and Teams to donors; each is ignored on
	// the other, where it falls back to lifetime rather than erroring, because a
	// column that does not exist there was never orderable in the first place.
	Members SortKey = "members"
	Teams   SortKey = "teams"
)

// sortAliases keeps the first published names working. daily/weekly/monthly shipped
// before per_day existed, at which point "daily" became ambiguous with it.
var sortAliases = map[SortKey]SortKey{
	"daily":   Today,
	"weekly":  ThisWeek,
	"monthly": ThisMonth,
}

// NormalizeSort resolves an alias and reports whether the result is a known ordering.
func NormalizeSort(k SortKey) (SortKey, bool) {
	if alias, ok := sortAliases[k]; ok {
		k = alias
	}
	switch k {
	case Lifetime, PerDay, Today, ThisWeek, ThisMonth, Last24h, WUs, Members, Teams:
		return k, true
	}
	return "", false
}

// BuildOrders computes every leaderboard ordering.
//
// One radix sort per key per entity kind, once a cycle, against many thousands of
// reads — the same trade the rest of this package makes. Sorting on demand is not an
// option at 2.1M donors, and a lazily filled cache would put a full sort on whichever
// unlucky request arrived first after a publish.
//
// teamMonth and memberMonth are month-to-date totals indexed by slot; everything else
// comes from the rolling windows, which already maintain calendar buckets. A nil or
// short month slice yields a zeroed ordering rather than a wrong one.
func (t *Table) BuildOrders(st *model.State, members, teams *metrics.Window,
	teamMonth, memberMonth []int64) {

	t.teamOrders = map[SortKey][]int32{}
	t.donorOrders = map[SortKey][]int32{}

	tMonth := indexed(teamMonth, len(st.Teams))
	fill := func(n int, get func(int32) int64) []int64 {
		out := make([]int64, n)
		for i := range out {
			out[i] = get(int32(i))
		}
		return out
	}

	for _, o := range []struct {
		key SortKey
		val func(int32) int64
	}{
		{PerDay, teams.PointsPerDay24h},
		{Today, teams.Today},
		{ThisWeek, teams.ThisWeek},
		{ThisMonth, func(i int32) int64 { return tMonth[i] }},
		{Last24h, teams.Last24h},
		{WUs, func(i int32) int64 { return st.Teams[i].WUs }},
		{Members, func(i int32) int64 { return int64(len(t.TeamMembers(st.Teams[i].ID))) }},
	} {
		t.teamOrders[o.key] = t.orderBy(fill(len(st.Teams), o.val))
	}
	t.teamOrders[Lifetime] = t.TeamOrder

	// Donor figures aggregate across the donor's members, the same read-time view
	// buildDonors takes for lifetime totals. One pass over every member fills them
	// all, because the pass itself — 2.7M random reads into the member array — costs
	// far more than the arithmetic inside it.
	n := len(t.Donors)
	day := make([]int64, n)
	week := make([]int64, n)
	month := make([]int64, n)
	last24 := make([]int64, n)
	last7d := make([]int64, n)
	update := make([]int64, n)
	observed := make([]time.Duration, n)
	mMonth := indexed(memberMonth, len(st.Members))
	for slot := range st.Members {
		d := t.DonorIndex[st.Members[slot].NameID]
		if d < 0 {
			continue
		}
		id := int32(slot)
		day[d] += members.Today(id)
		week[d] += members.ThisWeek(id)
		month[d] += mMonth[slot]
		last24[d] += members.Last24h(id)
		last7d[d] += members.Last7d(id)
		update[d] += members.LastUpdate(id)
		// A donor has been observed since the first of its members was; see donorView.
		if span := members.ObservedSpan(id); span > observed[d] {
			observed[d] = span
		}
	}
	perDay := make([]int64, n)
	perDay24 := make([]int64, n)
	wus := make([]int64, n)
	teamCount := make([]int64, n)
	for d := range t.Donors {
		perDay[d] = metrics.PerDay(last7d[d], observed[d])
		// Capped at the window itself: points in the last day are already a daily
		// rate, and only a donor younger than a day has less than one to divide by.
		span := observed[d]
		if span > 24*time.Hour {
			span = 24 * time.Hour
		}
		perDay24[d] = metrics.PerDay(last24[d], span)
		wus[d] = t.Donors[d].WUs
		teamCount[d] = int64(t.Donors[d].TeamCount)
	}

	t.donorOrders[Lifetime] = identity(n) // Donors is already in lifetime order
	t.donorOrders[PerDay] = t.orderBy(perDay24)
	t.donorOrders[Today] = t.orderBy(day)
	t.donorOrders[ThisWeek] = t.orderBy(week)
	t.donorOrders[ThisMonth] = t.orderBy(month)
	t.donorOrders[Last24h] = t.orderBy(last24)
	t.donorOrders[WUs] = t.orderBy(wus)
	t.donorOrders[Teams] = t.orderBy(teamCount)

	t.donorMonth, t.donorDay, t.donorWeek = month, day, week
	t.donorLast24, t.donorLast7d, t.donorUpdate, t.donorPerDay = last24, last7d, update, perDay
	t.donorPerDay24 = perDay24

	t.buf = sortBuf{}
}

// DonorTotals is a donor's production over every period the API reports.
type DonorTotals struct {
	LastUpdate int64
	Last24h    int64
	Last7d     int64
	Today      int64
	ThisWeek   int64
	ThisMonth  int64
	PerDay     int64
	// PerDay24h is the same rate over the rolling day rather than the week.
	PerDay24h int64
}

// DonorTotals returns a donor's summed production, and whether it is available.
//
// False means BuildOrders has not run — the case for a Table built directly in a
// test. Callers sum the members themselves then, so the totals are an accelerator
// rather than something correctness depends on.
func (t *Table) DonorTotals(i int32) (DonorTotals, bool) {
	if i < 0 || int(i) >= len(t.donorLast7d) {
		return DonorTotals{}, false
	}
	return DonorTotals{
		LastUpdate: t.donorUpdate[i],
		Last24h:    t.donorLast24[i],
		Last7d:     t.donorLast7d[i],
		Today:      t.donorDay[i],
		ThisWeek:   t.donorWeek[i],
		ThisMonth:  t.donorMonth[i],
		PerDay:     t.donorPerDay[i],
		PerDay24h:  t.donorPerDay24[i],
	}, true
}

// orderBy sorts ids best-first by score. The radix scratch aliases across calls, so
// the result is copied out.
func (t *Table) orderBy(scores []int64) []int32 {
	if len(scores) == 0 {
		return nil
	}
	return append([]int32(nil), sortDescByScore(scores, &t.buf)...)
}

// indexed returns s sized to exactly n, so a short or absent rollup degrades to zeros
// rather than to an out-of-range read.
func indexed(s []int64, n int) []int64 {
	if len(s) == n {
		return s
	}
	out := make([]int64, n)
	copy(out, s)
	return out
}

func identity(n int) []int32 {
	out := make([]int32, n)
	for i := range out {
		out[i] = int32(i)
	}
	return out
}

// TeamOrderFor returns team slots ordered best-first for the key. Keys that name a
// column teams do not have fall back to lifetime rather than erroring.
func (t *Table) TeamOrderFor(k SortKey) []int32 {
	if o, ok := t.teamOrders[k]; ok && o != nil {
		return o
	}
	return t.TeamOrder
}

// DonorOrderFor is TeamOrderFor for donors. For Lifetime this is the identity, because
// Donors is stored in lifetime order.
func (t *Table) DonorOrderFor(k SortKey) []int32 {
	if o, ok := t.donorOrders[k]; ok && o != nil {
		return o
	}
	return identity(len(t.Donors))
}

// DonorMonth is a donor's month-to-date production.
func (t *Table) DonorMonth(i int32) int64 {
	if i < 0 || int(i) >= len(t.donorMonth) {
		return 0
	}
	return t.donorMonth[i]
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
	// Scattered in global rank order rather than slot order, exactly as the team
	// rosters above are. A donor's teams are presented best-first everywhere they
	// appear, and scattering in slot order meant every one of those places had to sort
	// the donor's whole membership first — 10,426 entries for a name like "PS3", to
	// return a page of 100. The global order is already to hand here, so ordering it
	// once per cycle costs nothing and every read is a slice.
	t.donorMembers = make([]int32, len(st.Members))
	cursor := append([]int32(nil), t.donorOffsets[:len(t.Donors)]...)
	for _, slot := range t.MemberOrder {
		d := t.DonorIndex[st.Members[slot].NameID]
		t.donorMembers[cursor[d]] = slot
		cursor[d]++
	}
}

// The published Snapshot holds a pointer to the *live* model.State but to a table
// frozen at the last publish, so State grows past this table between cycles: every
// entity first seen since it was built has a slot beyond these arrays. Indexing them
// blind panicked the handler — `index out of range [4] with length 3` — for any
// request naming a new team or donor during the seconds between state.Apply and
// Publish, on every detail, rivals, history and search route.
//
// Zero means "not ranked in this snapshot", which is the convention DonorRank already
// used, and is the truthful answer: this snapshot describes the corpus as of its own
// timestamp, and the entity did not exist then.

// TeamRankOf returns a team's rank, or 0 for a slot this table does not cover.
func (t *Table) TeamRankOf(slot int32) int32 { return rankAt(t.TeamRank, slot) }

// MemberRankOf returns a member's global rank, or 0 if this table predates it.
func (t *Table) MemberRankOf(slot int32) int32 { return rankAt(t.MemberRank, slot) }

// InTeamRankOf returns a member's rank within its team, or 0 if this table predates it.
func (t *Table) InTeamRankOf(slot int32) int32 { return rankAt(t.InTeamRank, slot) }

// DonorIndexOf resolves a name id to its position in Donors, or -1 when the name has
// no members or arrived after this table was built.
func (t *Table) DonorIndexOf(nameID int32) int32 {
	if nameID < 0 || int(nameID) >= len(t.DonorIndex) {
		return -1
	}
	return t.DonorIndex[nameID]
}

func rankAt(ranks []int32, slot int32) int32 {
	if slot < 0 || int(slot) >= len(ranks) {
		return 0
	}
	return ranks[slot]
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

// DonorMembers returns the member slots belonging to donor i in global rank order,
// so the donor's strongest membership leads. The result aliases internal storage and
// must not be modified.
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

// ------------------------------------------------------- 24-hour movement ---

// BuildChange24h fills in rank movement over the last 24 hours.
//
// It is a second phase rather than part of Build because it needs the rate windows,
// and because it is the one part of the table that can legitimately be absent: for
// the first day of a cold start there is no earlier corpus to compare against.
//
// Nothing about a past ranking is stored. A cumulative total minus its own last-24h
// production is that entity's total a day ago, so the earlier ranking is recovered by
// sorting reconstructed totals — the same radix sort Build already uses. Persisting
// ranks instead would cost a rank per entity per cycle (~92 GB a year for members
// alone) to answer a question the deltas already contain.
func (t *Table) BuildChange24h(st *model.State, members, teams *metrics.Window) {
	t.memberChange, t.memberBaseline = t.change24h(memberScores(st), members, t.MemberRank)
	t.teamChange, t.teamBaseline = t.change24h(teamScores(st), teams, t.TeamRank)
	t.donorChange24h(st, members)
	// The sort scratch is ~22 MB and, as in Build, useless once the orders are
	// materialised.
	t.buf = sortBuf{}
}

func memberScores(st *model.State) func(int32) int64 {
	return func(i int32) int64 { return st.Members[i].Score }
}

func teamScores(st *model.State) func(int32) int64 {
	return func(i int32) int64 { return st.Teams[i].Score }
}

// change24h ranks the corpus as it stood a day ago and returns the movement since,
// indexed by slot. The returned baseline is the exclusive upper bound of slots that
// existed then.
func (t *Table) change24h(score func(int32) int64, w *metrics.Window, nowRank []int32) ([]int32, int32) {
	base, ok := w.Baseline()
	if !ok {
		return nil, 0
	}
	if int(base) > len(nowRank) {
		base = int32(len(nowRank))
	}
	if base == 0 {
		return nil, 0
	}

	hist := make([]int64, base)
	for i := int32(0); i < base; i++ {
		hist[i] = histScore(score(i), w.Last24h(i))
	}

	// Ranked over only the entities that existed, so an entity that has appeared
	// since does not silently displace everyone below it in the earlier ordering.
	order := sortDescByScore(hist, &t.buf)
	change := make([]int32, base)
	for pos, id := range order {
		change[id] = int32(pos) + 1 - nowRank[id]
	}
	return change, base
}

// histScore reconstructs a total as of 24 hours ago.
//
// Clamped at zero because sortDescByScore inverts the key bits, which only orders
// correctly for non-negative values. A total can only go negative here if a feed
// glitch produced deltas summing to more than the entity has ever scored — which the
// model already counts as a regression, and which must not be allowed to scramble an
// entire ranking.
func histScore(now, last24 int64) int64 {
	if v := now - last24; v > 0 {
		return v
	}
	return 0
}

// donorChange24h is the same reconstruction aggregated by name.
//
// A donor's earlier total is summed only over the members that existed then, and a
// donor with no such members is new — reported as unknown rather than as movement.
func (t *Table) donorChange24h(st *model.State, w *metrics.Window) {
	base, ok := w.Baseline()
	if !ok || len(t.Donors) == 0 {
		return
	}
	if int(base) > len(st.Members) {
		base = int32(len(st.Members))
	}

	hist := make([]int64, len(t.Donors))
	known := make([]bool, len(t.Donors))
	for slot := int32(0); slot < base; slot++ {
		d := t.DonorIndex[st.Members[slot].NameID]
		if d < 0 {
			continue
		}
		hist[d] += histScore(st.Members[slot].Score, w.Last24h(slot))
		known[d] = true
	}

	// Compact to the donors that existed, so the earlier ranking covers exactly them.
	idx := make([]int32, 0, len(t.Donors))
	for d, ok := range known {
		if ok {
			idx = append(idx, int32(d))
		}
	}
	if len(idx) == 0 {
		return
	}
	sub := make([]int64, len(idx))
	for i, d := range idx {
		sub[i] = hist[d]
	}

	order := sortDescByScore(sub, &t.buf)
	change := make([]int32, len(t.Donors))
	for pos, oi := range order {
		// Donors is stored in rank order, so a donor's current rank is its index + 1.
		d := idx[oi]
		change[d] = int32(pos) + 1 - (d + 1)
	}
	t.donorChange, t.donorKnown = change, known
	t.donorBaseline = int32(len(idx))
}

// NewSince24h reports how many donors, teams and members have appeared since the
// baseline the rank movement is measured against.
//
// It costs nothing to answer, because the baselines already exist: slots are assigned
// in first-sighting order, so "did not exist a day ago" is a comparison against a
// count rather than a timestamp, and that count is what rank movement is computed from.
// Reading it here is the difference between arrivals being knowable and needing an
// index over 2.7M first_seen values to ask.
//
// ok is false before a full day has been observed, matching rank movement: nobody is
// new when there is nothing to be new since.
func (t *Table) NewSince24h(members, teams int) (newDonors, newTeams, newMembers int, ok bool) {
	if t.donorChange == nil || t.memberBaseline == 0 || t.teamBaseline == 0 {
		return 0, 0, 0, false
	}
	return max(len(t.Donors)-int(t.donorBaseline), 0),
		max(teams-int(t.teamBaseline), 0),
		max(members-int(t.memberBaseline), 0), true
}

// MemberArrivedSince24h reports whether a member did not exist a day ago.
//
// Slots are assigned in first-sighting order, so this is a comparison against the same
// baseline rank movement uses rather than a timestamp lookup. ok is false before a full
// day has been observed, when nobody can be called new.
func (t *Table) MemberArrivedSince24h(slot int32) (bool, bool) {
	if t.memberBaseline == 0 {
		return false, false
	}
	return slot >= t.memberBaseline, true
}

// MemberChange24h reports a member's rank movement over the last 24 hours, positive
// meaning improved. ok is false when there is no earlier ranking to compare against —
// either the window has not yet covered a day, or the member did not exist then.
func (t *Table) MemberChange24h(slot int32) (int32, bool) {
	return lookupChange(t.memberChange, t.memberBaseline, slot)
}

// TeamChange24h is MemberChange24h for teams.
func (t *Table) TeamChange24h(slot int32) (int32, bool) {
	return lookupChange(t.teamChange, t.teamBaseline, slot)
}

// DonorChange24h reports movement for the donor at index i.
func (t *Table) DonorChange24h(i int32) (int32, bool) {
	if t.donorChange == nil || i < 0 || int(i) >= len(t.donorChange) || !t.donorKnown[i] {
		return 0, false
	}
	return t.donorChange[i], true
}

func lookupChange(change []int32, baseline, id int32) (int32, bool) {
	if change == nil || id < 0 || id >= baseline {
		return 0, false
	}
	return change[id], true
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

	// Teams get the same treatment, and did not have it. Search walked all 130k of
	// them in rank order per request, allocating a string for the name and another to
	// lowercase it — twice over, because an interned query also triggered an exact-name
	// scan. A query matching nothing walked the whole corpus twice and cost 16.8ms,
	// against 0.2ms for the donor half doing the same job with this index.
	t.teamNameOrder = make([]int32, len(st.Teams))
	for i := range st.Teams {
		t.teamNameOrder[i] = int32(i)
	}
	sort.Slice(t.teamNameOrder, func(a, b int) bool {
		return lessFold(
			st.Names.Bytes(st.Teams[t.teamNameOrder[a]].NameID),
			st.Names.Bytes(st.Teams[t.teamNameOrder[b]].NameID),
		)
	})
}

// TeamPrefix returns up to limit team slots whose names begin with q,
// case-insensitively, ordered by rank so the strongest team leads.
//
// Names that equal q exactly sort to the front of the prefix range, so a caller
// looking for an exact hit finds it among the first results rather than needing a
// separate pass over every team.
func (t *Table) TeamPrefix(st *model.State, q string, limit int) []int32 {
	if q == "" || limit <= 0 || len(t.teamNameOrder) == 0 {
		return nil
	}
	p := []byte(q)
	name := func(i int32) []byte { return st.Names.Bytes(st.Teams[i].NameID) }

	lo := sort.Search(len(t.teamNameOrder), func(i int) bool {
		return !lessFold(name(t.teamNameOrder[i]), p)
	})

	var hits []int32
	for i := lo; i < len(t.teamNameOrder); i++ {
		slot := t.teamNameOrder[i]
		if !hasPrefixFold(name(slot), p) {
			break // sorted, so the prefix range has ended
		}
		hits = append(hits, slot)
		// A one-letter query would otherwise walk most of the corpus; stop once there
		// is plenty to rank.
		if len(hits) >= limit*20 {
			break
		}
	}

	// Unlike Donors, team slots are not stored in rank order, so rank explicitly.
	sort.Slice(hits, func(a, b int) bool {
		return t.TeamRankOf(hits[a]) < t.TeamRankOf(hits[b])
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
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
