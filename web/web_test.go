package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestServesAssetsAndRoutes(t *testing.T) {
	h, err := Handler(nil)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}

	for _, tc := range []struct {
		path        string
		wantStatus  int
		wantType    string
		wantCaching string
	}{
		{"/", 200, "text/html", "no-cache"},
		// Unversioned requests revalidate: only a URL carrying the current build
		// stamp is promised never to change.
		{"/app.css", 200, "text/css", "no-cache"},
		{"/app.js", 200, "text/javascript", "no-cache"},
		{"/vendor/uPlot.esm.min.js", 200, "text/javascript", "no-cache"},
		{"/icon.svg", 200, "image/svg+xml", "no-cache"},
		// Client-side routes must deep-link to the shell, not 404.
		{"/teams", 200, "text/html", "no-cache"},
		{"/donors/DH", 200, "text/html", "no-cache"},
		{"/api", 200, "text/html", "no-cache"},
		// A missing asset must 404 rather than returning the HTML shell under a
		// JavaScript content type, which fails in the browser confusingly.
		{"/missing.js", 404, "", ""},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.wantStatus {
			t.Errorf("%s: status %d, want %d", tc.path, rec.Code, tc.wantStatus)
			continue
		}
		if tc.wantType != "" && !strings.Contains(rec.Header().Get("Content-Type"), tc.wantType) {
			t.Errorf("%s: content-type %q, want %q", tc.path, rec.Header().Get("Content-Type"), tc.wantType)
		}
		if tc.wantCaching != "" && !strings.Contains(rec.Header().Get("Cache-Control"), tc.wantCaching) {
			t.Errorf("%s: cache-control %q, want %q", tc.path, rec.Header().Get("Cache-Control"), tc.wantCaching)
		}
	}
}

// TestVersionedAssetsAreImmutable is the other half of the cache contract: a URL
// stamped with the current build never changes, so it is safe to keep for a year.
func TestVersionedAssetsAreImmutable(t *testing.T) {
	h, _ := Handler(nil)
	build := Build()
	if build == "" {
		t.Fatal("Build() is empty")
	}
	for _, p := range []string{"/app.js", "/app.css", "/vendor/uPlot.esm.min.js"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p+"?v="+build, nil))
		if rec.Code != 200 {
			t.Errorf("%s?v=%s: status %d", p, build, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("%s?v=%s: cache-control %q, want immutable", p, build, cc)
		}
	}
	// A stale stamp is a browser holding a previous deploy's URL. It must revalidate
	// rather than be handed the new bytes under a promise of immutability.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app.js?v=deadbeef", nil))
	if cc := rec.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Errorf("stale stamp got %q, want revalidation", cc)
	}
}

// TestModuleGraphIsVersionedTogether is the property that makes a deploy atomic.
//
// The frontend is ES modules importing each other by absolute path. If any specifier
// in the graph is unversioned, a browser may satisfy it from cache and run one
// deploy's module against another's — which surfaces as a blank page, not an error.
func TestModuleGraphIsVersionedTogether(t *testing.T) {
	h, _ := Handler(nil)
	build := Build()

	for _, p := range []string{"/", "/app.js", "/views.js", "/charts.js", "/countdown.js"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		body := rec.Body.String()

		for _, m := range assetRef.FindAllStringSubmatch(body, -1) {
			ref := m[2]
			if !strings.Contains(body, ref+"?v="+build) {
				t.Errorf("%s references %s without the build stamp", p, ref)
			}
			// And the stamped URL must actually resolve.
			sub := httptest.NewRecorder()
			h.ServeHTTP(sub, httptest.NewRequest(http.MethodGet, ref+"?v="+build, nil))
			if sub.Code != 200 {
				t.Errorf("%s imports %s which returns %d", p, ref, sub.Code)
			}
		}
	}
}

func TestShellReferencesItsAssets(t *testing.T) {
	// Catch an asset renamed without updating index.html: the page would load and
	// then silently do nothing.
	h, _ := Handler(nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	for _, ref := range []string{"/app.css", "/app.js", "/vendor/uPlot.min.css"} {
		if !strings.Contains(body, ref) {
			t.Errorf("index.html does not reference %s", ref)
		}
		sub := httptest.NewRecorder()
		h.ServeHTTP(sub, httptest.NewRequest(http.MethodGet, ref, nil))
		if sub.Code != 200 {
			t.Errorf("%s referenced by index.html but returns %d", ref, sub.Code)
		}
	}
}

// TestEveryAssetIsEmbedded guards the explicit go:embed list.
//
// The directive names each file, so adding a module and forgetting to list it
// compiles, serves, and 404s only in the browser — the failure appears as a blank
// page with one console line, nowhere near the change that caused it.
func TestEveryAssetIsEmbedded(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".js") && !strings.HasSuffix(name, ".css") &&
			!strings.HasSuffix(name, ".html") && !strings.HasSuffix(name, ".svg") {
			continue
		}
		if strings.HasSuffix(name, "_test.js") {
			continue
		}
		if _, err := assets.Open(name); err != nil {
			t.Errorf("%s exists on disk but is not embedded — add it to the go:embed directive", name)
		}
	}
}

// TestUnknownPathsAre404 closes a soft 404.
//
// Paths the router has no page for used to return the shell with status 200. The app
// rendered "Not found" correctly, so it looked right to a person and wrong to
// everything else: a crawler indexes the not-found page as content, and an agent
// probing for /openapi.json or /llms.txt is told it found one. On a site that invites
// automated clients in robots.txt and publishes an MCP endpoint, that matters.
func TestUnknownPathsAre404(t *testing.T) {
	h, err := Handler(nil)
	if err != nil {
		t.Fatal(err)
	}
	hit := func(p string) (int, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		return rec.Code, rec.Body.String()
	}

	// Real pages, including ones with a user-supplied segment.
	for _, p := range []string{
		"/", "/overview", "/teams", "/donors", "/api", "/agents", "/bots", "/search",
		"/privacy", "/disclaimer", "/teams/0", "/donors/Anonymous",
		"/donors/Mr.Hello", "/teams/0/rivals", "/blog/a-free-folding-at-home-stats-api",
	} {
		if code, _ := hit(p); code != http.StatusOK {
			t.Errorf("%s = %d, want 200", p, code)
		}
	}

	// Misses, whatever they look like. The manifest names are the ones an agent
	// actually probes for.
	for _, p := range []string{
		"/openapi.json", "/llms.txt", "/.well-known/ai-plugin.json", "/nope.png",
		"/no-such-page", "/teams/0/nonsense", "/blog", "/v2/summary",
	} {
		code, body := hit(p)
		if code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", p, code)
		}
		// Still the app, so a person gets the site's own not-found page.
		if !strings.Contains(body, "<title>") {
			t.Errorf("%s: 404 body is not the shell", p)
		}
	}
}

// TestClientRoutesComeFromAppJS pins the single source of truth.
//
// Restating the router's table in Go would work until someone adds a route to app.js
// and not here, at which point a working page answers 404 to every crawler while
// looking fine in a browser.
func TestClientRoutesComeFromAppJS(t *testing.T) {
	s, err := newSite()
	if err != nil {
		t.Fatal(err)
	}
	// Count the entries in app.js's table directly and require every one compiled.
	inJS := strings.Count(string(s.files["/app.js"]), "\n  [/^")
	if inJS == 0 {
		t.Fatal("could not find the route table in app.js; the extraction is looking for the wrong shape")
	}
	if len(s.routes) != inJS {
		t.Errorf("compiled %d routes, app.js declares %d", len(s.routes), inJS)
	}

	// And extraction failing quietly must not be survivable: every page 404ing is a
	// worse outcome than refusing to boot.
	if _, err := clientRoutes([]byte("const routes = [];")); err == nil {
		t.Error("clientRoutes accepted a table with no routes")
	}
}

// TestIconIsFingerprintedAndReal covers the favicon end to end.
//
// It is the one asset referenced from a link rather than imported by a module, so it
// sits outside the graph everything else is in: nothing would have failed if the
// go:embed line, the reference rewriter and the shell had disagreed about it. The
// symptom would have been a missing tab icon, which nobody files a bug about.
func TestIconIsFingerprintedAndReal(t *testing.T) {
	h, err := Handler(nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	shell := rec.Body.String()

	want := "/icon.svg?v=" + Build()
	if !strings.Contains(shell, want) {
		t.Errorf("the shell does not reference %s", want)
	}
	// The old mark was a DNA double helix, the wrong molecule for a project about
	// proteins folding. Nothing should put it back.
	if strings.Contains(shell, "\U0001f9ec") {
		t.Error("the DNA emoji is back in the shell")
	}

	icon := httptest.NewRecorder()
	h.ServeHTTP(icon, httptest.NewRequest(http.MethodGet, want, nil))
	if icon.Code != http.StatusOK {
		t.Fatalf("versioned icon: status %d", icon.Code)
	}
	if cc := icon.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("versioned icon is not immutable: %q", cc)
	}
	body := icon.Body.String()
	if !strings.HasPrefix(body, "<svg") || !strings.Contains(body, "</svg>") {
		t.Error("icon body is not an SVG document")
	}
	// The mark is drawn in the site accent; another colour means the asset and the
	// stylesheet have drifted apart.
	if !strings.Contains(body, "#3987e5") {
		t.Error("icon does not use the site accent")
	}
}

// TestFrontendNeverNamesItsOwnHost keeps the pages portable.
//
// The site moved from one domain to another and the frontend held the old name twice:
// in the relay's websocket URL, and — much worse — in the allowed-origins snippet the
// setup card tells a reader to paste into their folding client. That snippet is the one
// thing standing between somebody and a working page, and naming the wrong host in it
// produces the least debuggable failure available: they paste it, the client goes on
// refusing the connection, and nothing anywhere says why.
//
// Both are derivable, because a page always knows where it was served from. So the rule
// is narrow and permanent: the browser bundle may name any host in the world except one
// of ours. Naming other services is ordinary and stays unpoliced — an allowlist of them
// would only have to be extended by whoever adds the next link.
func TestFrontendNeverNamesItsOwnHost(t *testing.T) {
	names, err := scriptNames()
	if err != nil {
		t.Fatal(err)
	}
	// Every name this site has answered to. A retired one is worth keeping: it is
	// exactly what a copied-and-pasted snippet would still be carrying.
	ours := []string{"foldingstats.org", "folding.exec.codes"}

	for _, name := range names {
		src, err := assets.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range ours {
			if strings.Contains(string(src), h) {
				t.Errorf("%s names %q — derive it from location instead, or it breaks "+
					"silently the next time the site moves", name, h)
			}
		}
	}
}
