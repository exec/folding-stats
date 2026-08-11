package web

// Per-page metadata, written into the shell before it is served.
//
// This site is a single-page app, and every URL used to be answered with the same
// bytes: the same <title>, the same description, for the homepage and for every one of
// 2.1M donors. A crawler reads the HTML first and renders JavaScript later — if at
// all, and for a new domain, from the back of a queue. So what it saw was a site whose
// every page was a duplicate of every other page, and search results are written from
// what the HTML said, not from what the app eventually drew.
//
// The fix is not server-side rendering. The page still renders client-side; only the
// head is filled in per URL, which is the part that decides whether a page is indexed
// at all and what its result reads like. That is a few hundred bytes of difference per
// request against a rewrite of the frontend.
//
// The data lives in the API's snapshot, not here, so it arrives as a function. This
// package knows how to render a title; it does not know how to look one up.

import (
	"bytes"
	"fmt"
	"html"
	"net/url"
	"strings"
)

// Meta is what differs between one page and the next.
type Meta struct {
	Title       string
	Description string
	// NoIndex keeps a page that resolves out of the index. A donor who does not
	// exist still renders — the app says so — but it is not a page worth indexing,
	// and saying so is what stops 20,000 sitemap URLs turning into 20,000 soft 404s
	// the day a name is retired upstream.
	NoIndex bool
}

// MetaFunc returns the metadata for a path, or false to leave the shell's defaults
// alone. Called on every HTML request, so it must be cheap and safe for concurrent
// use; a nil MetaFunc disables the whole mechanism and serves the shell unchanged.
type MetaFunc func(path string) (Meta, bool)

// shellMarks are where the metadata block begins and ends inside index.html.
const (
	titleMark = "<title>"
	descMark  = `<meta name="description"`
)

// splitShell cuts index.html either side of the metadata it is going to replace.
//
// Done once at startup, and a failure here refuses to start rather than degrading
// quietly: serving the shell with no title at all would be worse than the duplicate
// titles this exists to fix, and it is the kind of fault that looks fine in a browser.
func splitShell(index []byte) (head, tail []byte, err error) {
	i := bytes.Index(index, []byte(titleMark))
	if i < 0 {
		return nil, nil, fmt.Errorf("web: no %s in index.html; the shell's head has changed shape", titleMark)
	}
	j := bytes.Index(index, []byte(descMark))
	if j < i {
		return nil, nil, fmt.Errorf("web: no %s after the title in index.html", descMark)
	}
	k := bytes.IndexByte(index[j:], '>')
	if k < 0 {
		return nil, nil, fmt.Errorf("web: unterminated %s in index.html", descMark)
	}
	return index[:i], index[j+k+1:], nil
}

// shell renders the document for one URL.
func (s *site) shell(base, path string, m Meta) string {
	var b strings.Builder
	b.Grow(len(s.shellHead) + len(s.shellTail) + 1024)
	b.Write(s.shellHead)
	writeHead(&b, m, canonicalURL(base, path))
	b.Write(s.shellTail)
	return b.String()
}

// canonicalURL is the one address a page should be indexed under.
//
// Built from the cleaned path with the query dropped: /donors/DH, /donors/DH/ and
// /donors/DH?from=somewhere are one page, and without this they are three that each
// dilute the others. Re-encoded rather than passed through, because donor names are
// upstream text — they contain spaces, slashes and characters no crawler should have
// to guess the escaping of.
func canonicalURL(base, path string) string {
	return base + (&url.URL{Path: path}).EscapedPath()
}

// writeHead emits the metadata block. Every value is escaped: a donor name is
// arbitrary text from upstream, and it lands in both element content and attribute
// values here.
func writeHead(b *strings.Builder, m Meta, loc string) {
	title := html.EscapeString(m.Title)
	desc := html.EscapeString(m.Description)
	href := html.EscapeString(loc)

	b.WriteString("<title>" + title + "</title>\n")
	b.WriteString(`<meta name="description" content="` + desc + "\">\n")
	b.WriteString(`<link rel="canonical" href="` + href + "\">\n")
	// Open Graph is what a link unfurls as in a chat client or a post. The same two
	// strings do the work, so there is no second set of copy to keep in step.
	b.WriteString(`<meta property="og:type" content="website">` + "\n")
	b.WriteString(`<meta property="og:site_name" content="foldingstats.org">` + "\n")
	b.WriteString(`<meta property="og:title" content="` + title + "\">\n")
	b.WriteString(`<meta property="og:description" content="` + desc + "\">\n")
	b.WriteString(`<meta property="og:url" content="` + href + "\">\n")
	b.WriteString(`<meta name="twitter:card" content="summary">` + "\n")
	if m.NoIndex {
		// follow, not none: the page is not worth indexing but its links are still
		// worth walking, which is how a crawler reaches the pages that are.
		b.WriteString(`<meta name="robots" content="noindex,follow">` + "\n")
	}
}

// defaultMeta is the site's own description, used for the homepage and for anything
// the lookup declines to describe.
var defaultMeta = Meta{
	Title:       "Folding@home Stats",
	Description: "Folding@home donor and team statistics, with a free public API. No sign-up, no challenge pages, and no rate limits today.",
}

// metaFor resolves the metadata for a path, falling back to the site default.
func (s *site) metaFor(path string) Meta {
	if s.meta == nil {
		return defaultMeta
	}
	m, ok := s.meta(path)
	if !ok {
		return defaultMeta
	}
	if m.Title == "" {
		m.Title = defaultMeta.Title
	}
	if m.Description == "" {
		m.Description = defaultMeta.Description
	}
	return m
}
