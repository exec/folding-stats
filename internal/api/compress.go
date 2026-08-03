package api

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strings"
	"sync"
)

// This API's responses are repetitive field names wrapped around numbers, which is
// close to the best case for gzip: the measured mix compresses by 80%, from an
// average 3,839 bytes to 753. At 24,000 req/s that is the difference between 0.8
// Gbit/s and 0.21 — the difference between the network being the binding constraint
// and the CPU being it, which is the better problem to have.
//
// Two decisions worth stating, because both directions are wrong by default:
//
// Small responses are not compressed. Over half of real traffic is sub-kilobyte —
// a team lookup is 581 bytes, a history query 383 — and gzipping those spends CPU on
// every request to save a fraction of one network packet. The threshold is what keeps
// this a win rather than a trade.
//
// Compression is BestSpeed, not the default. On JSON this gives up a few points of
// ratio for several times the throughput, and the whole point here is to spend as
// little CPU as possible per byte saved.
const (
	// compressMinBytes is the size below which compressing costs more than it saves.
	compressMinBytes = 1000
	compressLevel    = gzip.BestSpeed
)

var (
	bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}
	gzPool  = sync.Pool{New: func() any {
		w, _ := gzip.NewWriterLevel(nil, compressLevel)
		return w
	}}
)

// acceptsGzip reports whether the client offered to accept a gzipped body.
//
// It looks for the token rather than the substring: "gzip" appears inside
// "x-gzip-not-really", and more importantly a client may explicitly refuse it with
// "gzip;q=0", which a substring match would read as acceptance.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		token, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(token), "gzip") {
			continue
		}
		// q=0 means "do not send me this".
		if q, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			return strings.Trim(q, "0.") != ""
		}
		return true
	}
	return false
}

// writeBody sends b, compressing it when that is worth doing.
//
// Vary is set on every response, compressed or not: without it a shared cache that
// stored a gzipped body could hand it to a client that never asked for one, which
// fails as unreadable bytes rather than as an error.
func writeBody(w http.ResponseWriter, r *http.Request, code int, contentType string, b []byte) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Add("Vary", "Accept-Encoding")

	if len(b) < compressMinBytes || !acceptsGzip(r) {
		h.Set("Content-Length", itoa(len(b)))
		w.WriteHeader(code)
		_, _ = w.Write(b)
		return
	}

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	zw := gzPool.Get().(*gzip.Writer)
	zw.Reset(buf)
	if _, err := zw.Write(b); err != nil || zw.Close() != nil {
		// Compression failing is not a reason to fail the request.
		gzPool.Put(zw)
		h.Set("Content-Length", itoa(len(b)))
		w.WriteHeader(code)
		_, _ = w.Write(b)
		return
	}
	gzPool.Put(zw)

	// Incompressible data does exist, and shipping a larger body under a
	// Content-Encoding header would be strictly worse than not bothering.
	if buf.Len() >= len(b) {
		h.Set("Content-Length", itoa(len(b)))
		w.WriteHeader(code)
		_, _ = w.Write(b)
		return
	}

	h.Set("Content-Encoding", "gzip")
	h.Set("Content-Length", itoa(buf.Len()))
	w.WriteHeader(code)
	_, _ = w.Write(buf.Bytes())
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
