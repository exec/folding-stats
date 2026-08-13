package feed

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// payload mimics the real feed: a timestamp line, a header, then TSV rows —
// including the embedded-newline record that a naive line reader corrupts.
const payload = "Sun Aug 02 21:29:05 GMT 2026\n" +
	"team\tteamname\tscore\twu\n" +
	"0\tDefault (No team specified)\t8213749748944\t364747804\n" +
	"151775\tdiscworld\n\t3448577\t434\n" +
	"182116\tAtheists, Skeptics, & Humanists  -  ASH Folding\t251359529740\t2884335\n"

const lastMod = "Sun, 02 Aug 2026 21:29:06 GMT"

func testServer(t *testing.T, hits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.Header().Set("ETag", `"abc123"`)
		w.Header().Set("Last-Modified", lastMod)
		if r.Header.Get("If-None-Match") == `"abc123"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		io.WriteString(w, payload)
	}))
}

// fetchFrom points a Fetcher at the test server regardless of Kind.
type rewriteTransport struct{ base string }

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	u := *r.URL
	target, _ := http.NewRequest(r.Method, rt.base+u.Path, nil)
	target.Header = r.Header
	return http.DefaultTransport.RoundTrip(target)
}

func TestStoreRoundTripPreservesBytes(t *testing.T) {
	hits := 0
	srv := testServer(t, &hits)
	defer srv.Close()

	a := &Archive{Root: t.TempDir()}
	f := NewFetcher("test")
	f.Client = &http.Client{Transport: rewriteTransport{base: srv.URL}}

	res, err := a.Store(context.Background(), f, Teams, Validator{})
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if res.NotModified {
		t.Fatal("first fetch should not be NotModified")
	}

	// The archive must be byte-identical to what upstream served: any normalisation
	// here would be baked in permanently and could not be undone by a later replay.
	snaps, err := a.List(Teams)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("List: %v, got %d snapshots", err, len(snaps))
	}
	rc, err := snaps[0].Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != payload {
		t.Errorf("round-trip altered payload:\n got %q\nwant %q", got, payload)
	}
}

func TestStoreCapturesMetadata(t *testing.T) {
	hits := 0
	srv := testServer(t, &hits)
	defer srv.Close()

	a := &Archive{Root: t.TempDir()}
	f := NewFetcher("test")
	f.Client = &http.Client{Transport: rewriteTransport{base: srv.URL}}

	res, err := a.Store(context.Background(), f, Teams, Validator{})
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	// The leading timestamp line identifies the snapshot upstream; it must survive
	// verbatim rather than being parsed into a time.Time at capture.
	if want := "Sun Aug 02 21:29:05 GMT 2026"; res.Meta.FeedTimestamp != want {
		t.Errorf("FeedTimestamp = %q, want %q", res.Meta.FeedTimestamp, want)
	}
	if res.Meta.Bytes != int64(len(payload)) {
		t.Errorf("Bytes = %d, want %d", res.Meta.Bytes, len(payload))
	}
	if res.Meta.SHA256 == "" {
		t.Error("SHA256 not computed")
	}
	// SnapshotAt must track upstream's publish time, not our fetch time, so two
	// archivers polling at different moments agree on a snapshot's identity.
	want, _ := http.ParseTime(lastMod)
	if !res.Meta.SnapshotAt.Equal(want.UTC()) {
		t.Errorf("SnapshotAt = %v, want %v", res.Meta.SnapshotAt, want.UTC())
	}
}

func TestConditionalGetSkipsUnchanged(t *testing.T) {
	hits := 0
	srv := testServer(t, &hits)
	defer srv.Close()

	a := &Archive{Root: t.TempDir()}
	f := NewFetcher("test")
	f.Client = &http.Client{Transport: rewriteTransport{base: srv.URL}}

	first, err := a.Store(context.Background(), f, Teams, Validator{})
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	v := Validator{ETag: first.Meta.ETag, LastModified: first.Meta.LastModified}
	second, err := a.Store(context.Background(), f, Teams, v)
	if err != nil {
		t.Fatalf("Store (conditional): %v", err)
	}
	if !second.NotModified {
		t.Error("second fetch should have been NotModified")
	}

	// An unchanged feed must not produce a duplicate snapshot.
	snaps, _ := a.List(Teams)
	if len(snaps) != 1 {
		t.Errorf("got %d snapshots, want 1", len(snaps))
	}
}

func TestHasDetectsExistingSnapshot(t *testing.T) {
	hits := 0
	srv := testServer(t, &hits)
	defer srv.Close()

	a := &Archive{Root: t.TempDir()}
	f := NewFetcher("test")
	f.Client = &http.Client{Transport: rewriteTransport{base: srv.URL}}

	at, _ := http.ParseTime(lastMod)
	if a.Has(Teams, at) {
		t.Error("Has reported a snapshot before anything was stored")
	}
	if _, err := a.Store(context.Background(), f, Teams, Validator{}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if !a.Has(Teams, at) {
		t.Error("Has did not find the stored snapshot")
	}
}

func TestListIsChronological(t *testing.T) {
	a := &Archive{Root: t.TempDir()}
	base := time.Date(2026, 8, 2, 21, 0, 0, 0, time.UTC)
	for _, off := range []time.Duration{2 * time.Hour, 0, time.Hour} {
		writeStub(t, a, Teams, base.Add(off))
	}
	snaps, err := a.List(Teams)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(snaps) != 3 {
		t.Fatalf("got %d snapshots, want 3", len(snaps))
	}
	for i := 1; i < len(snaps); i++ {
		if !snaps[i-1].Meta.SnapshotAt.Before(snaps[i].Meta.SnapshotAt) {
			t.Errorf("snapshots out of order at %d: %v then %v",
				i, snaps[i-1].Meta.SnapshotAt, snaps[i].Meta.SnapshotAt)
		}
	}
}

func TestPeekFirstLineHandlesMissingNewline(t *testing.T) {
	// A truncated or empty feed must not panic or block the archive.
	for _, in := range []string{"", "no trailing newline"} {
		br := bufio.NewReaderSize(strings.NewReader(in), 1<<10)
		got, err := peekFirstLine(br)
		if err != nil {
			t.Errorf("peekFirstLine(%q): %v", in, err)
		}
		if got != "" {
			t.Errorf("peekFirstLine(%q) = %q, want empty", in, got)
		}
	}
}

func TestStoreRefusesTimestampCollisionWithoutOverwriting(t *testing.T) {
	body := payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Last-Modified", lastMod)
		io.WriteString(w, body)
	}))
	defer srv.Close()
	a := &Archive{Root: t.TempDir()}
	f := NewFetcher("test")
	f.Client = &http.Client{Transport: rewriteTransport{base: srv.URL}}
	if _, err := a.Store(context.Background(), f, Teams, Validator{}); err != nil {
		t.Fatal(err)
	}
	body = strings.Replace(payload, "8213749748944", "999", 1)
	if _, err := a.Store(context.Background(), f, Teams, Validator{}); err == nil || !strings.Contains(err.Error(), "conflicting snapshot") {
		t.Fatalf("second Store error = %v, want collision", err)
	}
	snaps, _ := a.List(Teams)
	r, err := snaps[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(r)
	r.Close()
	if readErr != nil || string(got) != payload {
		t.Fatalf("original snapshot changed: err=%v body=%q", readErr, got)
	}
}

func TestPutRefusesTimestampCollisionWithoutOverwriting(t *testing.T) {
	a := &Archive{Root: t.TempDir()}
	at := time.Now().UTC()
	if _, err := a.Put(Teams, at, strings.NewReader("original")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Put(Teams, at, strings.NewReader("replacement")); err == nil {
		t.Fatal("conflicting import overwrote an existing snapshot")
	}
	snaps, _ := a.List(Teams)
	r, err := snaps[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(got) != "original" {
		t.Fatalf("original import changed: err=%v body=%q", err, got)
	}
}

func TestArchiveReplayBoundsAndAuthenticatesDecodedData(t *testing.T) {
	a := &Archive{Root: t.TempDir()}
	at := time.Now().UTC()
	if _, err := a.Put(Teams, at, strings.NewReader(strings.Repeat("x", 80))); err != nil {
		t.Fatal(err)
	}
	snaps, _ := a.List(Teams)

	old := maxDecodedBytes
	t.Cleanup(func() { maxDecodedBytes = old })
	maxDecodedBytes = 32
	r, err := snaps[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(r)
	r.Close()
	maxDecodedBytes = old
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized replay error = %v", err)
	}

	snaps[0].Meta.SHA256 = strings.Repeat("0", 64)
	r, err = snaps[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(r)
	r.Close()
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("tampered replay error = %v", err)
	}
}

// writeStub places a minimal snapshot into the archive without going over HTTP.
func writeStub(t *testing.T, a *Archive, k Kind, at time.Time) {
	t.Helper()
	dir := a.dir(at)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := a.base(k, at)
	m := Meta{Feed: k, SnapshotAt: at.UTC(), FetchedAt: at.UTC()}
	if err := writeJSON(filepath.Join(dir, base+metaExt), m); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, base+payloadExt))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, _ := zstd.NewWriter(f)
	io.WriteString(w, payload)
	w.Close()
}
