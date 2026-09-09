package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBlockBulkMetaCrawlers(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := blockBulkMetaCrawlers(next)
	metaUA := "Mozilla/5.0 Chrome/145.0.0.0 (compatible; meta-webindexer/1.1 (+https://developers.facebook.com/docs/sharing/webmasters/crawler))"

	for _, tc := range []struct {
		path, ua string
		want     int
	}{
		{"/v1/teams", "meta-externalagent/1.1", http.StatusForbidden},
		{"/teams/32", metaUA, http.StatusForbidden},
		{"/donors/DH", metaUA, http.StatusForbidden},
		{"/badge/donor/DH", "meta-externalagent/1.1", http.StatusForbidden},
		{"/badge/team/32?metric=rank", "Meta-ExternalAgent/1.1", http.StatusForbidden},
		{"/badge/team/32", metaUA, http.StatusForbidden},
		{"/badge/team/32", "Mozilla/5.0", http.StatusNoContent},
		{"/badge/donor/DH", "facebookexternalhit/1.1", http.StatusNoContent},
		{"/teams/32", "facebookexternalhit/1.1", http.StatusNoContent},
		{"/v1/teams", "Mozilla/5.0", http.StatusNoContent},
		{"/", metaUA, http.StatusNoContent},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("User-Agent", tc.ua)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s %q: status %d, want %d", tc.path, tc.ua, rec.Code, tc.want)
		}
		if tc.want == http.StatusForbidden && rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: crawler refusal must not be cached", tc.path)
		}
	}
}
