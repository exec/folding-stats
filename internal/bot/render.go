package bot

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// SiteURL is the public site, used only for links in embeds. The bot itself talks to
// the private address; a reader clicking through needs the name they can reach.
var SiteURL = "https://folding.exec.codes"

const (
	colourNormal = 0x3987e5
	colourWarn   = 0xfab219
	colourBad    = 0xd95926
)

// n formats a count with thousands separators.
func n(v int64) string {
	s := fmt.Sprintf("%d", v)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// short renders large point totals the way a leaderboard does.
func short(v int64) string {
	f := float64(v)
	switch {
	case v >= 1e12:
		return trim(f/1e12) + "T"
	case v >= 1e9:
		return trim(f/1e9) + "B"
	case v >= 1e6:
		return trim(f/1e6) + "M"
	case v >= 1e3:
		return trim(f/1e3) + "K"
	}
	return fmt.Sprintf("%d", v)
}

func trim(f float64) string {
	s := fmt.Sprintf("%.2f", f)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

// movement renders a 24-hour rank change.
//
// Positive means improved, which is a smaller rank number — the arrow has to follow
// the meaning rather than the arithmetic, or every reader reads it backwards once.
func movement(change int64) string {
	switch {
	case change > 0:
		return fmt.Sprintf("▲ %d in 24h", change)
	case change < 0:
		return fmt.Sprintf("▼ %d in 24h", -change)
	}
	return "unchanged in 24h"
}

// footer states the age of the data on every answer.
//
// A number posted in a channel outlives the moment it was asked for: somebody scrolls
// back tomorrow and reads it as current. The site can rely on a live countdown in the
// header; a message cannot, so it has to carry its own timestamp.
func footer(s Snapshot) *discordgo.MessageEmbedFooter {
	txt := fmt.Sprintf("data from %s UTC (%s old)", s.At.UTC().Format("15:04"), since(s.At))
	if s.Stale {
		txt += " — upstream update is late"
	}
	if s.WarmingUp != nil && s.WarmingUp.HistorySpanSec > 0 {
		d := time.Duration(s.WarmingUp.HistorySpanSec) * time.Second
		txt += fmt.Sprintf(" — only %s of history collected, so rates read low", humanDur(d))
	}
	return &discordgo.MessageEmbedFooter{Text: txt}
}

func since(t time.Time) string { return humanDur(time.Since(t)) }

// humanDur says a duration the way a person would.
//
// time.Duration's own String is built for logs, and printing it in a reply leaks that
// straight to the reader: "32m0s old", "only 91h0m0s of history collected". Days keep
// a decimal below ten because the figures they caveat are about how much history
// exists, where "3 days" for 91 hours is a visible undercount.
func humanDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		if m := int(d.Minutes()) % 60; m != 0 {
			return fmt.Sprintf("%dh %dm", int(d.Hours()), m)
		}
		return plural(int(d.Hours()), "hour")
	case d < 10*24*time.Hour:
		return trim1(d.Hours()/24) + " days"
	}
	return plural(int(d.Hours()/24+0.5), "day")
}

func plural(v int, unit string) string {
	if v == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", v, unit)
}

func trim1(f float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", f), ".0")
}

// mdEsc defuses the markdown in names people chose themselves.
//
// Team names are arbitrary user text, and Discord renders markdown everywhere: a team
// called **WINNERS** would come out bold and one containing a backtick would tear open
// the code span around the rate beside it, spilling the formatting into the rest of
// the list.
func mdEsc(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("*_`~|\\>[]()", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// clip shortens a name to fit without letting one long entry eat the field.
func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimRight(string(r[:max-1]), " ") + "…"
}

func field(name, value string, inline bool) *discordgo.MessageEmbedField {
	return &discordgo.MessageEmbedField{Name: name, Value: value, Inline: inline}
}

// streakText renders a daily-production run, and refuses to overstate one.
//
// A run that reaches the first day on record is a floor, not a measurement: this
// service has been collecting for days, so somebody who has folded every day for a
// decade would otherwise be credited with the age of the site. The website and the
// MCP tools both say so; a number in a channel gets read by more people than either.
func streakText(s *Streak) string {
	if s == nil || s.Current == 0 {
		return ""
	}
	if s.AtCollectionFloor {
		return fmt.Sprintf("**%s**\nevery day on record — at least", plural(s.Current, "day"))
	}
	return fmt.Sprintf("**%s**\nbest %s", plural(s.Current, "day"), plural(s.Longest, "day"))
}

// teamList renders the teams a donor folds for.
//
// Ordered by lifetime points it answers the wrong question. Someone who has joined
// twenty-five teams over the years gets a list led by four rows reading `0/day` —
// where they have *ever* folded, when the field is headed "Folding for". So the teams
// actually producing come first and the dormant ones collapse into a count, and if
// nothing is producing at all the lifetime totals are the only story left to tell.
func teamList(ts []Membership) string {
	var live, idle []Membership
	for _, t := range ts {
		if t.PointsPerDay > 0 {
			live = append(live, t)
		} else {
			idle = append(idle, t)
		}
	}
	sort.SliceStable(live, func(i, j int) bool { return live[i].PointsPerDay > live[j].PointsPerDay })

	shown, rate := live, true
	if len(shown) == 0 {
		shown, idle, rate = idle, nil, false
	}

	const max = 5
	var b strings.Builder
	for i, t := range shown {
		if i == max {
			break
		}
		if rate {
			fmt.Fprintf(&b, "`%s/day` ", short(t.PointsPerDay))
		} else {
			fmt.Fprintf(&b, "`%s` ", short(t.PointsTotal))
		}
		b.WriteString(mdEsc(clip(t.TeamName, 44)))
		if t.RankInTeam > 0 {
			fmt.Fprintf(&b, " · #%s on team", n(t.RankInTeam))
		}
		b.WriteByte('\n')
	}

	var rest []string
	if over := len(shown) - max; over > 0 {
		rest = append(rest, fmt.Sprintf("%d more", over))
	}
	if len(idle) > 0 {
		rest = append(rest, fmt.Sprintf("%d with nothing recent", len(idle)))
	}
	if len(rest) > 0 {
		fmt.Fprintf(&b, "…and %s", strings.Join(rest, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// DonorEmbed renders one donor.
func DonorEmbed(d Donor, s Snapshot) *discordgo.MessageEmbed {
	e := &discordgo.MessageEmbed{
		Title: d.Name,
		URL:   SiteURL + "/donors/" + esc(d.Name),
		Color: colourNormal,
		Fields: []*discordgo.MessageEmbedField{
			field("Rank", fmt.Sprintf("**#%s**\n%s", n(d.Rank), movement(d.RankChange24h)), true),
			field("Points", fmt.Sprintf("**%s**\n%s", short(d.PointsTotal), n(d.PointsTotal)), true),
			field("Per day", fmt.Sprintf("**%s**\n7-day average", short(d.PointsPerDay)), true),
			field("Last 24h", short(d.PointsLast24h), true),
			field("Today (UTC)", short(d.PointsToday), true),
			field("Work units", n(d.WUsTotal), true),
		},
		Footer: footer(s),
	}
	if d.Standing != nil && d.Standing.Lifetime != nil {
		e.Fields = append(e.Fields, field("Standing",
			fmt.Sprintf("top %.2f%% of %s donors", d.Standing.Lifetime.TopPercent, short(d.Standing.Lifetime.Of)), true))
	}
	if s := streakText(d.Streak); s != "" {
		e.Fields = append(e.Fields, field("Streak", s, true))
	}
	if d.TeamCount > 0 {
		e.Fields = append(e.Fields, field("Teams", n(d.TeamCount), true))
	}
	if len(d.Teams) > 0 {
		e.Fields = append(e.Fields, field("Folding for", teamList(d.Teams), false))
	}
	// Said plainly, not as a badge: presenting a shared name's aggregate as one
	// person is the single most misleading thing this bot could do.
	if d.LikelyNotAPerson {
		e.Color = colourWarn
		e.Description = fmt.Sprintf(
			"⚠️ **%s** appears on %s teams and is almost certainly a default name shared by many "+
				"different people. These totals are the sum of all of them, not one folder's record.",
			d.Name, n(d.TeamCount))
	}
	return e
}

// TeamEmbed renders one team.
func TeamEmbed(t Team, s Snapshot) *discordgo.MessageEmbed {
	e := &discordgo.MessageEmbed{
		Title: t.Name,
		URL:   fmt.Sprintf("%s/teams/%d", SiteURL, t.TeamID),
		Color: colourNormal,
		Fields: []*discordgo.MessageEmbedField{
			field("Rank", fmt.Sprintf("**#%s**\n%s", n(t.Rank), movement(t.RankChange24h)), true),
			field("Points", fmt.Sprintf("**%s**\n%s", short(t.PointsTotal), n(t.PointsTotal)), true),
			field("Per day", fmt.Sprintf("**%s**\n7-day average", short(t.PointsPerDay)), true),
			field("Last 24h", short(t.PointsLast24h), true),
			field("Members", fmt.Sprintf("%s producing\nof %s", n(t.MembersActive), n(t.MembersTotal)), true),
			field("Team", fmt.Sprintf("#%d", t.TeamID), true),
		},
		Footer: footer(s),
	}
	if t.Standing != nil && t.Standing.Lifetime != nil {
		e.Fields = append(e.Fields, field("Standing",
			fmt.Sprintf("top %.2f%% of %s teams", t.Standing.Lifetime.TopPercent, short(t.Standing.Lifetime.Of)), true))
	}
	if s := streakText(t.Streak); s != "" {
		e.Fields = append(e.Fields, field("Streak", s, true))
	}
	return e
}

// TextEmbed wraps a preformatted block, which is how the richer answers arrive.
func TextEmbed(title, url, body string, s Snapshot) *discordgo.MessageEmbed {
	// Discord truncates at 4096 and shows nothing rather than part of it, so trim
	// with a marker instead of letting the message vanish.
	const max = 3900
	if len(body) > max {
		body = body[:max] + "\n…truncated"
	}
	return &discordgo.MessageEmbed{
		Title:       title,
		URL:         url,
		Color:       colourNormal,
		Description: "```\n" + body + "\n```",
		Footer:      footer(s),
	}
}

func ErrorEmbed(msg string) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{Color: colourBad, Description: msg}
}
