package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"folding/content"
)

const (
	defaultPerPage = 100
	maxPerPage     = 1000
)

// Server serves the API from whichever Snapshot is currently published.
//
// Handlers hold no locks: a cycle builds a new Snapshot and swaps the pointer, so
// requests in flight finish against the version they started with.
type Server struct {
	snap atomic.Pointer[Snapshot]
	mux  *http.ServeMux
}

// NewServer returns a Server with no snapshot published yet; requests will report
// 503 until Publish is called.
func NewServer() *Server {
	s := &Server{mux: http.NewServeMux()}
	s.routes()
	return s
}

// Publish makes snap the version served by subsequent requests.
func (s *Server) Publish(snap *Snapshot) { s.snap.Store(snap) }

// Current returns the published snapshot, or nil.
func (s *Server) Current() *Snapshot { return s.snap.Load() }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /v1/status", s.handle(s.status))
	s.mux.HandleFunc("GET /v1/summary", s.handle(s.summary))
	s.mux.HandleFunc("GET /v1/summary/history", s.handle(s.projectHistory))
	s.mux.HandleFunc("GET /v1/posts", s.handle(s.posts))
	s.mux.HandleFunc("GET /v1/posts/{slug}", s.handle(s.post))

	s.mux.HandleFunc("GET /v1/teams", s.handle(s.teams))
	s.mux.HandleFunc("GET /v1/teams/{id}", s.handle(s.team))
	s.mux.HandleFunc("GET /v1/teams/{id}/members", s.handle(s.teamMembers))
	s.mux.HandleFunc("GET /v1/teams/{id}/history", s.handle(s.teamHistory))
	s.mux.HandleFunc("GET /v1/teams/{id}/rivals", s.handle(s.teamRivals))

	s.mux.HandleFunc("GET /v1/donors", s.handle(s.donors))
	s.mux.HandleFunc("GET /v1/donors/{name}", s.handle(s.donor))
	s.mux.HandleFunc("GET /v1/donors/{name}/teams", s.handle(s.donorTeams))
	s.mux.HandleFunc("GET /v1/donors/{name}/history", s.handle(s.donorHistory))
	s.mux.HandleFunc("GET /v1/donors/{name}/rivals", s.handle(s.donorRivals))

	s.mux.HandleFunc("GET /v1/search", s.handle(s.search))

	// Incremental sync. Deliberately alongside the collections rather than under them:
	// it is a different question about all of them, not a sub-resource of any one.
	s.mux.HandleFunc("GET /v1/changes", s.handle(s.changes))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The API is public and unauthenticated by design (R3), so it is usable
	// directly from a browser or a notebook without a proxy.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	s.mux.ServeHTTP(w, r)
}

// handlerFunc returns the payload to wrap, plus optional pagination.
type handlerFunc func(*Snapshot, *http.Request) (any, *PageInfo, error)

// handle wraps a handler with snapshot resolution, caching headers and the envelope.
func (s *Server) handle(fn handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap := s.snap.Load()
		if snap == nil {
			writeError(w, http.StatusServiceUnavailable, "no_data",
				"no snapshot has been ingested yet")
			return
		}

		// Data is immutable between cycles, so a conditional request can be
		// answered without touching any of it.
		//
		// The validator identifies the response, which is a function of the snapshot
		// *and* of the code that derived it — every figure here is computed, not
		// stored. Keyed on the snapshot alone, a deploy that changed a derivation
		// left every cached copy answering 304 against numbers that no longer
		// existed: the client asks whether anything changed, is told no, and keeps
		// the old answer until upstream happens to publish. That was observed —
		// changing the per-day divisor took effect on the origin instantly and was
		// invisible to anything holding a cached copy.
		//
		// Posts were already exempted for this reason, but the reasoning was never
		// theirs alone; they only made it obvious first, because their content is
		// visibly authored rather than derived.
		etag := `"` + buildID() + "-" + snap.ETag + `"`
		if isPosts(r.URL.Path) {
			etag = `"` + buildID() + "-posts-" + content.Fingerprint() + `"`
		} else {
			w.Header().Set("Last-Modified", snap.At.UTC().Format(http.TimeFormat))
		}
		// A gzipped body is a different entity from the same JSON uncompressed, so
		// it needs a different validator. Vary tells a well-behaved cache to key on
		// Accept-Encoding; this makes a cache that ignores Vary fail safely too,
		// since the ETags will simply not match.
		//
		// Keyed on what the client *asked for*, not on what we ended up sending:
		// whether a body cleared the compression threshold is not something the
		// client can know, and an ETag that varied with it would be unstable.
		if acceptsGzip(r) {
			etag = etag[:len(etag)-1] + `-gz"`
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", cacheControl(snap, r.URL.Path))
		if match := r.Header.Get("If-None-Match"); match == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		// The snapshot points at live ingest structures, so every read of them —
		// including the view builders — happens under the read lock.
		//
		// The lock is scoped to exactly those reads and released before anything is
		// marshalled or written. It used to be held by a deferred unlock for the whole
		// handler, which put the response write inside it: a client that asked for
		// per_page=1000 and then stopped reading blocked in w.Write, holding the read
		// lock until WriteTimeout. Ingest's writer then queued behind it, and Go's
		// RWMutex blocks new readers once a writer is waiting — so every later request
		// blocked too, including /v1/status. One connection renewed every 59 seconds
		// was an unauthenticated outage from a single host.
		//
		// This is safe because the payload stops aliasing guarded state at the moment
		// fn returns: the view builders copy scalars and take Names.Name(), which
		// allocates its own string, so nothing in `data` points back into the arena or
		// the windows. warmingUp is the one straggler — it reads the live window — so
		// it is evaluated here rather than in the Envelope literal below.
		var (
			data any
			page *PageInfo
			warm *WarmingUp
			err  error
		)
		func() {
			if snap.Guard != nil {
				snap.Guard.RLock()
				defer snap.Guard.RUnlock()
			}
			if data, page, err = fn(snap, r); err != nil {
				return
			}
			warm = warmingUp(snap)
		}()
		if err != nil {
			writeAPIError(w, err)
			return
		}

		// Staleness is a function of the clock, not of the snapshot, so it is decided
		// here rather than baked in at publish time. Computed at publish it would be
		// wrong until the next periodic republish — up to five minutes of claiming
		// data is current after it demonstrably is not.
		now := time.Now().UTC()

		writeJSON(w, r, http.StatusOK, Envelope{
			Snapshot: SnapshotInfo{
				At:             snap.At.UTC(),
				NextExpectedAt: snap.NextExpected.UTC(),
				Stale:          !snap.StaleAfter.IsZero() && now.After(snap.StaleAfter),
				ServerTime:     now,
				WarmingUp:      warm,
			},
			Data: data,
			Page: page,
		})
	}
}

// cacheControl lets clients cache until the next upstream publish is due. Once that
// passes without an update the data is stale, and a short max-age keeps clients
// checking back rather than pinning a stale copy.
//
// /v1/status is exempt. It exists to answer "has anything changed yet", so a cached
// answer is not a cheap answer but a wrong one: a client polling it would be served
// its own last result and never see the publish it is waiting for. It is also the
// cheapest route we have, served from memory, so there is nothing to save.
// warmingUp returns the qualifier block, or nil once nothing needs qualifying.
//
// Both figures converge and then stay converged forever, so shipping them on every
// response for the life of the site would be ~50 bytes of permanent constant on
// payloads that are frequently under a kilobyte.
func warmingUp(snap *Snapshot) *WarmingUp {
	w := WarmingUp{IntervalEstimated: !snap.IntervalMeasured}
	if !snap.AvgWindowComplete() {
		w.HistorySpanSec = int64(snap.Members.Span().Seconds())
	}
	_, haveBaseline := snap.Members.Baseline()
	w.RankChange24hUnavailable = !haveBaseline
	if w.HistorySpanSec == 0 && !w.IntervalEstimated && !w.RankChange24hUnavailable {
		return nil
	}
	return &w
}

func isPosts(path string) bool {
	return path == "/v1/posts" || strings.HasPrefix(path, "/v1/posts/")
}

// publishMargin is how far before the predicted publish a cached copy expires.
//
// Sized from the prediction error actually observed, not chosen for roundness:
// upstream intervals run 3606–3613s and the median estimator lands within roughly ten
// seconds of the real instant in either direction. Thirty seconds covers that with
// room, and is small against an hourly cadence.
const publishMargin = 30 * time.Second

func cacheControl(snap *Snapshot, path string) string {
	if path == "/v1/status" {
		return "no-store"
	}
	// Revalidate rather than expire: the ETag makes an unchanged post a 304 with no
	// body, and a deploy that edits one takes effect immediately instead of after
	// however long the data cache happened to have left.
	if isPosts(path) {
		return "no-cache"
	}
	// Expire a little before the publish is due rather than exactly on it. The
	// estimate is a median of recent intervals and lands within about ten seconds
	// either side of the truth — late is harmless, since the cache expires early and
	// revalidates into a 304, but early means serving a snapshot that has already
	// been superseded. The margin costs one conditional request per cache per cycle
	// and removes the only case where the TTL is wrong rather than merely
	// conservative.
	secs := int(time.Until(snap.NextExpected).Seconds()) - int(publishMargin.Seconds())
	if secs < 30 {
		secs = 30
	}
	if secs > 3600 {
		secs = 3600
	}
	return fmt.Sprintf("public, max-age=%d", secs)
}

// writeJSON marshals into a pooled buffer rather than streaming into the
// ResponseWriter, because compression needs to know the size before it can decide
// whether compressing is worth it. Everything served here is already resident in
// memory, so holding one response briefly costs nothing.
func writeJSON(w http.ResponseWriter, r *http.Request, code int, v any) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		http.Error(w, "encoding response", http.StatusInternalServerError)
		return
	}
	writeBody(w, r, code, "application/json; charset=utf-8", buf.Bytes())
}

// writeError writes plainly. Error bodies are a couple of hundred bytes and would
// never clear the compression threshold, so they do not need the request in hand.
func writeError(w http.ResponseWriter, code int, kind, msg string) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(APIError{Error: kind, Message: msg})

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", itoa(buf.Len()))
	w.WriteHeader(code)
	_, _ = w.Write(buf.Bytes())
}

// statusError carries an HTTP status alongside a message.
type statusError struct {
	code int
	kind string
	msg  string
}

func (e *statusError) Error() string { return e.msg }

func notFound(format string, args ...any) error {
	return &statusError{http.StatusNotFound, "not_found", fmt.Sprintf(format, args...)}
}

func badRequest(format string, args ...any) error {
	return &statusError{http.StatusBadRequest, "bad_request", fmt.Sprintf(format, args...)}
}

func writeAPIError(w http.ResponseWriter, err error) {
	if se, ok := err.(*statusError); ok {
		writeError(w, se.code, se.kind, se.msg)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal", err.Error())
}

// paginate resolves page/per_page and returns the slice bounds for n items.
func paginate(r *http.Request, n int) (lo, hi int, info *PageInfo, err error) {
	return paginateAround(r, n, 0)
}

// paginateAround is paginate with the default page chosen to contain anchor, a 1-based
// item position. Zero means no anchor, and the default is page 1 as usual.
//
// A collection anchored on one of its own members should open where that member is.
// The rivals view is a slice of the leaderboard around one team, and opening it at
// rank 1 would answer a question nobody asked — an explicit ?page= still means exactly
// what it says, so the pager works from there like any other.
func paginateAround(r *http.Request, n, anchor int) (lo, hi int, info *PageInfo, err error) {
	perPage, err := intParam(r, "per_page", defaultPerPage)
	if err != nil {
		return 0, 0, nil, err
	}
	defaultPage := 1
	if anchor > 0 && perPage > 0 {
		defaultPage = (anchor-1)/perPage + 1
	}
	page, err := intParam(r, "page", defaultPage)
	if err != nil {
		return 0, 0, nil, err
	}
	if page < 1 {
		return 0, 0, nil, badRequest("page must be >= 1")
	}
	if perPage < 1 || perPage > maxPerPage {
		return 0, 0, nil, badRequest("per_page must be between 1 and %d", maxPerPage)
	}

	totalPages := (n + perPage - 1) / perPage

	// A page past the end is an empty page, not an error — but it must not be
	// multiplied out first. page is only bounded below, so `?page=9223372036854775807`
	// overflowed (page-1)*perPage into a negative offset that survived the `lo > n`
	// clamp and reached the caller's slice expression as a negative bound: a one-URL
	// unauthenticated panic on every paginated route. Doing the multiply only for
	// pages that exist keeps it inside n by construction.
	lo = n
	if page <= totalPages {
		lo = (page - 1) * perPage
	}
	hi = lo + perPage
	if hi > n {
		hi = n
	}
	return lo, hi, &PageInfo{
		Page: page, PerPage: perPage, TotalItems: n, TotalPages: totalPages,
	}, nil
}

func intParam(r *http.Request, name string, def int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, badRequest("%s must be an integer", name)
	}
	return v, nil
}
