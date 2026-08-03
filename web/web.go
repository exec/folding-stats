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
	return s, nil
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

		// A path with an extension is an asset request: serve it or 404, rather
		// than returning the SPA shell under a JavaScript content type — which
		// fails in the browser in a thoroughly confusing way.
		if path.Ext(clean) != "" {
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
