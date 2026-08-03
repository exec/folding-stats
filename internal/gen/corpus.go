package gen

import (
	"fmt"
	"io"
	"math"
	"math/rand"
	"strings"
)

func io_Copy(dst io.Writer, src io.Reader) (int64, error) { return io.Copy(dst, src) }

// Config describes the shape of a synthetic corpus.
type Config struct {
	Members int // (name, team) pairs
	Teams   int
	Seed    int64

	// ActiveFraction is the share of members producing in a given cycle. Measured
	// upstream: 1,149 of 2,710,067 in one hour, or 0.042%. This is the single most
	// important parameter — the entire storage and sliding-window design rests on
	// almost everyone being idle almost always.
	ActiveFraction float64

	// MultiTeamFraction is the share of donor names appearing on more than one
	// team. Measured: 15.16%.
	MultiTeamFraction float64

	// PseudoIdentities is how many names are shared by a very large number of
	// teams, in the manner of "PS3" (10,426 teams) or "Anonymous" (5,993).
	PseudoIdentities int
	// PseudoIdentityTeams is how many teams each of those spans, at most.
	PseudoIdentityTeams int
}

// DefaultConfig is a scaled-down corpus with the real one's proportions intact.
// Scaling members and teams together keeps team sizes, the multi-team share and the
// active fraction all faithful.
func DefaultConfig() Config {
	return Config{
		Members:             200_000,
		Teams:               10_000,
		Seed:                1,
		ActiveFraction:      0.00042,
		MultiTeamFraction:   0.1516,
		PseudoIdentities:    3,
		PseudoIdentityTeams: 800,
	}
}

// Scale returns cfg resized to n members, holding every proportion constant.
func (c Config) Scale(n int) Config {
	if c.Members == 0 {
		return c
	}
	ratio := float64(n) / float64(c.Members)
	out := c
	out.Members = n
	out.Teams = int(float64(c.Teams) * ratio)
	if out.Teams < 1 {
		out.Teams = 1
	}
	out.PseudoIdentityTeams = int(float64(c.PseudoIdentityTeams) * ratio)
	if out.PseudoIdentityTeams > out.Teams {
		out.PseudoIdentityTeams = out.Teams
	}
	return out
}

// Member is one synthetic (name, team) pair with its running totals.
type Member struct {
	Name   string
	TeamID int32
	Score  int64
	WUs    int64
	// pseudo marks a shared placeholder name. Those hold little individually — a
	// console default is one idle machine, not a dedicated folder — even though
	// their aggregate across thousands of teams is large.
	pseudo bool
	// rate is this member's points per producing cycle. Output is wildly unequal
	// upstream — the top donor holds 3.85 trillion points while the median holds
	// almost none — so rates are drawn from a heavy-tailed distribution rather than
	// a uniform one.
	rate int64
}

// Corpus is a generated world that can be advanced cycle by cycle.
type Corpus struct {
	Members []Member
	Teams   []Team

	cfg Config
	rng *rand.Rand
	// teamIndex maps an upstream team number to its slot in Teams.
	teamIndex map[int32]int
}

// Team is one synthetic team with its running totals.
type Team struct {
	ID    int32
	Name  string
	Score int64
	WUs   int64
}

// New builds a corpus. Names are drawn from words, combined and suffixed to reach
// the required variety without collapsing into a single naming style.
func New(cfg Config, words []string) (*Corpus, error) {
	if len(words) < 1000 {
		return nil, fmt.Errorf("gen: need at least 1000 words, got %d", len(words))
	}
	rng := rand.New(rand.NewSource(cfg.Seed))
	c := &Corpus{cfg: cfg, rng: rng, teamIndex: make(map[int32]int, cfg.Teams)}

	// Team 0 is the "no team specified" bucket upstream and holds far more members
	// than any real team — a third of the corpus. Reproducing that matters: it is
	// the pathological case for per-team rosters and member listings.
	c.Teams = make([]Team, 0, cfg.Teams)
	c.addTeam(0, "Default (No team specified)")
	for i := 1; i < cfg.Teams; i++ {
		// Upstream team numbers are sparse and reach ~1.3M, which is what makes a
		// dense lookup array a real (if modest) memory decision.
		id := int32(i*13 + rng.Intn(11) + 1)
		c.addTeam(id, teamName(rng, words))
	}

	c.Members = make([]Member, 0, cfg.Members)
	names := make([]string, 0, cfg.Members)

	// Pseudo-identities first, so they are present regardless of how the rest
	// happens to fall.
	for p := 0; p < cfg.PseudoIdentities; p++ {
		name := []string{"PS3", "Anonymous", "Default"}[p%3]
		if p >= 3 {
			name = fmt.Sprintf("%s%d", name, p)
		}
		n := cfg.PseudoIdentityTeams
		if n > len(c.Teams) {
			n = len(c.Teams)
		}
		for t := 0; t < n && len(c.Members) < cfg.Members; t++ {
			c.addMemberAs(name, c.Teams[t].ID, rng, true)
		}
	}

	for len(c.Members) < cfg.Members {
		name := donorName(rng, words)
		names = append(names, name)

		teams := 1
		if rng.Float64() < cfg.MultiTeamFraction {
			// Multi-team donors cluster at 2-3 teams; a long tail reaches further.
			teams = 2 + rng.Intn(3)
			if rng.Float64() < 0.02 {
				teams = 5 + rng.Intn(20)
			}
		}
		for t := 0; t < teams && len(c.Members) < cfg.Members; t++ {
			c.addMember(name, c.pickTeam(rng), rng)
		}
	}

	c.seedTotals()
	return c, nil
}

func (c *Corpus) addTeam(id int32, name string) {
	c.teamIndex[id] = len(c.Teams)
	c.Teams = append(c.Teams, Team{ID: id, Name: name})
}

func (c *Corpus) addMember(name string, teamID int32, rng *rand.Rand) {
	c.addMemberAs(name, teamID, rng, false)
}

func (c *Corpus) addMemberAs(name string, teamID int32, rng *rand.Rand, pseudo bool) {
	c.Members = append(c.Members, Member{
		Name: name, TeamID: teamID, rate: rateFor(rng), pseudo: pseudo,
	})
}

// pickTeam favours team 0 heavily and otherwise skews toward low-numbered teams, so
// team sizes are unequal in the way real ones are.
func (c *Corpus) pickTeam(rng *rand.Rand) int32 {
	if rng.Float64() < 0.33 {
		return 0
	}
	// Square the uniform draw to bias toward the front of the list.
	f := rng.Float64()
	idx := int(f * f * float64(len(c.Teams)))
	if idx >= len(c.Teams) {
		idx = len(c.Teams) - 1
	}
	return c.Teams[idx].ID
}

// rateFor draws a per-cycle production rate.
//
// Calibrated against the measured hourly figure: 705,867,186 points across ~1,149
// producing members, so an active member averages roughly 600k points in the hour it
// produces. The tail is wide — a few contributors dwarf everyone else.
func rateFor(rng *rand.Rand) int64 {
	switch f := rng.Float64(); {
	case f < 0.70:
		return int64(1 + rng.Intn(100_000))
	case f < 0.93:
		return int64(100_000 + rng.Intn(1_000_000))
	case f < 0.995:
		return int64(1_000_000 + rng.Intn(10_000_000))
	default:
		return int64(10_000_000 + rng.Intn(90_000_000))
	}
}

// lifetimeScore draws a cumulative total by position in the corpus.
//
// Lifetime points are far more skewed than production: in the real corpus the top 25
// donors hold ~16% of all 52 trillion points, while the median holds almost nothing.
// Rank-banded log-uniform draws reproduce that without the mean being swallowed by
// the tail, which is what a naive per-member distribution does.
func lifetimeScore(rng *rand.Rand, i, n int) int64 {
	frac := float64(i) / float64(n)
	var lo, hi float64
	switch {
	case frac < 0.00004: // ~100 of 2.71M: the household names
		lo, hi = 1e11, 4e12
	case frac < 0.004: // ~10k
		lo, hi = 1e8, 1e10
	case frac < 0.08: // ~200k
		lo, hi = 1e6, 1e8
	default: // the long quiet majority
		lo, hi = 0, 2e7
	}
	if lo == 0 {
		return int64(rng.Float64() * hi)
	}
	// Log-uniform within the band, so a band spanning four orders of magnitude is
	// not dominated by its top decade.
	return int64(lo * math.Pow(hi/lo, rng.Float64()))
}

// seedTotals gives every entity a plausible lifetime history, so the first generated
// cycle is not a world where everybody starts at zero.
func (c *Corpus) seedTotals() {
	// Position in the slice is already random with respect to name and team, so it
	// serves directly as the rank band.
	for i := range c.Members {
		m := &c.Members[i]
		if m.pseudo {
			// Deliberately kept out of the rank bands: these sit at the front of the
			// slice, and letting them inherit the top band would make one shared
			// placeholder outrank the entire real corpus.
			m.Score = int64(c.rng.Float64() * 5e6)
		} else {
			m.Score = lifetimeScore(c.rng, i, len(c.Members))
		}
		// Points per work unit varies widely by hardware and project.
		m.WUs = m.Score / int64(20_000+c.rng.Intn(80_000))
	}
	c.recomputeTeams()
}

// recomputeTeams sums member totals into their teams. Upstream team totals slightly
// exceed the sum of listed members because the two feeds publish a minute apart; that
// skew is added when the feeds are written, not here.
func (c *Corpus) recomputeTeams() {
	for i := range c.Teams {
		c.Teams[i].Score, c.Teams[i].WUs = 0, 0
	}
	for _, m := range c.Members {
		if idx, ok := c.teamIndex[m.TeamID]; ok {
			c.Teams[idx].Score += m.Score
			c.Teams[idx].WUs += m.WUs
		}
	}
}

// Advance simulates one publish cycle: a small random subset of members produces.
// Returns how many members moved.
func (c *Corpus) Advance() int {
	active := int(float64(len(c.Members)) * c.cfg.ActiveFraction)
	if active < 1 {
		active = 1
	}
	moved := 0
	for i := 0; i < active; i++ {
		m := &c.Members[c.rng.Intn(len(c.Members))]
		// Vary the increment so consecutive cycles are not identical, which would
		// make rate windows suspiciously smooth.
		gain := m.rate/2 + c.rng.Int63n(m.rate+1)
		m.Score += gain
		m.WUs += 1 + c.rng.Int63n(4)
		if idx, ok := c.teamIndex[m.TeamID]; ok {
			c.Teams[idx].Score += gain
			c.Teams[idx].WUs += 1
		}
		moved++
	}
	return moved
}

func donorName(rng *rand.Rand, words []string) string {
	w := func() string { return words[rng.Intn(len(words))] }
	switch f := rng.Float64(); {
	case f < 0.30:
		return w()
	case f < 0.55:
		return w() + "_" + w()
	case f < 0.70:
		return capitalize(w()) + capitalize(w())
	case f < 0.85:
		return w() + fmt.Sprint(rng.Intn(10000))
	case f < 0.92:
		return fmt.Sprint(rng.Intn(100000000)) // purely numeric names exist upstream
	case f < 0.97:
		// BOINC/Gridcoin-style CPID suffixes: long, opaque, and a real stress on
		// the name arena.
		return w() + "_ALL_" + randHex(rng, 32)
	case f < 0.99:
		return strings.ToUpper(w()[:2]) // short names, which EOC cannot even search
	default:
		// Names carrying literal tabs and newlines occur upstream and must survive
		// the round trip through the feed format.
		return w() + string([]byte{"\t\n"[rng.Intn(2)]}) + w()
	}
}

func teamName(rng *rand.Rand, words []string) string {
	w := func() string { return words[rng.Intn(len(words))] }
	switch f := rng.Float64(); {
	case f < 0.35:
		return capitalize(w()) + " " + capitalize(w())
	case f < 0.55:
		return "Team " + capitalize(w())
	case f < 0.70:
		return w() + "." + []string{"com", "net", "org", "de", "ru"}[rng.Intn(5)]
	case f < 0.85:
		return capitalize(w()) + ", " + capitalize(w()) + " & " + capitalize(w())
	case f < 0.95:
		return "[" + strings.ToUpper(w()[:3]) + "] " + capitalize(w())
	default:
		return capitalize(w()) + "  -  " + capitalize(w()) // doubled spaces occur upstream
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func randHex(rng *rand.Rand, n int) string {
	const hex = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = hex[rng.Intn(16)]
	}
	return string(b)
}
