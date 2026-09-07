package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func unknownQuery(n int) string {
	var q strings.Builder
	for i := n; i > 0; i-- {
		fmt.Fprintf(&q, "unknown%05d=1&", i)
	}
	return q.String()
}

func TestQueryValidationBoundsInvalidResponses(t *testing.T) {
	srv := NewServer()
	for _, path := range []string{"/v1/summary", "/v1/teams", "/badge/team/32"} {
		for _, tc := range []struct{ name, query string }{
			{"many_keys", unknownQuery(8192)},
			{"over_parser_limit", unknownQuery(32000)},
			{"huge_single_key", strings.Repeat("x", 256<<10) + "=1"},
			{"escaped_single_key", strings.Repeat("%00", 4096) + "=1"},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path+"?"+tc.query, nil))
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400 before snapshot resolution", rec.Code)
				}
				if rec.Body.Len() > 1024 {
					t.Fatalf("invalid query produced a %d-byte response, want at most 1024", rec.Body.Len())
				}
				var problem APIError
				if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
					t.Fatal(err)
				}
				if problem.Status != 400 || problem.Error != "bad_request" || problem.Detail == "" || problem.Detail != problem.Message {
					t.Fatalf("invalid problem response: %+v", problem)
				}
			})
		}
	}
}

func TestQueryValidationPreservesValidQueriesAndDiagnostics(t *testing.T) {
	srv := fixture(t)
	for _, tc := range []struct{ path, want string }{
		{"/v1/summary", ""},
		{"/v1/teams?sort=lifetime&page=1&per_page=10", ""},
		{"/badge/team/32?metric=rank&name=1", ""},
		{"/badge/team/32?name=" + strings.Repeat("x", 256<<10), ""},
		{"/v1/summary?typo=1", "typo is not a parameter of this endpoint, which takes none"},
		{"/v1/teams?zzz=1&aaa=1", "aaa, zzz is not a parameter of this endpoint. It accepts: page, per_page, sort"},
		{"/badge/team/32?zzz=1&aaa=1", "aaa, zzz is not a parameter of this endpoint. It accepts: metric, name"},
		{"/v1/teams?sort=lifetime&page=1&per_page=10&typo=1", "typo is not a parameter of this endpoint. It accepts: page, per_page, sort"},
		{"/v1/teams?sort=lifetime&sort=today", "sort was given 2 times; each parameter may appear once"},
		{"/badge/team/32?metric=rank&metric=points", "metric was given 2 times; each parameter may appear once"},
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if tc.want == "" {
			if rec.Code != http.StatusOK {
				t.Fatalf("valid query: status = %d: %s", rec.Code, rec.Body.String())
			}
			continue
		}
		var problem APIError
		if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusBadRequest || problem.Detail != tc.want {
			t.Errorf("%s: status = %d, detail = %q; want 400, %q", tc.path, rec.Code, problem.Detail, tc.want)
		}
	}
}

func BenchmarkQueryValidationUnknownKeys(b *testing.B) {
	permitted := map[string]bool{"sort": true, "page": true, "per_page": true}
	allowed := []string{"sort", "page", "per_page"}
	for _, n := range []int{1000, 4000, 8000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			r := httptest.NewRequest(http.MethodGet, "/v1/teams?"+unknownQuery(n), nil)
			b.ReportAllocs()
			for b.Loop() {
				if err := checkQuery(r, permitted, allowed); err == nil {
					b.Fatal("unknown query accepted")
				}
			}
		})
	}
}
