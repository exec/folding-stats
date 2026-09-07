package model

// Cross-feed reconciliation: does the user feed claim more than the team feed says?
//
// The parser cannot tell a record boundary from a newline inside a name, because the
// upstream format escapes neither and real names contain both. A participant whose
// chosen name is "forged\t1000000000000\t1\t32\nreal" therefore publishes as two
// parseable records: a phantom donor with a trillion points on a team of their
// choosing, followed by their genuine row. Nothing in the parse is wrong; the format
// is ambiguous and the earliest complete reading wins.
//
// That cannot be fixed locally — a character blacklist would reject the thousands of
// legitimate names containing tabs and newlines — so this closes the gap from the
// other side. The team feed is authoritative and independent: a member's score is
// their score *within* one team, so the members of a team cannot together hold more
// than the team does. Inflating a donor means inflating their team's member sum past
// the total the team feed publishes, and that is arithmetic an attacker does not
// control both sides of.
//
// Measured against the live corpus on 2026-08-13 before choosing the thresholds:
// across 129,967 teams, the number whose member rows summed to more than the team's
// authoritative total was zero. The only rows without a counterpart were 60 members
// on teams absent from the team feed — a real effect, since a new team can appear in
// one feed a minute before the other — and the largest of those held 1.2M points.

import (
	"math"
	"sort"

	"folding/internal/parse"
)

// Thresholds, as vars so tests can exercise the boundary without fabricating
// billion-point fixtures.
var (
	// ReconcileTolerance is the excess a team may show before its member rows are
	// dropped. Generous on purpose: the feeds are fetched teams-first, so the user
	// feed can legitimately be a publish newer, and a false positive here discards a
	// real cycle for a real team. A hundred million is roughly an hour of the largest
	// team's production and eighty times the largest unmatched row ever observed,
	// while capping what a forged record can claim at 0.003% of the top donor.
	ReconcileTolerance int64 = 100_000_000

	// ReconcileWarnAt is where it becomes worth saying something.
	//
	// Not zero, which was the first attempt: a real cycle has around fifty-six teams
	// showing a small excess, because a team can appear in the user feed a publish
	// before the team feed, and warning on each would put fifty-six lines an hour in
	// the journal forever. A warning that is always present is one nobody reads,
	// including on the day it means something. Eight times the largest excess ever
	// observed leaves normal cycles silent.
	ReconcileWarnAt int64 = 10_000_000
)

// TeamExcess is a team with excess points or unsafe member aggregates.
type TeamExcess struct {
	TeamID int32
	// Excess is the representable member score sum minus the authoritative total.
	// It need not be positive when Overflow is set, including WUs-only failures.
	Excess int64
	// Members is how many rows were summed, for the log line — one team going wrong
	// with three members reads very differently from one with thirty thousand.
	Members int
	// Overflow requires quarantine regardless of the normal score tolerance.
	Overflow bool
}

// Reconcile returns every team whose member rows sum past its authoritative total by
// more than warnAt, worst first, plus teams that make member aggregates unsafe.
// Callers must validate the authoritative team rows first. Member sums are checked
// here so a forged score or WU count cannot reject the whole cycle before isolation.
//
// A team named by a member row but absent from the team feed is reconciled against
// zero rather than skipped. Skipping it would leave the widest hole in the check: the
// forged team id is chosen by the attacker, so an id the team feed has never heard of
// is the cheapest possible evasion. Reconciling against zero costs the sixty real
// rows in that position nothing, because they are four orders of magnitude below the
// tolerance.
func Reconcile(teams []parse.TeamRow, users []parse.UserRow, warnAt int64) []TeamExcess {
	type totals struct{ score, wus int64 }
	authoritative := make(map[int32]totals, len(teams))
	for _, t := range teams {
		// Preserve reconciliation's treatment of duplicate team rows.
		a := authoritative[t.ID]
		a.score += t.Score
		a.wus += t.WUs
		authoritative[t.ID] = a
	}

	type sum struct {
		totals
		rows                       int
		scoreOverflow, wusOverflow bool
	}
	add := func(total *int64, value int64) bool {
		if value < 0 || *total > math.MaxInt64-value {
			return true
		}
		*total += value
		return false
	}
	var all sum
	claimed := make(map[int32]*sum, len(teams))
	for _, u := range users {
		s := claimed[u.TeamID]
		if s == nil {
			s = &sum{}
			claimed[u.TeamID] = s
		}
		s.scoreOverflow = s.scoreOverflow || add(&s.score, u.Score)
		s.wusOverflow = s.wusOverflow || add(&s.wus, u.WUs)
		all.scoreOverflow = all.scoreOverflow || add(&all.score, u.Score)
		all.wusOverflow = all.wusOverflow || add(&all.wus, u.WUs)
		s.rows++
	}

	var out []TeamExcess
	for id, s := range claimed {
		a := authoritative[id]
		// When a corpus total overflows, normal tolerances cannot apply to that
		// counter. Removing its positive-excess teams bounds the remaining total
		// by the already validated authoritative sum, even across different teams.
		overflow := s.scoreOverflow || s.wusOverflow ||
			(all.scoreOverflow && s.score > a.score) ||
			(all.wusOverflow && s.wus > a.wus)
		if excess := s.score - a.score; excess > warnAt || overflow {
			out = append(out, TeamExcess{TeamID: id, Excess: excess, Members: s.rows, Overflow: overflow})
		}
	}
	// Worst first, then by id, so a log line is stable between cycles and the most
	// serious team is the one a reader sees.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.Excess > b.Excess || (a.Excess == b.Excess && a.TeamID < b.TeamID)
	})
	return out
}

// DropTeams removes every user row belonging to one of the given teams.
//
// The whole team goes, not the largest row in it, because nothing distinguishes the
// forged record from a real one once both have parsed — the forgery is a well-formed
// row whose only crime is arithmetic its team cannot support.
//
// It drops rows rather than rejecting the snapshot, and that is the important part.
// Refusing a cycle outright would hand any participant a way to stop ingest for
// everybody by choosing a name: the service would fail closed on a feed it can never
// influence, and the hour would be unrecoverable because upstream overwrites it.
// Losing one team's member deltas for one cycle is self-healing instead — the totals
// are cumulative, so the next clean cycle carries the gap through — and the team's own
// authoritative row is untouched either way.
func DropTeams(users []parse.UserRow, teams []TeamExcess) []parse.UserRow {
	if len(teams) == 0 {
		return users
	}
	drop := make(map[int32]bool, len(teams))
	for _, t := range teams {
		drop[t.TeamID] = true
	}
	out := users[:0:0]
	for _, u := range users {
		if !drop[u.TeamID] {
			out = append(out, u)
		}
	}
	return out
}
