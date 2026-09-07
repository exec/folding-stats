package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConditionalValidators(t *testing.T) {
	srv := fixture(t)
	for _, path := range []string{"/v1/summary", "/badge/team/32"} {
		var plainETag string
		for _, encoding := range []string{"", "gzip"} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set("Accept-Encoding", encoding)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, r)
			etag := rec.Header().Get("ETag")
			if rec.Code != http.StatusOK || etag == "" {
				t.Fatalf("%s: status %d, ETag %q", path, rec.Code, etag)
			}
			if encoding == "" {
				plainETag = etag
			} else if etag == plainETag {
				t.Fatalf("%s shares a validator across encodings", path)
			}
			for _, match := range []string{etag, "W/" + etag, `"old", W/` + etag} {
				r.Header.Set("If-None-Match", match)
				rec = httptest.NewRecorder()
				srv.ServeHTTP(rec, r)
				if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 || rec.Header().Get("Vary") != "Accept-Encoding" {
					t.Errorf("%s %s: status %d, body %d bytes, Vary %q", path, match, rec.Code, rec.Body.Len(), rec.Header().Get("Vary"))
				}
			}
		}
	}
	// This endpoint reports the current clock and traffic even without a new feed.
	r := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	r.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("status was revalidated: status %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}
