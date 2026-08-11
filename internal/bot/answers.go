package bot

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// The composed answers — rivals, head to head, movers, goals.
//
// These used to be routed through the MCP endpoint, which was the right first move:
// those tools already compose the multi-step questions and, more importantly, state
// their own assumptions. But an MCP tool writes for a model. It returns a wall of
// fixed-width prose, and a Discord embed can only carry that inside a code block —
// which does not wrap, scrolls sideways on a phone, and looks like debug output next
// to the embeds every other command produces.
//
// So they are composed here from the REST routes instead. The one thing that must not
// be lost in the move is the caveats: a projection that holds both sides at today's
// average forever is not a forecast, and a figure that travels without that sentence
// is a figure somebody will quote as one. Each answer below carries its assumption in
// the footer, beside the number rather than in documentation about it.

// gapLine renders one rival row: the distance, and when the crossing is projected.
func gapLine(pointsGap int64, days *float64) string {
	g := short(pointsGap)
	if days == nil || *days <= 0 {
		return "`" + g + "`"
	}
	return fmt.Sprintf("`%s` · %s", g, etaText(*days))
}

// etaText turns a projection in days into something worth reading.
//
// Rounded hard, and deliberately: a crossing "in 0.55 days" is arithmetic, not a
// prediction, and printing two decimals implies a precision that holding a seven-day
// average constant for a decade does not have.
func etaText(days float64) string {
	switch {
	case days < 1.0/24:
		return "any moment"
	case days < 1:
		return fmt.Sprintf("~%d h", int(math.Round(days*24)))
	case days < 14:
		return fmt.Sprintf("~%s", plural(int(math.Round(days)), "day"))
	case days < 60:
		return fmt.Sprintf("~%s", plural(int(math.Round(days/7)), "week"))
	case days < 730:
		return fmt.Sprintf("~%s", plural(int(math.Round(days/30.44)), "month"))
	}
	return fmt.Sprintf("~%s", plural(int(math.Round(days/365.25)), "year"))
}

/* --------------------------------------------------------------- rivals --- */

func (b *Bot) cmdRivals(ctx context.Context, kind, who string) (*discordgo.MessageEmbed, error) {
	id, err := b.resolveFor(ctx, kind, who)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return b.suggest(ctx, who)
	}
	r, snap, err := b.api.Rivals(ctx, kind, id)
	if err != nil {
		if NotFound(err) {
			return b.suggest(ctx, who)
		}
		return nil, err
	}

	// The endpoint returns a wide window — a hundred rows either side. A channel wants
	// the few that are actually in reach, so take the nearest handful in each direction.
	const show = 5
	var ahead, behind []Rival
	for _, v := range r.Rivals {
		switch {
		case v.Rank < r.Rank:
			ahead = append(ahead, v)
		case v.Rank > r.Rank:
			behind = append(behind, v)
		}
	}
	// Nearest first in both directions: ahead is ascending by rank, so take the tail.
	if len(ahead) > show {
		ahead = ahead[len(ahead)-show:]
	}
	if len(behind) > show {
		behind = behind[:show]
	}

	e := &discordgo.MessageEmbed{
		Title: fmt.Sprintf("%s — #%s", r.Name, n(r.Rank)),
		URL:   entityURL(kind, id),
		Color: colourNormal,
		Footer: &discordgo.MessageEmbedFooter{
			Text: footer(snap).Text + " · projections hold both sides at their rolling 24h rate",
		},
	}

	if len(ahead) > 0 {
		var sb strings.Builder
		// Reversed: the one they are closest to passing reads first.
		for i := len(ahead) - 1; i >= 0; i-- {
			v := ahead[i]
			fmt.Fprintf(&sb, "**#%s** %s\n%s\n", n(v.Rank), mdEsc(clip(v.Name, 34)),
				gapLine(v.PointsGap, v.OvertakeDays))
		}
		e.Fields = append(e.Fields, field("Catching", strings.TrimRight(sb.String(), "\n"), true))
	}
	if len(behind) > 0 {
		var sb strings.Builder
		for _, v := range behind {
			fmt.Fprintf(&sb, "**#%s** %s\n%s\n", n(v.Rank), mdEsc(clip(v.Name, 34)),
				gapLine(v.PointsGap, v.OvertakeDays))
		}
		e.Fields = append(e.Fields, field("Chasing you", strings.TrimRight(sb.String(), "\n"), true))
	}
	if len(e.Fields) == 0 {
		e.Description = "Nobody within reach in either direction."
	} else if !anyProjected(ahead) && !anyProjected(behind) {
		// Saying so beats printing a column of bare gaps and leaving the reader to
		// wonder why nothing has a date on it.
		e.Description = "No crossings projected — almost nobody nearby is producing right now."
	}
	return e, nil
}

func anyProjected(rs []Rival) bool {
	for _, r := range rs {
		if r.OvertakeDays != nil && *r.OvertakeDays > 0 {
			return true
		}
	}
	return false
}

/* -------------------------------------------------------------- compare --- */

func (b *Bot) cmdCompare(ctx context.Context, kind, a, bb string) (*discordgo.MessageEmbed, error) {
	ea, snap, err := b.lookup(ctx, kind, a)
	if err != nil {
		return nil, err
	}
	if ea == nil {
		return b.suggest(ctx, a)
	}
	eb, _, err := b.lookup(ctx, kind, bb)
	if err != nil {
		return nil, err
	}
	if eb == nil {
		return b.suggest(ctx, bb)
	}

	lead, trail := ea, eb
	if eb.PointsTotal > ea.PointsTotal {
		lead, trail = eb, ea
	}
	gap := lead.PointsTotal - trail.PointsTotal

	e := &discordgo.MessageEmbed{
		Title: fmt.Sprintf("%s vs %s", clip(ea.Name, 40), clip(eb.Name, 40)),
		Color: colourNormal,
		Fields: []*discordgo.MessageEmbedField{
			compareField(ea), compareField(eb),
			field("Gap", fmt.Sprintf("**%s**\n%s ahead", short(gap), mdEsc(clip(lead.Name, 30))), false),
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: footer(snap).Text + " · projection holds both at their rolling 24h rate",
		},
	}

	// The only projection worth making: the one behind is also the faster one, so the
	// gap is closing rather than widening.
	switch {
	case gap == 0:
		e.Description = "Level."
	case trail.PerDay > lead.PerDay:
		days := float64(gap) / float64(trail.PerDay-lead.PerDay)
		e.Description = fmt.Sprintf("**%s** is behind but gaining %s a day — level in %s at these rates.",
			mdEsc(clip(trail.Name, 30)), short(trail.PerDay-lead.PerDay), etaText(days))
	case lead.PerDay > trail.PerDay:
		e.Description = fmt.Sprintf("**%s** is ahead and pulling away by %s a day.",
			mdEsc(clip(lead.Name, 30)), short(lead.PerDay-trail.PerDay))
	default:
		e.Description = "Neither is producing, so the gap is not moving."
	}
	return e, nil
}

func compareField(e *entity) *discordgo.MessageEmbedField {
	return field(clip(e.Name, 40), fmt.Sprintf(
		"Rank **#%s**\n**%s** points\n**%s**/day\n%s WUs",
		n(e.Rank), short(e.PointsTotal), short(e.PerDay), short(e.WUs)), true)
}

/* --------------------------------------------------------------- movers --- */

// cmdMovers ranks by 24-hour movement, which no leaderboard sort exposes.
//
// The server's own tool scans the top thousand for non-zero changes, so this pulls the
// same thousand rows in one request and sorts them here. Below that depth the numbers
// stop meaning anything: a donor at rank 400,000 can move a thousand places on one
// work unit, and an unbounded list is a readout of who happened to submit.
const moversWithin = 1000

func (b *Bot) cmdMovers(ctx context.Context, kind, direction string) (*discordgo.MessageEmbed, error) {
	type mover struct {
		name   string
		rank   int64
		change int64
		rate   int64
	}
	var all []mover
	var snap Snapshot
	var field0 string

	if kind == "teams" {
		ts, s, err := b.api.TopTeams(ctx, "lifetime", moversWithin)
		if err != nil {
			return nil, err
		}
		snap, field0 = s, "teams"
		for _, t := range ts {
			if t.RankChange24h != 0 {
				all = append(all, mover{t.Name, t.Rank, t.RankChange24h, t.PointsPerDay})
			}
		}
	} else {
		ds, s, err := b.api.TopDonors(ctx, "lifetime", moversWithin)
		if err != nil {
			return nil, err
		}
		snap, field0 = s, "donors"
		for _, d := range ds {
			if d.RankChange24h != 0 {
				all = append(all, mover{d.Name, d.Rank, d.RankChange24h, d.PointsPerDay})
			}
		}
	}

	up := make([]mover, 0, len(all))
	down := make([]mover, 0, len(all))
	for _, m := range all {
		if m.change > 0 {
			up = append(up, m)
		} else {
			down = append(down, m)
		}
	}
	sort.Slice(up, func(i, j int) bool { return up[i].change > up[j].change })
	sort.Slice(down, func(i, j int) bool { return down[i].change < down[j].change })

	const show = 6
	line := func(ms []mover, sign string) string {
		if len(ms) == 0 {
			return "Nobody"
		}
		var sb strings.Builder
		for i, m := range ms {
			if i == show {
				break
			}
			c := m.change
			if c < 0 {
				c = -c
			}
			fmt.Fprintf(&sb, "`%s%d` **#%s** %s\n", sign, c, n(m.rank), mdEsc(clip(m.name, 28)))
		}
		return strings.TrimRight(sb.String(), "\n")
	}

	e := &discordgo.MessageEmbed{
		Title: "Biggest 24-hour movements",
		URL:   SiteURL + "/" + field0,
		Color: colourNormal,
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("%s · within the top %s by lifetime points", footer(snap).Text, n(moversWithin)),
		},
	}
	if direction != "down" {
		e.Fields = append(e.Fields, field("Climbed", line(up, "+"), true))
	}
	if direction != "up" {
		e.Fields = append(e.Fields, field("Fell", line(down, "−"), true))
	}
	if len(all) == 0 {
		e.Description = "Nothing moved in the top " + n(moversWithin) + " in the last 24 hours."
	}
	return e, nil
}

/* ----------------------------------------------------------------- goal --- */

func (b *Bot) cmdGoal(ctx context.Context, kind, who string, target int64) (*discordgo.MessageEmbed, error) {
	me, snap, err := b.lookup(ctx, kind, who)
	if err != nil {
		return nil, err
	}
	if me == nil {
		return b.suggest(ctx, who)
	}
	if target < 1 {
		return ErrorEmbed("Pick a rank of 1 or better."), nil
	}
	if me.Rank == target {
		return &discordgo.MessageEmbed{
			Title: fmt.Sprintf("%s is already #%s", me.Name, n(target)),
			Color: colourGood, URL: entityURL(kind, who),
		}, nil
	}
	if me.Rank < target {
		return &discordgo.MessageEmbed{
			Title: fmt.Sprintf("%s is already past #%s", me.Name, n(target)),
			Color: colourGood, URL: entityURL(kind, who),
			Description: fmt.Sprintf("Currently **#%s**, which is %s better than the target.",
				n(me.Rank), plural(int(target-me.Rank), "place")),
		}, nil
	}

	// Whoever holds the target rank is the bar to clear.
	holder, err := b.atRank(ctx, kind, target)
	if err != nil {
		return nil, err
	}
	if holder == nil {
		return ErrorEmbed(fmt.Sprintf("Rank #%s is beyond the leaderboard.", n(target))), nil
	}
	gap := holder.PointsTotal - me.PointsTotal
	if gap < 0 {
		gap = 0
	}

	e := &discordgo.MessageEmbed{
		Title: fmt.Sprintf("%s → #%s", clip(me.Name, 40), n(target)),
		URL:   entityURL(kind, who),
		Color: colourNormal,
		Fields: []*discordgo.MessageEmbedField{
			field("Now", fmt.Sprintf("**#%s**\n%s points", n(me.Rank), short(me.PointsTotal)), true),
			field("To beat", fmt.Sprintf("**%s**\n%s points", mdEsc(clip(holder.Name, 24)), short(holder.PointsTotal)), true),
			field("Gap", fmt.Sprintf("**%s**\n%s", short(gap), n(gap)), true),
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: footer(snap).Text + " · assumes the target stays where it is",
		},
	}

	switch {
	case me.PerDay <= 0:
		e.Description = fmt.Sprintf(
			"**%s** is not producing right now, so at this rate the gap never closes. "+
				"At **1M/day** it would take %s.", clip(me.Name, 30), etaText(float64(gap)/1e6))
		e.Color = colourWarn
	default:
		days := float64(gap) / float64(me.PerDay)
		e.Description = fmt.Sprintf(
			"At **%s/day** that is %s — around %s.",
			short(me.PerDay), etaText(days), dateText(snap.ServerTime, days))
		// The honest caveat: the holder is a moving target, and if they are faster this
		// is not a forecast at all.
		if holder.PointsPerDay > me.PerDay {
			e.Description += fmt.Sprintf(
				"\n\nBut they are producing **%s/day** to your **%s/day**, so the gap is widening, "+
					"not closing.", short(holder.PointsPerDay), short(me.PerDay))
			e.Color = colourWarn
		}
	}
	return e, nil
}

func dateText(from time.Time, days float64) string {
	if from.IsZero() {
		from = time.Now().UTC()
	}
	if days > 3650 {
		return "not this decade"
	}
	return from.AddDate(0, 0, int(math.Round(days))).UTC().Format("2 Jan 2006")
}

/* --------------------------------------------------------------- shared --- */

// lookup resolves either kind into the shared entity shape.
func (b *Bot) lookup(ctx context.Context, kind, who string) (*entity, Snapshot, error) {
	id, err := b.resolveFor(ctx, kind, who)
	if err != nil || id == "" {
		return nil, Snapshot{}, err
	}
	if kind == "teams" {
		n64, convErr := strconv.ParseInt(id, 10, 64)
		if convErr != nil {
			return nil, Snapshot{}, nil
		}
		t, snap, err := b.api.Team(ctx, n64)
		if err != nil {
			return nil, snap, err
		}
		return &entity{Name: t.Name, Rank: t.Rank, PointsTotal: t.PointsTotal,
			Last24h: t.PointsLast24h, PerDay: t.PointsPerDay, WUs: t.WUsTotal}, snap, nil
	}
	d, snap, err := b.api.Donor(ctx, id)
	if err != nil {
		return nil, snap, err
	}
	return &entity{Name: d.Name, Rank: d.Rank, PointsTotal: d.PointsTotal,
		Last24h: d.PointsLast24h, PerDay: d.PointsPerDay, WUs: d.WUsTotal}, snap, nil
}

// resolveFor turns what somebody typed or picked into the key the routes want.
//
// Autocomplete sends a team id and an exact donor name, so the common path is a
// no-op. The rest is for people who type: a bare number is a team, and anything else
// gets one search before giving up.
func (b *Bot) resolveFor(ctx context.Context, kind, who string) (string, error) {
	who = strings.TrimSpace(who)
	if who == "" {
		return "", nil
	}
	if kind == "teams" {
		if _, convErr := strconv.ParseInt(who, 10, 64); convErr == nil {
			return who, nil
		}
		res, _, err := b.api.Search(ctx, who, 5)
		if err != nil {
			return "", err
		}
		if len(res.Teams) == 0 {
			return "", nil
		}
		return strconv.FormatInt(res.Teams[0].TeamID, 10), nil
	}
	if _, _, err := b.api.Donor(ctx, who); err == nil {
		return who, nil
	} else if !NotFound(err) {
		return "", err
	}
	res, _, err := b.api.Search(ctx, who, 5)
	if err != nil {
		return "", err
	}
	if len(res.Donors) == 0 {
		return "", nil
	}
	return res.Donors[0].Name, nil
}

// atRank finds whoever currently holds a place.
type rankHolder struct {
	Name          string
	PointsTotal   int64
	PointsPerDay  int64
	RankChange24h int64
}

func (b *Bot) atRank(ctx context.Context, kind string, rank int64) (*rankHolder, error) {
	// One page containing the rank rather than the whole board: page size is the rank
	// itself capped at the API's limit, so a target of #5 costs five rows.
	per := int(rank)
	if per > 1000 {
		per = 1000
	}
	if rank > 1000 {
		return nil, nil
	}
	if kind == "teams" {
		ts, _, err := b.api.TopTeams(ctx, "lifetime", per)
		if err != nil {
			return nil, err
		}
		if int(rank) > len(ts) {
			return nil, nil
		}
		t := ts[rank-1]
		return &rankHolder{t.Name, t.PointsTotal, t.PointsPerDay, t.RankChange24h}, nil
	}
	ds, _, err := b.api.TopDonors(ctx, "lifetime", per)
	if err != nil {
		return nil, err
	}
	if int(rank) > len(ds) {
		return nil, nil
	}
	d := ds[rank-1]
	return &rankHolder{d.Name, d.PointsTotal, d.PointsPerDay, d.RankChange24h}, nil
}

func entityURL(kind, id string) string {
	if kind == "teams" {
		return SiteURL + "/teams/" + esc(id)
	}
	return SiteURL + "/donors/" + esc(id)
}
