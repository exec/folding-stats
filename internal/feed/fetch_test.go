package feed

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSnapshotTimeRefusesAFutureLastModified covers a header that would stop ingest
// rather than merely mis-date one snapshot.
//
// A snapshot instant becomes state.At, which is restored from MAX(ts) at startup, and
// only snapshots newer than it are ever applied. So one future-dated Last-Modified
// makes every subsequent real publish look older and get skipped — with no error, and
// with a restart making no difference, until wall clock reaches whatever the header
// claimed. Two hours fast costs two hours of history. A wrong year costs the site.
func TestSnapshotTimeRefusesAFutureLastModified(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	fetched := now.Add(-2 * time.Second)
	hdr := func(t time.Time) string { return t.UTC().Format(http.TimeFormat) }

	cases := []struct {
		name         string
		lastModified string
		want         time.Time
	}{
		// The ordinary case: upstream's own timestamp identifies the publish, and it
		// is what two archivers polling at different times have to agree on.
		{"an hour ago", hdr(now.Add(-time.Hour)), now.Add(-time.Hour)},
		{"seconds ago", hdr(now.Add(-3 * time.Second)), now.Add(-3 * time.Second)},
		// Slightly ahead is ordinary clock disagreement between two machines, not a
		// bad header. Refusing it would throw away a correct publish identity over
		// NTP drift.
		{"a minute ahead", hdr(now.Add(time.Minute)), now.Add(time.Minute)},
		{"four minutes ahead", hdr(now.Add(4 * time.Minute)), now.Add(4 * time.Minute)},
		// Past the grace it is wrong, whatever caused it.
		{"an hour ahead", hdr(now.Add(time.Hour)), fetched},
		{"two hours ahead", hdr(now.Add(2 * time.Hour)), fetched},
		{"next year", hdr(now.AddDate(1, 0, 0)), fetched},
		// Absent or unparseable falls back the way it always did.
		{"absent", "", fetched},
		{"garbage", "not a date", fetched},
	}
	for _, c := range cases {
		got := snapshotTime(c.lastModified, fetched, now)
		if !got.Equal(c.want.UTC()) {
			t.Errorf("%s: got %s, want %s", c.name, got.Format(time.RFC3339), c.want.UTC().Format(time.RFC3339))
		}
	}
}

func TestFetchBoundsWireAndDecodedBodies(t *testing.T) {
	oldWire, oldDecoded := maxWireBytes, maxDecodedBytes
	maxWireBytes, maxDecodedBytes = 64, 64
	t.Cleanup(func() { maxWireBytes, maxDecodedBytes = oldWire, oldDecoded })

	t.Run("wire", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, "ts\n"+strings.Repeat("x", 80))
		}))
		defer srv.Close()
		f := NewFetcher("test")
		f.Client = &http.Client{Transport: rewriteTransport{base: srv.URL}}
		if _, err := f.Fetch(context.Background(), Teams, Validator{}, io.Discard); err == nil ||
			(!strings.Contains(err.Error(), "wire body exceeds") && !strings.Contains(err.Error(), "limit is")) {
			t.Fatalf("Fetch error = %v, want wire limit", err)
		}
	})

	t.Run("decoded", func(t *testing.T) {
		var compressed bytes.Buffer
		zw := gzip.NewWriter(&compressed)
		io.WriteString(zw, "ts\n"+strings.Repeat("x", 80))
		zw.Close()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Write(compressed.Bytes())
		}))
		defer srv.Close()
		f := NewFetcher("test")
		f.Client = &http.Client{Transport: rewriteTransport{base: srv.URL}}
		if _, err := f.Fetch(context.Background(), Teams, Validator{}, io.Discard); err == nil || !strings.Contains(err.Error(), "decoded body exceeds") {
			t.Fatalf("Fetch error = %v, want decoded limit", err)
		}
	})
}
