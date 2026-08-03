package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestServesAssetsAndRoutes(t *testing.T) {
	h, err := Handler()
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
		{"/vendor/uPlot.esm.js", 200, "text/javascript", "no-cache"},
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
	h, _ := Handler()
	build := Build()
	if build == "" {
		t.Fatal("Build() is empty")
	}
	for _, p := range []string{"/app.js", "/app.css", "/vendor/uPlot.esm.js"} {
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
	h, _ := Handler()
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
	h, _ := Handler()
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
			!strings.HasSuffix(name, ".html") {
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
