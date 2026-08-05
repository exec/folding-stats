package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// TestPreloadLinkMatchesTheShell holds the Early Hints header to the document it
// describes.
//
// A preload hint is a claim about a page made before the page arrives, so the failure
// mode is not an error: naming an asset the shell no longer loads wastes a fetch, and
// omitting one it does load silently gives back the latency the hint existed to save.
// Both render perfectly. So this derives the expected set from index.html
// independently and requires the header to name exactly that.
func TestPreloadLinkMatchesTheShell(t *testing.T) {
	s, err := newSite()
	if err != nil {
		t.Fatal(err)
	}
	if s.preload == "" {
		t.Fatal("no preload header built; Early Hints would have nothing to replay")
	}

	// Everything the shell fetches to render, found without reusing the production
	// regexps — a bug in those would otherwise agree with itself.
	href := regexp.MustCompile(`(?:href|src)="(/[^"]+\.(?:css|js)[^"]*)"`)
	want := map[string]bool{}
	for _, m := range href.FindAllStringSubmatch(string(s.index), -1) {
		want[m[1]] = true
	}
	if len(want) < 3 {
		t.Fatalf("found %d assets in the shell, expected at least 3", len(want))
	}

	got := map[string]bool{}
	for _, part := range strings.Split(s.preload, ", ") {
		u := part[strings.Index(part, "<")+1 : strings.Index(part, ">")]
		got[u] = true
		if !strings.Contains(part, "rel=preload") {
			t.Errorf("%q is not a preload directive", part)
		}
		if !strings.Contains(part, "; as=") {
			t.Errorf("%q has no `as` type; browsers ignore a preload without one", part)
		}
		// Every reference in the shell is build-stamped, and an unstamped preload
		// would fetch a URL the page then does not use — two requests, no reuse.
		if !strings.Contains(u, "?v="+s.build) {
			t.Errorf("preload %q is not stamped with the build %s", u, s.build)
		}
	}

	for u := range want {
		if !got[u] {
			t.Errorf("shell loads %s but it is not preloaded", u)
		}
	}
	for u := range got {
		if !want[u] {
			t.Errorf("preloading %s, which the shell does not load", u)
		}
	}

	// Stylesheets block rendering; the module does not. Hints are acted on in order.
	first := s.preload[:strings.Index(s.preload, ",")]
	if !strings.Contains(first, "as=style") {
		t.Errorf("first hint is %q; stylesheets should lead", first)
	}
}

// TestPreloadIgnoresInlineData keeps the favicon out. It is a data: URI already in
// the document, so preloading it would be a fetch of something never fetched.
func TestPreloadIgnoresInlineData(t *testing.T) {
	doc := []byte(`<link rel="icon" href="data:image/svg+xml,<svg/>">` +
		`<link rel="stylesheet" href="/a.css?v=1">` +
		`<script type="module" src="/a.js?v=1"></script>`)
	got := preloadLink(doc)
	if strings.Contains(got, "data:") {
		t.Errorf("data URI made it into the header: %s", got)
	}
	if want := `</a.css?v=1>; rel=preload; as=style, </a.js?v=1>; rel=preload; as=script`; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestShellResponseCarriesTheLinkHeader checks the wiring, not the string: the header
// has to be on the shell (which a CDN replays as a 103) and on nothing else.
func TestShellResponseCarriesTheLinkHeader(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatal(err)
	}
	s, _ := newSite()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Header().Get("Link"); got != s.preload {
		t.Errorf("shell Link header = %q, want %q", got, s.preload)
	}

	// A deep-linked client route serves the same shell, so it needs the same hint.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/teams", nil))
	if rec.Header().Get("Link") == "" {
		t.Error("a client-side route serves the shell but sends no preload hint")
	}

	// An asset must not: it is already the thing being fetched.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app.css?v="+s.build, nil))
	if got := rec.Header().Get("Link"); got != "" {
		t.Errorf("asset response carries a preload header: %q", got)
	}
}

// TestDottedRoutesReachTheShell covers the split between an asset request and a
// client route.
//
// A donor name is a path segment, and 2.69% of them — 57,039 people — contain a dot.
// Treating "has an extension" as "is an asset" sent every one of those to 404 on
// direct load: the page worked when clicked through inside the app and broke the
// instant anyone pasted the link, which is the entire purpose of a stats page.
func TestDottedRoutesReachTheShell(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatal(err)
	}
	s, _ := newSite()

	shell := func(path string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200 — a shared link to this page is dead", path, rec.Code)
			return
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s: content type %q, want html", path, ct)
		}
	}
	missing := func(path string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404 — a missing asset must not resolve to the shell",
				path, rec.Code)
		}
	}
	asset := func(path, wantType string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", path, rec.Code)
			return
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, wantType) {
			t.Errorf("%s: content type %q, want %s", path, ct, wantType)
		}
	}

	// Real donor names from the production corpus.
	for _, name := range []string{
		"Mr.Hello", "Patrick.Paquet", "g.l", "45363.pinzgauer",
		"tnt.uni-hannover.de", "[Zebulon.fr]_Gtevoone82",
		"PantyShot(www.overclockers.com)",
		// A name that looks exactly like an asset is still a donor at this depth.
		"app.js", "index.html",
	} {
		shell("/donors/" + url.PathEscape(name))
	}
	shell("/teams")
	shell("/donors")
	shell("/overview")
	shell("/posts/some.slug.with.dots")

	// The protection this replaced must survive: a missing module at the root, and a
	// stale import under an embedded directory, both have to 404 rather than hand
	// back HTML under a JavaScript content type.
	missing("/nonexistent.js")
	missing("/nonexistent.css")
	missing("/vendor/gone.js")
	missing("/vendor/nested/deep.css")

	// An extension the asset tree does not use cannot be a missing module, so it
	// falls through to the shell and the client router renders its own not-found.
	// That is the same fallback that makes a dotted donor name work at all.
	shell("/app.notanext")

	// And real assets still serve.
	asset("/app.js", "text/javascript")
	asset("/app.css", "text/css")
	asset("/app.js?v="+s.build, "text/javascript")
	for n := range s.files {
		if strings.HasPrefix(n, "/vendor/") && strings.HasSuffix(n, ".js") {
			asset(n, "text/javascript")
			break
		}
	}
}
