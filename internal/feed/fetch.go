package feed

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultUserAgent identifies us to upstream. Feed operators reasonably expect to be
// able to work out who is hitting them and get in touch if something is wrong.
//
// Set -user-agent in production to add a contact URL. The default deliberately
// carries none rather than a placeholder link: a dead URL in a User-Agent is worse
// than an honest description, because it looks like a way to reach someone.
const DefaultUserAgent = "folding-stats/0.1 (Folding@home statistics mirror)"

// Fetcher performs conditional GETs against the upstream feeds.
type Fetcher struct {
	Client    *http.Client
	UserAgent string
}

// NewFetcher returns a Fetcher with timeouts sized for the ~63 MB user feed on a
// slow link.
func NewFetcher(ua string) *Fetcher {
	if ua == "" {
		ua = DefaultUserAgent
	}
	return &Fetcher{
		Client:    &http.Client{Timeout: 10 * time.Minute},
		UserAgent: ua,
	}
}

// Validator carries the cache validators from a previous fetch. Sending these back
// turns an unchanged feed into a 304 with no body, which is what makes frequent
// polling cheap.
type Validator struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
}

// Result reports the outcome of a fetch. When NotModified is set, sink was never
// written to and Meta is zero.
type Result struct {
	Feed        Kind
	NotModified bool
	Meta        Meta
}

// Fetch issues a conditional GET for k and, when the feed has changed, streams the
// body into sink while computing its digest and capturing the leading timestamp
// line. The payload is never buffered in full — the user feed is ~63 MB and there is
// no reason to hold it in memory.
func (f *Fetcher) Fetch(ctx context.Context, k Kind, prev Validator, sink io.Writer) (Result, error) {
	if !k.Valid() {
		return Result{}, fmt.Errorf("feed: unknown kind %q", k)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.URL(), nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("User-Agent", f.UserAgent)
	// Ask for gzip explicitly rather than letting net/http negotiate it, so the
	// wire bytes stay measurable — transparent decompression hides them, and what we
	// take from upstream is exactly the number worth being able to see.
	//
	// This previously forced "identity", on the reasoning that we compress for
	// storage ourselves. That conflated storage with transfer. Upstream serves these
	// gzipped at roughly half the size (66.3 MB -> 32.6 MB for the user feed), and
	// cf-cache-status reports DYNAMIC — the CDN is not absorbing these requests, so
	// every byte we decline to compress is a byte their origin actually sends.
	req.Header.Set("Accept-Encoding", "gzip")
	if prev.ETag != "" {
		req.Header.Set("If-None-Match", prev.ETag)
	}
	if prev.LastModified != "" {
		req.Header.Set("If-Modified-Since", prev.LastModified)
	}

	resp, err := f.Client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("feed %s: %w", k, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return Result{Feed: k, NotModified: true}, nil
	case http.StatusOK:
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		// Upstream asking us to slow down is the one signal that matters more than
		// our own schedule. Treating it as a generic error would mean retrying on
		// the usual cadence — which, near a predicted publish, is once a minute.
		return Result{}, &BackoffError{
			Feed: k, Status: resp.StatusCode,
			RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
		}
	default:
		return Result{}, fmt.Errorf("feed %s: unexpected status %s", k, resp.Status)
	}

	meta := Meta{
		Feed:         k,
		URL:          k.URL(),
		FetchedAt:    time.Now().UTC(),
		LastModified: resp.Header.Get("Last-Modified"),
		ETag:         resp.Header.Get("ETag"),
	}
	meta.SnapshotAt = snapshotTime(meta.LastModified, meta.FetchedAt, meta.FetchedAt)

	// Count what crossed the wire before anything decompresses it.
	wire := &countingReader{r: resp.Body}
	var src io.Reader = wire
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(wire)
		if err != nil {
			return Result{}, fmt.Errorf("feed %s: gzip: %w", k, err)
		}
		defer zr.Close()
		src = zr
	}

	digest := sha256.New()
	counter := &countingWriter{}
	// Order matters: tee into the digest and byte counter as the body streams past,
	// so a single pass produces payload, checksum and size together.
	body := io.TeeReader(src, io.MultiWriter(digest, counter))

	br := bufio.NewReaderSize(body, 256<<10)
	first, err := peekFirstLine(br)
	if err != nil {
		return Result{}, fmt.Errorf("feed %s: reading header line: %w", k, err)
	}
	meta.FeedTimestamp = first

	if _, err := io.Copy(sink, br); err != nil {
		return Result{}, fmt.Errorf("feed %s: streaming body: %w", k, err)
	}

	meta.Bytes = counter.n
	meta.WireBytes = wire.n
	meta.SHA256 = hex.EncodeToString(digest.Sum(nil))
	return Result{Feed: k, Meta: meta}, nil
}

// peekFirstLine returns the first line without consuming it, so the caller still
// writes a byte-identical copy of the payload to storage.
func peekFirstLine(br *bufio.Reader) (string, error) {
	const max = 4 << 10
	buf, err := br.Peek(max)
	if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
		return "", err
	}
	if i := indexByte(buf, '\n'); i >= 0 {
		return strings.TrimRight(string(buf[:i]), "\r"), nil
	}
	return "", nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// futureSkewGrace is how far ahead of our own clock a Last-Modified may sit before it
// is treated as wrong rather than as news.
//
// Sized to absorb ordinary disagreement between two machines — a minute or two of NTP
// drift either way — while staying far below the hourly publish interval, so a header
// that is wrong by enough to matter can never be mistaken for one that is merely
// early.
const futureSkewGrace = 5 * time.Minute

// snapshotTime prefers upstream's Last-Modified, which identifies the publish rather
// than our discovery of it. Two archivers polling at different times must agree on a
// snapshot's identity, so falling back to fetch time is a last resort.
//
// A header dated in the future is refused. Snapshot instants become state.At, which is
// restored from MAX(ts) at startup, and ingest only accepts snapshots newer than it —
// so one bad timestamp does not cause a wrong reading, it stops ingest dead. Every
// subsequent real publish looks older than the future date and is skipped, silently,
// with no error and no recovery on restart, until wall clock catches up to whatever
// the header claimed. An origin two hours fast would cost two hours of history; a
// header with the wrong year would cost the site.
//
// Falling back to fetch time is right rather than merely safe: fetch time is what the
// field means when upstream does not say, and it is within seconds of the truth for a
// publish we polled for.
func snapshotTime(lastModified string, fallback time.Time, now time.Time) time.Time {
	if lastModified != "" {
		if t, err := http.ParseTime(lastModified); err == nil {
			if t.After(now.Add(futureSkewGrace)) {
				return fallback.UTC()
			}
			return t.UTC()
		}
	}
	return fallback.UTC()
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// BackoffError reports that upstream asked us to back off.
type BackoffError struct {
	Feed       Kind
	Status     int
	RetryAfter time.Duration
}

func (e *BackoffError) Error() string {
	return fmt.Sprintf("feed %s: upstream asked us to back off (status %d, retry after %v)",
		e.Feed, e.Status, e.RetryAfter)
}

// retryAfter parses the header in both forms the spec allows, falling back to a
// conservative wait when it is absent or unparseable. Erring long is the right
// direction: the whole point is to stop asking.
func retryAfter(h string) time.Duration {
	const fallback = 15 * time.Minute
	if h == "" {
		return fallback
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
		return 0
	}
	return fallback
}
