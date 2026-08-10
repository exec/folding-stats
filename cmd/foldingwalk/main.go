// Command foldingwalk reads the API the way a client would, at whatever rate you ask.
//
// It is not the same question as the other two load tools. cmd/loadtest runs a fixed
// number of workers flat out and reports the latency tail; cmd/loadramp raises the rate
// until something breaks. Both ask "how fast can this go". This one asks "what does a
// client that will not stop look like" — a steady, shaped, indefinite stream at a rate
// you choose, from one request a second to as many as the network will carry.
//
// The point of the dial is what comes next. Rate limiting has to be built against a
// client that behaves like a real one and simply asks for too much, not against a
// random URL fuzzer: it lists a page, searches every name on it, takes the next page,
// and wraps at the end. Those are the same two paths a mirror or a bot would use, in
// the order it would use them, so a limiter tuned against this is tuned against traffic
// that will actually turn up.
//
// The pacer is open loop. A ticker offers work at the configured rate whether or not
// the previous request has come back, and a pool of workers carries it — because the
// obvious design, one goroutine issuing a request and sleeping, silently caps at one
// over the round trip and reports a rate it never offered. Offered and achieved are
// both reported, and a gap between them is the finding rather than an error.
//
// It identifies itself in every request. A synthetic client wearing a browser's
// User-Agent is indistinguishable from real traffic in a log, which is exactly the
// confusion that stops an access log being evidence — and this is a tool for building
// something that will decide, from logs, who to turn away.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const userAgent = "foldingwalk/0.1 (+https://foldingstats.org; synthetic client, not a real reader)"

type envelope struct {
	Data json.RawMessage `json:"data"`
	Page *struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"page"`
}

type named struct {
	Name string `json:"name"`
}

func main() {
	var (
		base    = flag.String("url", "https://foldingstats.org", "base URL of the API")
		rate    = flag.Float64("rate", 1, "requests per second to offer")
		workers = flag.Int("workers", 0, "concurrent requests in flight (0 picks from the rate)")
		perPage = flag.Int("per-page", 10, "how many entries a listing page holds")
		start   = flag.Int("start", 1, "first page to walk (a high offset lands on paths no cache has seen)")
		kinds   = flag.String("kinds", "donors,teams", "which listings to walk, in order")
		dur     = flag.Duration("duration", 0, "stop after this long (0 runs until interrupted)")
		every   = flag.Duration("report", 15*time.Second, "how often to report")
		verbose = flag.Bool("v", false, "log every request")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *rate <= 0 {
		log.Error("rate must be above zero")
		os.Exit(1)
	}
	// Enough workers that the round trip is not the limit: at r requests a second and
	// a round trip of t, r*t are in flight at any moment. Assuming a slow 250ms and
	// leaving headroom costs idle goroutines, which are cheap; guessing low silently
	// caps the offered rate, which is the failure that makes the whole run a lie.
	if *workers <= 0 {
		*workers = int(math.Max(4, math.Ceil(*rate*0.25*2)))
	}

	w := &walker{
		base: strings.TrimRight(*base, "/"), perPage: *perPage, page: *start,
		log: log, verbose: *verbose,
		client: &http.Client{
			Timeout: 30 * time.Second,
			// The default pools two idle connections per host, so at any real rate
			// almost every request opens a new one — measuring TLS handshakes rather
			// than the API, and exhausting local ports on the way.
			Transport: &http.Transport{
				MaxIdleConns:        *workers * 2,
				MaxIdleConnsPerHost: *workers * 2,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
	for _, k := range strings.Split(*kinds, ",") {
		if k = strings.TrimSpace(k); k != "" {
			w.kinds = append(w.kinds, k)
		}
	}
	if len(w.kinds) == 0 {
		log.Error("no listings to walk")
		os.Exit(1)
	}

	log.Info("walking", "url", w.base, "rate", *rate, "workers", *workers,
		"per_page", w.perPage, "kinds", w.kinds)

	jobs := make(chan string, *workers*4)
	var wg sync.WaitGroup
	for range *workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				w.get(p)
			}
		}()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Above a thousand a second a per-request ticker spends more time waking up than
	// working, so the pacer batches: tick on a comfortable interval and offer the
	// whole batch due in it.
	interval := time.Duration(float64(time.Second) / *rate)
	batch := 1
	if interval < time.Millisecond {
		interval = time.Millisecond
		batch = int(math.Round(*rate / 1000))
		if batch < 1 {
			batch = 1
		}
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	report := time.NewTicker(*every)
	defer report.Stop()

	var deadline <-chan time.Time
	if *dur > 0 {
		t := time.NewTimer(*dur)
		defer t.Stop()
		deadline = t.C
	}

	started := time.Now()
	lastN, lastAt := int64(0), started
	finish := func() {
		close(jobs)
		wg.Wait()
		el := time.Since(started).Seconds()
		log.Info("done", "requests", w.n.Load(), "errors", w.errs.Load(),
			"offered_per_sec", *rate,
			"achieved_per_sec", math.Round(float64(w.n.Load())/el*100)/100,
			"behind", w.behind.Load(), "cache_hits", w.hits.Load())
	}

	for {
		select {
		case <-stop:
			finish()
			return
		case <-deadline:
			finish()
			return
		case <-report.C:
			n := w.n.Load()
			el := time.Since(lastAt).Seconds()
			log.Info("walking",
				"achieved_per_sec", math.Round(float64(n-lastN)/el*100)/100,
				"requests", n, "errors", w.errs.Load(),
				"behind", w.behind.Load(), "cache_hits", w.hits.Load(),
				"kind", w.kinds[w.kindIdx()], "page", w.pageNo())
			lastN, lastAt = n, time.Now()
		case <-tick.C:
			for range batch {
				select {
				case jobs <- w.next():
				default:
					// Every worker is busy and the queue is full: the server is not
					// keeping up with what was asked for. Counted rather than blocked
					// on, because blocking would quietly turn this back into a closed
					// loop and hide exactly the thing worth seeing.
					w.behind.Add(1)
				}
			}
		}
	}
}

type walker struct {
	base    string
	perPage int
	kinds   []string
	client  *http.Client
	log     *slog.Logger
	verbose bool

	mu    sync.Mutex
	kind  int
	page  int
	queue []string

	n      atomic.Int64
	errs   atomic.Int64
	hits   atomic.Int64
	behind atomic.Int64
}

func (w *walker) kindIdx() int { w.mu.Lock(); defer w.mu.Unlock(); return w.kind }
func (w *walker) pageNo() int  { w.mu.Lock(); defer w.mu.Unlock(); return w.page }

// next returns the path to request now.
//
// The walk stays sequential even though the requests are concurrent: the shape is what
// makes this traffic realistic, and a pool picking paths independently would just be a
// random sampler with extra steps.
func (w *walker) next() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.queue) > 0 {
		name := w.queue[0]
		w.queue = w.queue[1:]
		return "/v1/search?q=" + url.QueryEscape(name)
	}
	if w.page == 0 {
		w.page = 1
	}
	// Advanced here, not when the response lands. Waiting for absorb() meant every
	// worker that asked during a listing's round trip was handed the same URL — at two
	// thousand a second that is two hundred identical requests, one miss and the rest
	// answered by the CDN. The walk collapsed onto a single path and the origin saw
	// almost nothing, which is precisely the traffic this tool exists not to generate.
	page := w.page
	w.page++
	return fmt.Sprintf("/v1/%s?page=%d&per_page=%d", w.kinds[w.kind], page, w.perPage)
}

// absorb reads a listing response and queues the names on it. Called by whichever
// worker happened to fetch a listing.
func (w *walker) absorb(body []byte) {
	var env envelope
	if json.Unmarshal(body, &env) != nil {
		return
	}
	var rows []named
	json.Unmarshal(env.Data, &rows)

	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range rows {
		if r.Name != "" {
			w.queue = append(w.queue, r.Name)
		}
	}
	// An empty page is the end whatever the pagination claims, and total_pages is the
	// end when it is known. Either wraps to the next listing, and the last wraps to the
	// first — so this runs for as long as it is left running.
	//
	// The test is against what has been *handed out*, not against this response's own
	// page: with hundreds in flight the replies arrive out of order, and several will
	// report the end at once. Resetting page to 1 makes the condition false for the
	// stragglers, so a wrap happens once rather than skipping a listing per late reply.
	last := env.Page != nil && env.Page.TotalPages > 0 && w.page > env.Page.TotalPages
	if len(rows) == 0 || last {
		w.kind = (w.kind + 1) % len(w.kinds)
		w.page = 1
		w.queue = nil
		w.log.Info("wrapped", "now", w.kinds[w.kind], "requests", w.n.Load())
		return
	}
	w.page++
}

func (w *walker) get(path string) {
	req, err := http.NewRequest(http.MethodGet, w.base+path, nil)
	if err != nil {
		w.errs.Add(1)
		return
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	start := time.Now()
	resp, err := w.client.Do(req)
	if err != nil {
		w.n.Add(1)
		w.errs.Add(1)
		if w.verbose {
			w.log.Warn("request failed", "path", path, "err", err)
		}
		return
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	w.n.Add(1)
	if strings.EqualFold(resp.Header.Get("Cf-Cache-Status"), "HIT") {
		w.hits.Add(1)
	}
	if readErr != nil || resp.StatusCode != http.StatusOK {
		w.errs.Add(1)
		if w.verbose {
			w.log.Warn("bad response", "path", path, "status", resp.StatusCode, "err", readErr)
		}
		return
	}
	// Only a listing advances the walk; a search result is read and discarded, which is
	// what a client does with it too.
	if strings.Contains(path, "page=") {
		w.absorb(body)
	}
	if w.verbose {
		w.log.Info("ok", "path", path, "status", resp.StatusCode,
			"cf", resp.Header.Get("Cf-Cache-Status"), "took", time.Since(start).Round(time.Millisecond))
	}
}
