package bot

import (
	"fmt"
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
	age := time.Since(s.At).Round(time.Minute)
	txt := fmt.Sprintf("data from %s UTC (%s old)", s.At.UTC().Format("15:04"), age)
	if s.Stale {
		txt += " — upstream update is late"
	}
	if s.WarmingUp != nil && s.WarmingUp.HistorySpanSec > 0 {
		d := time.Duration(s.WarmingUp.HistorySpanSec) * time.Second
		txt += fmt.Sprintf(" — only %s of history collected, so rates read low", d.Round(time.Hour))
	}
	return &discordgo.MessageEmbedFooter{Text: txt}
}

func field(name, value string, inline bool) *discordgo.MessageEmbedField {
	return &discordgo.MessageEmbedField{Name: name, Value: value, Inline: inline}
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
	if d.Streak != nil && d.Streak.Current > 0 {
		e.Fields = append(e.Fields, field("Streak",
			fmt.Sprintf("%d days (best %d)", d.Streak.Current, d.Streak.Longest), true))
	}
	if d.TeamCount > 0 {
		e.Fields = append(e.Fields, field("Teams", n(d.TeamCount), true))
	}
	if len(d.Teams) > 0 {
		var b strings.Builder
		for i, t := range d.Teams {
			if i == 5 {
				fmt.Fprintf(&b, "…and %d more", len(d.Teams)-5)
				break
			}
			fmt.Fprintf(&b, "`%s/day` %s\n", short(t.PointsPerDay), t.Name)
		}
		e.Fields = append(e.Fields, field("Folding for", b.String(), false))
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
	if t.Streak != nil && t.Streak.Current > 0 {
		e.Fields = append(e.Fields, field("Streak",
			fmt.Sprintf("%d days (best %d)", t.Streak.Current, t.Streak.Longest), true))
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
