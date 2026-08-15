package api

// What a search engine is told each page is.
//
// The frontend renders client-side, so until now every URL on the site answered with
// the same head: one title, one description, for the homepage and for two million
// donors alike. That is the shape of a site with one page, and it is what got indexed.
//
// These are the two strings that decide whether a page is indexed separately and what
// its search result reads like. They are built from the snapshot already in memory —
// no database work, nothing that can block, because this runs on every HTML request
// including the ones that turn out to be crawlers walking ten thousand donors.
//
// Descriptions are written to survive truncation. Search results cut around 155
// characters, so the fact that distinguishes the page comes first and the invitation
// comes last, where losing it costs nothing.

import (
	"strconv"
	"strings"

	"folding/content"
)

// PageMeta is the per-page head. It mirrors web.Meta without importing it, so the
// frontend package stays independent of this one.
type PageMeta struct {
	Title       string
	Description string
	NoIndex     bool
}

// site is the suffix every title carries. Search results show a truncated title, and
// the name is what makes an unfamiliar result trustworthy enough to click.
const siteSuffix = " — Folding@home Stats"

// PageMeta describes the page at a client-side route.
//
// Returns false for anything it has no opinion about, which leaves the shell's own
// defaults in place — the homepage among them.
func (s *Server) PageMeta(p string) (PageMeta, bool) {
	snap := s.snap.Load()
	if snap == nil {
		// Before the first ingest there is nothing true to say about a donor, and a
		// description invented now would be cached by whoever read it.
		return PageMeta{}, false
	}

	switch {
	case p == "/teams/around-the-globe":
		return PageMeta{
			Title:       "Folding Around The Globe" + siteSuffix,
			Description: "Explore Folding@home teams around the world and see global participation by country.",
		}, true
	case strings.HasPrefix(p, "/donors/"):
		return snap.donorMeta(strings.TrimPrefix(p, "/donors/"))
	case strings.HasPrefix(p, "/teams/"):
		return snap.teamMeta(strings.TrimPrefix(p, "/teams/"))
	case strings.HasPrefix(p, "/blog/"):
		return postMeta(strings.TrimPrefix(p, "/blog/"))
	}

	switch p {
	case "/donors":
		return PageMeta{
			Title: "Donor rankings" + siteSuffix,
			Description: "Every one of " + compact(len(snap.Ranks.Donors)) +
				" Folding@home donors, ranked by points with daily and weekly production. Free JSON API, no sign-up.",
		}, true
	case "/teams":
		return PageMeta{
			Title: "Team rankings" + siteSuffix,
			Description: "All " + compact(len(snap.State.Teams)) +
				" Folding@home teams ranked by points, with active member counts and daily production. Free JSON API, no sign-up.",
		}, true
	case "/overview":
		return PageMeta{
			Title:       "Project overview" + siteSuffix,
			Description: "Folding@home at a glance: points and work units across the whole project, who is producing now, and what changed in the last day.",
		}, true
	case "/watchlist":
		return PageMeta{
			Title:       "Your Folding@home watchlist" + siteSuffix,
			Description: "A private, browser-local watchlist of Folding@home donors and teams.",
			NoIndex:     true,
		}, true
	case "/explore":
		return PageMeta{
			Title:       "Compare teams, donors and goals" + siteSuffix,
			Description: "Compare Folding@home teams and donors, calculate the production needed for a goal, and see the biggest daily rank movements.",
		}, true
	case "/api":
		return PageMeta{
			Title:       "Free Folding@home JSON API" + siteSuffix,
			Description: "A free, unauthenticated JSON API for Folding@home donor and team statistics. No key, no sign-up, no challenge pages. Hourly data, documented endpoints.",
		}, true
	case "/agents":
		return PageMeta{
			Title:       "MCP server for AI agents" + siteSuffix,
			Description: "Connect Claude Code, or any MCP client, to live Folding@home statistics. Eleven tools covering donors, teams, rankings, history and rivals.",
		}, true
	case "/fold":
		return PageMeta{
			Title:       "Start folding" + siteSuffix,
			Description: "Run Folding@home and contribute compute to disease research, or rent a GPU box by the hour. Setup, monitoring and live production from your own machines.",
		}, true
	case "/bots":
		return PageMeta{
			Title:       "Discord bot" + siteSuffix,
			Description: "A Discord bot for Folding@home statistics: look up donors and teams, track rankings, and get alerts when production stops.",
		}, true
	case "/privacy":
		return PageMeta{Title: "Privacy" + siteSuffix,
			Description: "What this site stores, what it does not, and who it shares data with."}, true
	case "/disclaimer":
		return PageMeta{Title: "Disclaimer" + siteSuffix,
			Description: "This is an independent mirror of published Folding@home statistics, not affiliated with Folding@home or Stanford."}, true
	case "/search":
		// A results page is a view of pages that each already exist.
		return PageMeta{Title: "Search" + siteSuffix,
			Description: "Search Folding@home donors and teams by name.", NoIndex: true}, true
	}
	return PageMeta{}, false
}

// donorMeta describes one donor's page. rest is everything after /donors/.
func (s *Snapshot) donorMeta(rest string) (PageMeta, bool) {
	name, rivals := strings.CutSuffix(rest, "/rivals")
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		return PageMeta{}, false
	}

	idx, ok := s.donorIndexByName(name)
	if !ok {
		// The route resolves and the app will say "not found". Naming the donor is
		// still the honest title, but the page must not be indexed — this is exactly
		// the URL a sitemap keeps pointing at after a name disappears upstream.
		return PageMeta{
			Title:       name + " — donor not found" + siteSuffix,
			Description: "No Folding@home donor by this name appears in the current statistics.",
			NoIndex:     true,
		}, true
	}
	d := s.donorView(idx, false)

	if rivals {
		return PageMeta{
			Title: d.Name + "'s rivals" + siteSuffix,
			Description: "The Folding@home donors immediately above and below " + d.Name +
				" at rank " + fmtInt(int64(d.Rank)) + ", and how long it would take to close the gap.",
		}, true
	}
	// Rank first: it is the fact that distinguishes this donor from the last one, and
	// the one a person scanning results is looking for.
	var b strings.Builder
	b.WriteString(d.Name + " is ranked #" + fmtInt(int64(d.Rank)) + " of " +
		compact(len(s.Ranks.Donors)) + " Folding@home donors with " +
		fmtInt(d.PointsTotal) + " points from " + plural(int(d.WUsTotal), "work unit"))
	if d.TeamCount > 1 {
		b.WriteString(", across " + plural(int(d.TeamCount), "team"))
	}
	b.WriteString(". Daily production, history and rivals.")

	return PageMeta{
		Title:       d.Name + " — Folding@home donor stats",
		Description: b.String(),
	}, true
}

// teamMeta describes one team's page. rest is everything after /teams/.
func (s *Snapshot) teamMeta(rest string) (PageMeta, bool) {
	idStr, rivals := strings.CutSuffix(rest, "/rivals")
	idStr = strings.TrimSuffix(idStr, "/")

	id, err := strconv.ParseInt(idStr, 10, 32)
	if err != nil {
		return PageMeta{}, false
	}
	slot, ok := s.State.TeamSlot(int32(id))
	if !ok {
		return PageMeta{
			Title:       "Team " + idStr + " not found" + siteSuffix,
			Description: "No Folding@home team with this id appears in the current statistics.",
			NoIndex:     true,
		}, true
	}
	t := s.teamView(slot)

	// A team's own name is what people search for; the id is what they arrive with.
	label := t.Name
	if strings.TrimSpace(label) == "" {
		label = "Team " + idStr
	}
	if rivals {
		return PageMeta{
			Title: label + "'s rivals" + siteSuffix,
			Description: "The Folding@home teams immediately above and below " + label +
				" at rank " + fmtInt(int64(t.Rank)) + ", and how long it would take to close the gap.",
		}, true
	}
	return PageMeta{
		Title: label + " — Folding@home team " + idStr + " stats",
		Description: label + " is ranked #" + fmtInt(int64(t.Rank)) + " of " +
			compact(len(s.State.Teams)) + " Folding@home teams with " +
			fmtInt(t.PointsTotal) + " points from " + plural(int(t.WUsTotal), "work unit") +
			" and " + plural(int(t.MembersActive), "active member") + " of " +
			fmtInt(int64(t.MembersTotal)) + ".",
	}, true
}

// postMeta describes a written post, which already has a title and a summary.
func postMeta(slug string) (PageMeta, bool) {
	p, ok := content.Lookup(strings.TrimSuffix(slug, "/"))
	if !ok {
		return PageMeta{}, false
	}
	return PageMeta{Title: p.Title + siteSuffix, Description: p.Summary}, true
}

// compact abbreviates a corpus size. "2.1M donors" reads as a scale, where the exact
// figure reads as a number to check — and it changes every hour, which would make
// every description on the site differ from the one indexed an hour ago.
func compact(n int) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 10_000:
		return strconv.Itoa(n/1000) + "k"
	default:
		return fmtInt(int64(n))
	}
}
