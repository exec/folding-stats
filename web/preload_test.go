package web

import (
	"net/http"
	"net/http/httptest"
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
