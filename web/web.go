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
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

//go:embed index.html app.css app.js api.js ui.js views.js charts.js countdown.js clock.js format.js vendor
var assets embed.FS

// assetRef matches a same-origin asset path in a quoted string: an ES module
// specifier, a stylesheet href, a script src. Only paths carrying a .js or .css
// extension match, so API paths and client-side routes are left alone.
var assetRef = regexp.MustCompile(`(["'])(/[A-Za-z0-9_./-]+\.(?:js|css))(["'])`)

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
		// Inline data: URIs are already present — the favicon is one — and there is
		// nothing to fetch.
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
