// Package web serves the single-page frontend from assets embedded in the binary.
//
// The Go file lives beside the assets it embeds because go:embed cannot reach
// above its own directory, and keeping a second copy elsewhere would only give the
// two a chance to drift.
//
// Embedding keeps deployment to one file with no build step, no node_modules and no
// CDN dependency — the page works on a machine with no outbound network, which is
// rather the point of not making a browser solve something before it can show a
// number.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

//go:embed index.html icon.svg app.css app.js api.js ui.js views.js charts.js countdown.js clock.js format.js vendor
var assets embed.FS

// assetRef matches a same-origin asset path in a quoted string: an ES module
// specifier, a stylesheet href, a script src, the icon link. Only paths carrying a
// .js, .css or .svg extension match, so API paths and client-side routes are left
// alone.
var assetRef = regexp.MustCompile(`(["'])(/[A-Za-z0-9_./-]+\.(?:js|css|svg))(["'])`)

// build is a fingerprint of the whole asset set, stamped onto every internal
// reference at startup.
//
// The frontend is a graph of ES modules that import each other by absolute path. If
// those paths are cacheable and unversioned, a deploy leaves browsers free to mix
// versions — a new app.js importing a stale countdown.js it still holds, which fails
// as a blank page rather than as anything diagnosable. Versioning every reference
// with one hash makes the graph atomic: a changed deploy changes every URL in it, so
// a browser either has the whole old set or fetches the whole new one.
//
// It also makes the assets genuinely immutable, which caches better than the short
// max-age it replaces: nothing at a versioned URL ever changes.
type site struct {
	files map[string][]byte
	index []byte
	build string
	// preload is the shell's render-blocking assets, as a Link header.
	preload string
	// The shape of the embedded tree: extensions that appear at its root, and the
	// directories it occupies. Together they say which paths are asset requests.
	assetExts map[string]bool
	assetDirs map[string]bool
	// routes are the client-side routes, read out of app.js rather than restated
	// here. A path matching none of them gets the shell with a 404 status.
	routes []*regexp.Regexp
}

// jsRoute pulls one route pattern out of the router's table in app.js.
//
// Each entry is `[/^\/whatever\/?$/, handler]`, and the sources happen to be valid Go
// regexps unchanged — including the character classes and the non-greedy donor
// pattern — so they can be compiled directly instead of transcribed.
var jsRoute = regexp.MustCompile(`(?m)^\s*\[/(\^.*?\$)/\s*,`)

// clientRoutes compiles the router's own table so the server can tell a real page from
// a miss.
//
// Restating the list in Go would work until the day someone adds a route to app.js and
// not here, at which point a working page starts answering 404 to every crawler while
// looking fine in a browser — a divergence nothing would surface. Reading the single
// source of truth cannot drift.
func clientRoutes(appJS []byte) ([]*regexp.Regexp, error) {
	ms := jsRoute.FindAllSubmatch(appJS, -1)
	out := make([]*regexp.Regexp, 0, len(ms))
	for _, m := range ms {
		re, err := regexp.Compile(string(m[1]))
		if err != nil {
			return nil, fmt.Errorf("web: route %q from app.js: %w", m[1], err)
		}
		out = append(out, re)
	}
	// Zero would mean the extraction silently stopped working and every page on the
	// site would start returning 404. Better to refuse to start.
	if len(out) < 5 {
		return nil, fmt.Errorf("web: found only %d client routes in app.js; the router's shape must have changed", len(out))
	}
	return out, nil
}

// isRoute reports whether the SPA has a page for this path.
func (s *site) isRoute(clean string) bool {
	for _, re := range s.routes {
		if re.MatchString(clean) {
			return true
		}
	}
	return false
}

// isAsset reports whether a path addresses the embedded asset tree rather than a
// client-side route.
//
// "Has an extension" is the obvious test and it is wrong, because a donor name is a
// path segment and 57,039 of them contain a dot. /donors/Mr.Hello has the extension
// ".Hello", so the obvious test sent every one of those to 404 on direct load — the
// page worked when clicked through in the app and broke the moment anyone shared the
// link, which is the one thing a stats page is for.
//
// The asset tree is shallow and known: files at the root, plus whatever directories
// it embeds. So an asset request is one that names a file we have, or that sits at
// the root carrying an extension the tree actually uses, or that lives under one of
// its directories. Anything deeper belongs to the router, which is where every client
// route with a user-supplied segment lives.
func (s *site) isAsset(clean string) bool {
	if _, ok := s.files[clean]; ok {
		return true
	}
	rest := strings.TrimPrefix(clean, "/")
	if dir, _, nested := strings.Cut(rest, "/"); nested {
		// Under an embedded directory, a miss is a miss — a stale /vendor/ import
		// must not resolve to HTML.
		return s.assetDirs[dir]
	}
	// At the root, an extension the tree uses means a missing module or stylesheet,
	// which has to 404 rather than quietly return the shell.
	return s.assetExts[path.Ext(clean)]
}

func newSite() (*site, error) {
	raw := map[string][]byte{}
	err := fs.WalkDir(assets, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(assets, p)
		if err != nil {
			return err
		}
		raw["/"+p] = b
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Hash every asset, in a stable order, so the fingerprint depends only on content.
	names := make([]string, 0, len(raw))
	for n := range raw {
		names = append(names, n)
	}
	sortStrings(names)
	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%s\x00%d\x00", n, len(raw[n]))
		h.Write(raw[n])
	}
	build := hex.EncodeToString(h.Sum(nil))[:12]

	s := &site{files: make(map[string][]byte, len(raw)), build: build}
	for n, b := range raw {
		if strings.HasSuffix(n, ".js") || strings.HasSuffix(n, ".css") || strings.HasSuffix(n, ".html") {
			b = assetRef.ReplaceAll(b, []byte("${1}${2}?v="+build+"${3}"))
		}
		s.files[n] = b
	}
	s.index = s.files["/index.html"]
	if s.index == nil {
		return nil, fmt.Errorf("web: index.html missing from embedded assets")
	}
	s.preload = preloadLink(s.index)

	if s.routes, err = clientRoutes(s.files["/app.js"]); err != nil {
		return nil, err
	}

	s.assetExts = map[string]bool{}
	s.assetDirs = map[string]bool{}
	for n := range s.files {
		rest := strings.TrimPrefix(n, "/")
		if dir, _, nested := strings.Cut(rest, "/"); nested {
			s.assetDirs[dir] = true
			continue
		}
		if e := path.Ext(n); e != "" {
			s.assetExts[e] = true
		}
	}
	return s, nil
}

// shellStyle and shellScript find what the shell needs before it can paint.
var (
	shellStyle  = regexp.MustCompile(`<link[^>]+rel="stylesheet"[^>]+href="([^"]+)"`)
	shellScript = regexp.MustCompile(`<script[^>]+src="([^"]+)"`)
)

// preloadLink builds the Link header advertising the shell's render-blocking assets.
//
// The point of it is Early Hints: a CDN caches these headers and replays them as a
// 103 before the origin has answered, so the browser can start fetching the CSS and
// the module while the shell itself is still in flight. That matters here because the
// shell is deliberately no-cache — every single load revalidates it before the
// browser learns that any of this exists.
//
// Read out of index.html rather than listed by hand. A hint is a claim about a
// document made before that document arrives, so a hardcoded list that drifted would
// preload files the page no longer uses and stay silent about the ones it does —
// and it would do so invisibly, because a wrong preload still renders correctly, just
// slower than before. Parsing the shell keeps the claim and the document identical by
// construction.
//
// Stylesheets first: they block rendering, the module does not.
func preloadLink(index []byte) string {
	var parts []string
	add := func(href, as string) {
		// A data: URI carries its own bytes, so there is nothing to fetch and a
		// preload for one is pure waste. None are left in the shell today, but the
		// guard is cheap and the alternative is a hint that costs a header and
		// delivers nothing.
		if href == "" || strings.HasPrefix(href, "data:") {
			return
		}
		parts = append(parts, fmt.Sprintf("<%s>; rel=preload; as=%s", href, as))
	}
	for _, m := range shellStyle.FindAllSubmatch(index, -1) {
		add(string(m[1]), "style")
	}
	for _, m := range shellScript.FindAllSubmatch(index, -1) {
		// as=script rather than rel=modulepreload: Cloudflare only commits to
		// caching preload and preconnect, and a hint it drops is worth less than a
		// slightly less precise one it keeps.
		add(string(m[1]), "script")
	}
	return strings.Join(parts, ", ")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Handler serves the frontend. Real files get cache headers; anything else falls
// through to index.html so client-side routes deep-link.
func Handler() (http.Handler, error) {
	s, err := newSite()
	if err != nil {
		return nil, err
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean(r.URL.Path)

		if clean == robotsPath {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			io.WriteString(w, robotsTxt())
			return
		}

		if clean == securityTxtPath {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			io.WriteString(w, securityTxt(time.Now(), r))
			return
		}

		// Serve it or 404 if it is an asset request, rather than returning the SPA
		// shell under a JavaScript content type — which fails in the browser in a
		// thoroughly confusing way.
		if s.isAsset(clean) {
			body, ok := s.files[clean]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", contentType(clean))
			s.setCache(w, r)
			http.ServeContent(w, r, clean, time.Time{}, strings.NewReader(string(body)))
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The shell is the one thing that must never be cached: it carries the
		// version stamp that invalidates everything else.
		w.Header().Set("Cache-Control", "no-cache")

		// A path the router has no page for gets the shell — the app renders its own
		// "Not found" — but with the status that says so.
		//
		// Serving 200 there is a soft 404: the page reads as missing to a person and
		// as real content to everything else. On a site that invites automated
		// clients in robots.txt and publishes an agent endpoint, that is not a
		// cosmetic detail — a crawler indexes the not-found page, and an agent
		// probing for /openapi.json or /llms.txt is told it found one.
		if !s.isRoute(clean) {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, string(s.index))
			return
		}

		// Sent on the real response so a CDN can cache it and replay it as a 103
		// Early Hint ahead of the next one. Harmless where nothing does: browsers
		// treat it as an ordinary preload directive.
		if s.preload != "" {
			w.Header().Set("Link", s.preload)
		}
		http.ServeContent(w, r, "index.html", time.Time{}, strings.NewReader(string(s.index)))
	}), nil
}

// Build is the asset fingerprint, exposed so the API can report which frontend a
// running binary is serving.
func Build() string {
	s, err := newSite()
	if err != nil {
		return ""
	}
	return s.build
}

func (s *site) setCache(w http.ResponseWriter, r *http.Request) {
	// A request carrying the current stamp asked for content that cannot change:
	// this exact URL will never serve anything else. Anything else — a hand-typed
	// path, a stale bookmark, a crawler — revalidates.
	if r.URL.Query().Get("v") == s.build {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
}

func contentType(p string) string {
	switch path.Ext(p) {
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

// securityTxtPath is where RFC 9116 says to look for a security contact.
const securityTxtPath = "/.well-known/security.txt"

// securityTxt renders the security contact document.
//
// Expires is required by RFC 9116 and is the field that makes these rot: a file whose
// date has passed is formally invalid, so a researcher's tooling reports "no contact"
// for a site that has one and simply forgot to edit a constant. Hardcoding a date
// guarantees that outcome eventually.
//
// So it is computed: the first of the current month, plus a year. That is always
// between eleven and twelve months ahead — inside the year RFC 9116 asks for — and it
// only changes when the month does, so the document is stable for weeks at a time and
// caches properly rather than differing on every request.
//
// The tradeoff is honest and worth naming: a date that renews itself can never prompt
// anyone to re-check that the address still works. That prompt is worth less than the
// alternative, which is a contact silently declared invalid while it was reachable
// the whole time.
func securityTxt(now time.Time, r *http.Request) string {
	u := now.UTC()
	expires := time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(1, 0, 0)

	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") == "" && strings.HasPrefix(r.Host, "127.0.0.1") {
		scheme = "http"
	}
	base := scheme + "://" + r.Host

	return "# Security contact for " + r.Host + "\n" +
		"#\n" +
		"# This is a statistics mirror with a public, unauthenticated, read-only API.\n" +
		"# There are no accounts and no user data beyond request logs. Reports about\n" +
		"# the service itself, its API, or the data it derives are all welcome.\n" +
		"\n" +
		"Contact: mailto:security@exec.codes\n" +
		"Expires: " + expires.Format(time.RFC3339) + "\n" +
		"Preferred-Languages: en\n" +
		"Canonical: " + base + securityTxtPath + "\n" +
		"Policy: " + base + "/disclaimer\n"
}

// robotsPath is the crawler policy, which for this site is an invitation.
const robotsPath = "/robots.txt"

// robotsTxt states the policy the project was founded on.
//
// The appetite for programmatic Folding@home statistics is what put pressure on the
// site this one exists to relieve. Automated clients are therefore the point here,
// not a problem to be managed, and the policy says so in the two places a machine
// reads: an unrestricted Allow, and a Content-Signal granting search and AI input.
//
// The named crawlers are listed explicitly and redundantly. A bare "User-agent: *"
// already permits them, but a CDN or a future intermediary that injects its own
// managed policy will list those names with Disallow — being explicit means the
// disagreement is visible in one file rather than silently resolved against us.
//
// ai-train is deliberately absent. Under the Content Signals policy an omitted
// signal neither grants nor restricts, which is the honest position for a decision
// nobody has actually made: "reachable by AI agents" and "licensed for model
// training" are different questions, and only the first one has been answered.
func robotsTxt() string {
	agents := []string{
		"ClaudeBot", "Claude-User", "Claude-SearchBot", "GPTBot", "OAI-SearchBot",
		"CCBot", "Google-Extended", "Applebot-Extended", "Amazonbot", "Bytespider",
		"meta-externalagent", "PerplexityBot", "cohere-ai",
	}
	var b strings.Builder
	b.WriteString("# Automated clients are welcome here. That is the point of the site.\n")
	b.WriteString("#\n")
	b.WriteString("# Before crawling these pages: there is a free, unauthenticated JSON API at\n")
	b.WriteString("# /api covering everything the HTML shows. No key, no sign-up, no challenge\n")
	b.WriteString("# page. It is cheaper for you and for us than parsing rendered pages, and it\n")
	b.WriteString("# is the interface that will not change shape under you.\n")
	b.WriteString("#\n")
	b.WriteString("# Please read /v1/status and cache against next_expected_at rather than\n")
	b.WriteString("# polling: the data changes once an hour and not otherwise.\n")
	b.WriteString("\n")
	b.WriteString("User-agent: *\n")
	b.WriteString("Content-Signal: search=yes, ai-input=yes\n")
	b.WriteString("Allow: /\n")
	for _, a := range agents {
		b.WriteString("\nUser-agent: " + a + "\nAllow: /\n")
	}
	return b.String()
}
