// Command loadramp finds the rate at which the API stops keeping up.
//
// Named loadramp rather than the obvious "stress" because `stress` is a well-known
// Debian package, and a binary of that name in /usr/local/bin shadows it — someone
// typing `stress --cpu 4` on a box where this was installed would get an HTTP load
// generator and a baffling error. Colliding with a common system tool's name is a
// trap set for whoever comes next.
//
// This asks a different question from cmd/loadtest, which runs a fixed number of
// workers flat out and reports what that produced. That is a *closed loop*: if the
// server slows down, the client sends more slowly too, so offered load is capped by
// the server's own speed and the test can never overload it. It answers "how fast is
// it under this much concurrency", not "where does it fall over".
//
// This is an *open loop*. Requests are scheduled at a target rate and sent whether or
// not earlier ones have come back, the rate steps up every stage, and the run stops
// when the service stops keeping up. That is the shape of real traffic: users arrive
// when they arrive, not when the server is ready for them.
//
// # Coordinated omission
//
// The trap in open-loop testing is measuring latency from when a request was *sent*
// rather than when it was *due*. Under overload the sender falls behind, so the
// requests that would have been slowest are simply never issued, and the ones that
// are get timed from late start points. Latency then looks fine right up until the
// service falls over — the failure hides in the requests the test declined to make.
//
// So every request carries the timestamp at which it should have gone out, and queue
// latency is measured from that. Service time is reported alongside it: service time
// is what the server took, queue latency is what a user would have experienced.
// The gap between them is the backlog.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	var (
		base     = flag.String("url", "http://127.0.0.1:8080", "base URL of the API")
		start    = flag.Int("start", 1000, "starting request rate per second")
		step     = flag.Int("step", 1500, "rate increase per stage")
		maxRate  = flag.Int("max", 40000, "stop after this rate")
		stage    = flag.Duration("stage", time.Minute, "duration of each rate stage")
		workers  = flag.Int("workers", 512, "concurrent senders")
		watch    = flag.Duration("watch", 15*time.Second, "how often the watcher probes")
		abortP99 = flag.Duration("abort-p99", 500*time.Millisecond, "stop when service p99 exceeds this")
		abortErr = flag.Float64("abort-errors", 0.01, "stop when the error rate exceeds this fraction")
	)
	flag.Parse()

	hints, err := sample(*base)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sampling corpus:", err)
		os.Exit(1)
	}
	fmt.Printf("corpus: %d teams, %d donors (widest donor %q, largest team %d)\n",
		len(hints.teamIDs), len(hints.donorNames), hints.bigDonor, hints.bigTeamID)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := newClient(*workers)

	// The watcher is a separate, single-threaded client that asks the same question
	// a person would: how long does one ordinary request take right now? It shares
	// nothing with the load generator — not its connections, not its scheduling — so
	// it cannot be starved by the generator's own backlog, and it reports what the
	// service looks like from outside the storm.
	w := &watcher{client: newClient(2), base: *base, hints: hints, every: *watch}
	go w.run(ctx)

	fmt.Printf("\nramp: %d req/s, +%d every %v, up to %d, %d workers\n",
		*start, *step, *stage, *maxRate, *workers)
	fmt.Printf("stopping early if service p99 > %v or errors > %.1f%%\n\n",
		*abortP99, *abortErr*100)

	header()
	var last *stageResult
	for rate := *start; rate <= *maxRate; rate += *step {
		if ctx.Err() != nil {
			break
		}
		w.beginStage()
		res := runStage(ctx, client, *base, hints, rate, *stage, *workers)
		res.watch = w.endStage()
		res.print()

		if res.errRate() > *abortErr {
			fmt.Printf("\nstopped: error rate %.2f%% exceeded %.2f%%\n",
				res.errRate()*100, *abortErr*100)
			break
		}
		if res.svcP99 > *abortP99 {
			fmt.Printf("\nstopped: service p99 %v exceeded %v\n", ms(res.svcP99), ms(*abortP99))
			break
		}
		if res.dropped > 0 {
			// The generator could not even enqueue at this rate, so the numbers
			// past here describe the test rig rather than the service.
			fmt.Printf("\nstopped: the load generator itself fell behind (%d requests never issued).\n"+
				"          Everything above this line is a valid measurement; this rate is not.\n", res.dropped)
			break
		}
		last = res
	}

	if last != nil {
		fmt.Printf("\nHighest clean rate: %d req/s sustained for %v\n", last.rate, *stage)
		fmt.Printf("  service   avg %s   p50 %s   p99 %s\n", ms(last.svcAvg), ms(last.svcP50), ms(last.svcP99))
		fmt.Printf("  as a user %s avg over %d watcher probes\n", ms(last.watch.avg), last.watch.n)
	}
}

/* --------------------------------------------------------------- client --- */

func newClient(conns int) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			// Connection churn would measure the kernel's accept path rather than
			// the handler, so keep every connection alive and reused.
			MaxIdleConns:        conns * 2,
			MaxIdleConnsPerHost: conns * 2,
			MaxConnsPerHost:     conns * 2,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
	}
}

/* -------------------------------------------------------------- watcher --- */

type watchStats struct {
	n        int
	avg, max time.Duration
}

// watcher probes the service on a fixed interval throughout the run.
type watcher struct {
	client *http.Client
	base   string
	hints  *corpusHints
	every  time.Duration

	mu      sync.Mutex
	samples []time.Duration
}

func (w *watcher) run(ctx context.Context) {
	t := time.NewTicker(w.every)
	defer t.Stop()
	r := rand.New(rand.NewSource(1))
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			path := "/v1/donors/" + urlEscape(w.hints.donorNames[r.Intn(len(w.hints.donorNames))])
			start := time.Now()
			resp, err := w.client.Get(w.base + path)
			if err != nil {
				fmt.Printf("    watcher %s  FAILED: %v\n", time.Now().UTC().Format("15:04:05"), err)
				continue
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			d := time.Since(start)

			w.mu.Lock()
			w.samples = append(w.samples, d)
			w.mu.Unlock()
			fmt.Printf("    watcher %s  %s\n", time.Now().UTC().Format("15:04:05"), ms(d))
		}
	}
}

func (w *watcher) beginStage() {
	w.mu.Lock()
	w.samples = w.samples[:0]
	w.mu.Unlock()
}

func (w *watcher) endStage() watchStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	var s watchStats
	s.n = len(w.samples)
	for _, d := range w.samples {
		s.avg += d
		if d > s.max {
			s.max = d
		}
	}
	if s.n > 0 {
		s.avg /= time.Duration(s.n)
	}
	return s
}

/* ---------------------------------------------------------------- stage --- */

type stageResult struct {
	rate                           int
	sent, ok, errs                 int64
	dropped                        int64
	elapsed                        time.Duration
	svcAvg, svcP50, svcP99, svcMax time.Duration
	queueAvg, queueP99             time.Duration
	watch                          watchStats
}

func (r *stageResult) errRate() float64 {
	if r.sent == 0 {
		return 0
	}
	return float64(r.errs) / float64(r.sent)
}

func header() {
	fmt.Printf("%7s %9s %8s %7s %9s %9s %9s %9s %9s %9s\n",
		"target", "achieved", "ok", "errors", "svc avg", "svc p50", "svc p99", "queue avg", "queue p99", "watcher")
	fmt.Println("  " + str('-', 100))
}

func (r *stageResult) print() {
	achieved := float64(r.sent) / r.elapsed.Seconds()
	fmt.Printf("%7d %9.0f %8d %7d %9s %9s %9s %9s %9s %9s\n",
		r.rate, achieved, r.ok, r.errs,
		ms(r.svcAvg), ms(r.svcP50), ms(r.svcP99),
		ms(r.queueAvg), ms(r.queueP99), ms(r.watch.avg))
}

// runStage offers `rate` requests per second for `dur`, and reports what happened.
func runStage(ctx context.Context, client *http.Client, base string, hints *corpusHints,
	rate int, dur time.Duration, workers int) *stageResult {

	res := &stageResult{rate: rate}
	total := int(float64(rate) * dur.Seconds())

	// Buffer one second of work. An unbounded queue would hide saturation by
	// absorbing it; a bounded one turns "the generator cannot keep up" into a
	// countable event rather than a silently growing backlog.
	jobs := make(chan time.Time, rate)

	svc := make([]time.Duration, 0, total)
	queue := make([]time.Duration, 0, total)
	var mu sync.Mutex

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			local := make([]time.Duration, 0, 1024)
			localQ := make([]time.Duration, 0, 1024)
			for due := range jobs {
				path := pick(r, hints)
				sent := time.Now()
				code, err := do(client, base+path)
				now := time.Now()

				local = append(local, now.Sub(sent))
				// Measured from when it was due, not when it went out: see the
				// package comment on coordinated omission.
				localQ = append(localQ, now.Sub(due))

				if err != nil || code != http.StatusOK {
					atomic.AddInt64(&res.errs, 1)
				} else {
					atomic.AddInt64(&res.ok, 1)
				}
				atomic.AddInt64(&res.sent, 1)
			}
			mu.Lock()
			svc = append(svc, local...)
			queue = append(queue, localQ...)
			mu.Unlock()
		}(int64(i) + 1)
	}

	begin := time.Now()
	interval := time.Second / time.Duration(rate)
	for i := 0; i < total; i++ {
		if ctx.Err() != nil {
			break
		}
		due := begin.Add(time.Duration(i) * interval)
		if d := time.Until(due); d > 0 {
			time.Sleep(d)
		}
		select {
		case jobs <- due:
		default:
			atomic.AddInt64(&res.dropped, 1)
		}
	}
	close(jobs)
	wg.Wait()
	res.elapsed = time.Since(begin)

	sort.Slice(svc, func(i, j int) bool { return svc[i] < svc[j] })
	sort.Slice(queue, func(i, j int) bool { return queue[i] < queue[j] })
	res.svcAvg, res.svcP50, res.svcP99, res.svcMax = avg(svc), pct(svc, 50), pct(svc, 99), pct(svc, 100)
	res.queueAvg, res.queueP99 = avg(queue), pct(queue, 99)
	return res
}

func do(client *http.Client, url string) (int, error) {
	resp, err := client.Get(url)
	if err != nil {
		return 0, err
	}
	// Draining matters: an unread body prevents the connection being reused, and the
	// test would end up measuring connection setup instead of the handler.
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

/* -------------------------------------------------------------- targets --- */

// pick chooses an endpoint. The mix is weighted toward the per-entity lookups the
// brief asks about — a random donor or a random team — with a little of everything
// else so the figures describe plausible traffic rather than one hot path.
func pick(r *rand.Rand, h *corpusHints) string {
	switch n := r.Intn(100); {
	case n < 35:
		return "/v1/donors/" + urlEscape(h.donorNames[r.Intn(len(h.donorNames))])
	case n < 65:
		return fmt.Sprintf("/v1/teams/%d", h.teamIDs[r.Intn(len(h.teamIDs))])
	case n < 75:
		return fmt.Sprintf("/v1/teams/%d/members?per_page=100", h.teamIDs[r.Intn(len(h.teamIDs))])
	case n < 85:
		return "/v1/donors/" + urlEscape(h.donorNames[r.Intn(len(h.donorNames))]) + "/history?granularity=hourly"
	case n < 92:
		return fmt.Sprintf("/v1/teams/%d/history?granularity=hourly", h.teamIDs[r.Intn(len(h.teamIDs))])
	case n < 97:
		return "/v1/summary"
	default:
		// The pathological cases, kept rare because they are rare in life: the
		// widest donor spans thousands of teams, the biggest team a third of the
		// corpus.
		if r.Intn(2) == 0 {
			return "/v1/donors/" + urlEscape(h.bigDonor)
		}
		return fmt.Sprintf("/v1/teams/%d/members?per_page=100", h.bigTeamID)
	}
}

/* ---------------------------------------------------------------- utils --- */

func avg(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	var t time.Duration
	for _, x := range d {
		t += x
	}
	return t / time.Duration(len(d))
}

func pct(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := len(sorted) * p / 100
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.2fms", float64(d.Microseconds())/1000) }

func str(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

func urlEscape(s string) string { return url.PathEscape(s) }

type corpusHints struct {
	teamIDs    []int64
	donorNames []string
	bigTeamID  int64
	bigDonor   string
}

func sample(base string) (*corpusHints, error) {
	h := &corpusHints{}
	client := &http.Client{Timeout: 60 * time.Second}

	get := func(path string, into any) error {
		resp, err := client.Get(base + path)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s returned %s", path, resp.Status)
		}
		return json.NewDecoder(resp.Body).Decode(into)
	}

	var teams struct {
		Data []struct {
			TeamID       int64 `json:"team_id"`
			MembersTotal int64 `json:"members_total"`
		} `json:"data"`
	}
	if err := get("/v1/teams?per_page=500", &teams); err != nil {
		return nil, err
	}
	// found, not "is it zero": team 0 is the "no team specified" bucket and really is
	// the largest, so a zero-value check would discard the correct answer.
	var biggest int64
	foundTeam := false
	for _, t := range teams.Data {
		h.teamIDs = append(h.teamIDs, t.TeamID)
		if !foundTeam || t.MembersTotal > biggest {
			biggest, h.bigTeamID, foundTeam = t.MembersTotal, t.TeamID, true
		}
	}

	var donors struct {
		Data []struct {
			Name      string `json:"name"`
			TeamCount int64  `json:"team_count"`
		} `json:"data"`
	}
	if err := get("/v1/donors?per_page=500", &donors); err != nil {
		return nil, err
	}
	var widest int64
	for _, d := range donors.Data {
		h.donorNames = append(h.donorNames, d.Name)
		if d.TeamCount > widest {
			widest, h.bigDonor = d.TeamCount, d.Name
		}
	}

	if len(h.teamIDs) == 0 || len(h.donorNames) == 0 {
		return nil, fmt.Errorf("service returned no teams or donors; has it ingested yet?")
	}
	if !foundTeam {
		h.bigTeamID = h.teamIDs[0]
	}
	if h.bigDonor == "" {
		h.bigDonor = h.donorNames[0]
	}
	return h, nil
}
