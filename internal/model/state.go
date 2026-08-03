// Package model holds the in-memory representation of the stats corpus and the
// differencing engine that derives every rate and rank from successive snapshots.
//
// The upstream feeds carry only cumulative lifetime totals, so a snapshot alone
// says nothing about production. Everything the site shows — points this hour, this
// day, the 7-day average, rank movement — is the difference between two snapshots.
// That makes this package the correctness core of the backend.
package model

import (
	"time"

	"folding/internal/parse"
)

// MemberID identifies a (name, team) pair, the grain of the upstream user feed.
// It is not a person: the same donor name appears on many teams, and the same name
// can even repeat within one team.
type MemberID int32

// Member is the current cumulative state of one (name, team) pair.
type Member struct {
	NameID int32
	TeamID int32
	Score  int64
	WUs    int64
}

// Team is the current cumulative state of one team, taken from the team feed.
type Team struct {
	ID     int32
	NameID int32
	Score  int64
	WUs    int64
}

// Delta is one entity's production between two snapshots. Only non-zero deltas are
// produced: at any moment the overwhelming majority of donors are idle, and that
// sparsity is what keeps both the history table and the sliding windows small.
type Delta struct {
	ID     int32
	DScore int64
	DWUs   int64
}

// Cycle is the outcome of applying one snapshot pair.
type Cycle struct {
	At time.Time

	MemberDeltas []Delta
	TeamDeltas   []Delta

	// NewMembers and NewTeams are entities seen for the first time. Their initial
	// score is *not* production and must never be counted as a delta — it is a
	// lifetime total accumulated before we started observing.
	NewMembers []int32
	NewTeams   []int32

	// Regressions counts entities whose cumulative score decreased. That should be
	// impossible; it indicates a feed glitch or a renamed donor colliding with an
	// existing row, and is surfaced rather than silently clamped.
	Regressions int
}

// State is the corpus as of the most recent applied snapshot.
type State struct {
	Names *NameArena

	Members []Member
	Teams   []Team

	// memberIdx maps (nameID, teamID) to a slot in Members.
	memberIdx map[uint64]int32
	// teamIdx maps the upstream team number to a slot in Teams.
	teamIdx map[int32]int32

	// Scratch buffers reused across cycles. Accumulating a snapshot in maps keyed
	// by slot costs ~125 MB per map at corpus scale and is re-allocated every hour;
	// slots are dense, so plain slices indexed by slot do the same job for a fifth
	// of the memory and no allocation in steady state.
	rowSlots []int32
	accScore []int64
	accWUs   []int64
	touched  []bool
	seen     []int32

	At time.Time
}

// NewState returns an empty State sized for the current corpus.
func NewState() *State {
	return &State{
		Names:     NewNameArena(2_200_000),
		Members:   make([]Member, 0, 2_800_000),
		Teams:     make([]Team, 0, 140_000),
		memberIdx: make(map[uint64]int32, 2_800_000),
		teamIdx:   make(map[int32]int32, 140_000),
	}
}

func memberKey(nameID, teamID int32) uint64 {
	return uint64(uint32(nameID))<<32 | uint64(uint32(teamID))
}

// MemberSlot returns the slot for a (name, team) pair.
func (s *State) MemberSlot(nameID, teamID int32) (int32, bool) {
	i, ok := s.memberIdx[memberKey(nameID, teamID)]
	return i, ok
}

// TeamSlot returns the slot for an upstream team number.
func (s *State) TeamSlot(teamID int32) (int32, bool) {
	i, ok := s.teamIdx[teamID]
	return i, ok
}

// Apply folds a parsed snapshot pair into the state and returns the deltas.
//
// Rows are accumulated before differencing because duplicate (name, team) pairs are
// real — 6,984 of them in the reference corpus — and summing is what reconciles
// against the authoritative team totals. Deduplicating instead would silently drop
// production.
func (s *State) Apply(at time.Time, teams []parse.TeamRow, users []parse.UserRow) *Cycle {
	c := &Cycle{At: at}

	// Two passes. The first resolves every row to a slot, which is also what
	// finalises len(s.Members); the second accumulates into slices sized to that,
	// summing duplicate (name, team) rows.
	s.rowSlots = grow32(s.rowSlots, len(users))
	for i, u := range users {
		nameID := s.Names.Intern(u.Name)
		slot, ok := s.MemberSlot(nameID, u.TeamID)
		if !ok {
			slot = int32(len(s.Members))
			s.Members = append(s.Members, Member{NameID: nameID, TeamID: u.TeamID})
			s.memberIdx[memberKey(nameID, u.TeamID)] = slot
			c.NewMembers = append(c.NewMembers, slot)
		}
		s.rowSlots[i] = slot
	}

	n := len(s.Members)
	s.accScore = grow64(s.accScore, n)
	s.accWUs = grow64(s.accWUs, n)
	s.touched = growBool(s.touched, n)
	s.seen = s.seen[:0]

	for i, u := range users {
		slot := s.rowSlots[i]
		if !s.touched[slot] {
			s.touched[slot] = true
			s.seen = append(s.seen, slot)
		}
		s.accScore[slot] += u.Score
		s.accWUs[slot] += u.WUs
	}

	for _, slot := range s.seen {
		m := &s.Members[slot]
		score, wus := s.accScore[slot], s.accWUs[slot]
		// Reset in place so the buffers are clean for the next cycle without
		// zeroing all 2.5M entries.
		s.accScore[slot], s.accWUs[slot], s.touched[slot] = 0, 0, false
		d, dw := score-m.Score, wus-m.WUs
		if d < 0 {
			c.Regressions++
		}
		// A member's first sighting carries its entire pre-existing lifetime total.
		// Recording that as production would invent an enormous fake spike on the
		// day we happened to start watching.
		if m.Score != 0 || m.WUs != 0 {
			if d != 0 || dw != 0 {
				c.MemberDeltas = append(c.MemberDeltas, Delta{ID: slot, DScore: d, DWUs: dw})
			}
		}
		m.Score, m.WUs = score, wus
	}

	for _, t := range teams {
		nameID := s.Names.Intern(t.Name)
		slot, ok := s.TeamSlot(t.ID)
		if !ok {
			slot = int32(len(s.Teams))
			s.Teams = append(s.Teams, Team{ID: t.ID, NameID: nameID})
			s.teamIdx[t.ID] = slot
			c.NewTeams = append(c.NewTeams, slot)
		}
		tm := &s.Teams[slot]
		d, dw := t.Score-tm.Score, t.WUs-tm.WUs
		if d < 0 {
			c.Regressions++
		}
		if tm.Score != 0 || tm.WUs != 0 {
			if d != 0 || dw != 0 {
				c.TeamDeltas = append(c.TeamDeltas, Delta{ID: slot, DScore: d, DWUs: dw})
			}
		}
		// Teams can be renamed upstream; keep the latest.
		tm.NameID = nameID
		tm.Score, tm.WUs = t.Score, t.WUs
	}

	s.At = at
	return c
}

// grow returns a slice of exactly n elements, reusing the backing array when it is
// already large enough. Reused buffers are always fully overwritten or reset by the
// caller, so stale contents cannot leak between cycles.
func grow32(s []int32, n int) []int32 {
	if cap(s) >= n {
		return s[:n]
	}
	return make([]int32, n)
}

func grow64(s []int64, n int) []int64 {
	if cap(s) >= n {
		return s[:n]
	}
	out := make([]int64, n)
	copy(out, s)
	return out
}

func growBool(s []bool, n int) []bool {
	if cap(s) >= n {
		return s[:n]
	}
	out := make([]bool, n)
	copy(out, s)
	return out
}

// AppendTeam registers a team at the next slot without treating it as new
// production. Used when rebuilding state from persisted identity: slots must be
// reassigned in exactly the order they were first assigned, because stored deltas
// reference them by number.
func (s *State) AppendTeam(teamID, nameID int32) int32 {
	slot := int32(len(s.Teams))
	s.Teams = append(s.Teams, Team{ID: teamID, NameID: nameID})
	s.teamIdx[teamID] = slot
	return slot
}

// AppendMember registers a (name, team) pair at the next slot. See AppendTeam.
func (s *State) AppendMember(nameID, teamID int32) int32 {
	slot := int32(len(s.Members))
	s.Members = append(s.Members, Member{NameID: nameID, TeamID: teamID})
	s.memberIdx[memberKey(nameID, teamID)] = slot
	return slot
}

// ResetTotals zeroes every cumulative total while keeping identity intact.
//
// Replaying history from the beginning needs the id assignments (so stored deltas
// still point at the right donors) but not the current scores — which would make
// the first replayed cycle produce a large negative delta against them. Resuming
// forward from the newest cycle wants the opposite, which is why restoring totals
// and clearing them are separate operations.
func (s *State) ResetTotals() {
	for i := range s.Members {
		s.Members[i].Score, s.Members[i].WUs = 0, 0
	}
	for i := range s.Teams {
		s.Teams[i].Score, s.Teams[i].WUs = 0, 0
	}
	s.At = time.Time{}
}
