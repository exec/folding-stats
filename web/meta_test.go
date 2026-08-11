package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// get fetches a path through a handler built with the given metadata source.
func get(t *testing.T, meta MetaFunc, path string) *httptest.ResponseRecorder {
	t.Helper()
	h, err := Handler(meta)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = "foldingstats.org"
	h.ServeHTTP(rec, r)
	return rec
}

// TestMetaReachesTheShell is the whole point: the head a crawler reads has to differ
// per URL. It rendered correctly in a browser the entire time it was broken.
func TestMetaReachesTheShell(t *testing.T) {
	meta := func(p string) (Meta, bool) {
		if p == "/donors/DH" {
			return Meta{Title: "DH — donor", Description: "DH is ranked #1."}, true
		}
		return Meta{}, false
	}

	rec := get(t, meta, "/donors/DH")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<title>DH — donor</title>",
		`<meta name="description" content="DH is ranked #1.">`,
		`<link rel="canonical" href="https://foldingstats.org/donors/DH">`,
		`<meta property="og:title" content="DH — donor">`,
		`<meta property="og:url" content="https://foldingstats.org/donors/DH">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing from the shell: %s", want)
		}
	}
	if strings.Contains(body, "<title>Folding@home Stats</title>") {
		t.Error("the default title is still present; there are now two titles in one document")
	}
	// The rest of the document has to survive the surgery.
	if !strings.Contains(body, "<body>") || !strings.HasPrefix(body, "<!doctype html>") {
		t.Error("the shell was damaged by the metadata splice")
	}

	// A path the lookup declines keeps the site's own copy rather than an empty head.
	other := get(t, meta, "/teams").Body.String()
	if !strings.Contains(other, "<title>Folding@home Stats</title>") {
		t.Error("a page with no metadata lost its title entirely")
	}

	// Nil lookup is the old behaviour exactly: defaults everywhere, nothing broken.
	if b := get(t, nil, "/donors/DH").Body.String(); !strings.Contains(b, "<title>Folding@home Stats</title>") {
		t.Error("with no metadata source the shell should serve its defaults")
	}
}

// TestMetaIsEscaped guards the injection point. A donor name is arbitrary upstream
// text that lands in both element content and attribute values.
func TestMetaIsEscaped(t *testing.T) {
	evil := `x"><script>alert(1)</script>`
	body := get(t, func(string) (Meta, bool) {
		return Meta{Title: evil, Description: evil}, true
	}, "/donors/x").Body.String()

	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("a name closed out of its attribute and injected a script tag")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("the name was not escaped at all")
	}
}

// TestCanonicalEncodesTheName keeps a canonical URL a URL. 2.69% of donor names carry
// a dot and plenty carry spaces and brackets; an unescaped one is not a link.
func TestCanonicalEncodesTheName(t *testing.T) {
	body := get(t, func(string) (Meta, bool) {
		return Meta{Title: "t", Description: "d"}, true
	}, "/donors/Some%20Name%5Bfr%5D").Body.String()

	if !strings.Contains(body, `href="https://foldingstats.org/donors/Some%20Name`) {
		i := strings.Index(body, "canonical")
		t.Errorf("canonical is not percent-encoded: %s", body[i:min(i+90, len(body))])
	}
}

// TestNotFoundIsNoIndex: a page that does not exist must not be indexed, and the
// status alone does not stop every crawler.
func TestNotFoundIsNoIndex(t *testing.T) {
	rec := get(t, nil, "/no-such-page")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `content="noindex,follow"`) {
		t.Error("the not-found shell is missing its noindex directive")
	}
}
