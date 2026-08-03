// Command loadtest measures API latency under concurrent load.
//
// It exists to answer "is this fast enough" before any frontend depends on it, and to
// make regressions visible. Because everything is served from a precomputed in-memory
// snapshot, the interesting question is not throughput in aggregate but the tail: a
// handler that scans the corpus rather than indexing into it looks fine at the median
// and terrible at p99.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// target is one endpoint under test. Weight reflects how often a real client would
// call it, so the aggregate figures describe plausible traffic rather than a uniform
// sweep of every route.
type target struct {
	name   string
	path   func(*rand.Rand, *corpusHints) string
	weight int
}

// corpusHints holds real ids sampled from the running service, so requests hit
// entities that actually exist rather than 404ing.
type corpusHints struct {
	teamIDs    []int64
	donorNames []string
	bigTeamID  int64
	bigDonor   string
}

func main() {
	var (
		base   = flag.String("url", "http://127.0.0.1:8080", "base URL of the API")
		dur    = flag.Duration("duration", 20*time.Second, "test duration")
		conc   = flag.Int("concurrency", 16, "concurrent workers")
		only   = flag.String("only", "", "run only endpoints whose name contains this")
		warmup = flag.Duration("warmup", 2*time.Second, "warmup period, excluded from results")
	)
	flag.Parse()

	hints, err := sample(*base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sampling the service failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("sampled %d teams, %d donors (largest team %d, widest donor %q)\n\n",
		len(hints.teamIDs), len(hints.donorNames), hints.bigTeamID, hints.bigDonor)

	targets := []target{
		{"summary", func(*rand.Rand, *corpusHints) string { return "/v1/summary" }, 10},
		{"teams:page1", func(*rand.Rand, *corpusHints) string { return "/v1/teams?per_page=100" }, 15},
		{"teams:deep", func(r *rand.Rand, _ *corpusHints) string {
			return fmt.Sprintf("/v1/teams?page=%d&per_page=100", 1+r.Intn(1000))
		}, 5},
		{"donors:page1", func(*rand.Rand, *corpusHints) string { return "/v1/donors?per_page=100" }, 15},
		{"donors:deep", func(r *rand.Rand, _ *corpusHints) string {
			return fmt.Sprintf("/v1/donors?page=%d&per_page=100", 1+r.Intn(10000))
		}, 5},
		{"team:detail", func(r *rand.Rand, h *corpusHints) string {
			return fmt.Sprintf("/v1/teams/%d", h.teamIDs[r.Intn(len(h.teamIDs))])
		}, 15},
		{"team:members", func(r *rand.Rand, h *corpusHints) string {
			return fmt.Sprintf("/v1/teams/%d/members?per_page=100", h.teamIDs[r.Intn(len(h.teamIDs))])
		}, 10},
		// The largest team holds a third of the corpus; its roster is the worst case
		// for anything proportional to team size.
		{"team:members:biggest", func(r *rand.Rand, h *corpusHints) string {
			return fmt.Sprintf("/v1/teams/%d/members?per_page=100&page=%d", h.bigTeamID, 1+r.Intn(100))
		}, 5},
		{"donor:detail", func(r *rand.Rand, h *corpusHints) string {
			return "/v1/donors/" + urlEscape(h.donorNames[r.Intn(len(h.donorNames))])
		}, 15},
		// A name spanning thousands of teams is the worst case for the donor view,
		// which aggregates every one of its members.
		{"donor:widest", func(r *rand.Rand, h *corpusHints) string {
			return "/v1/donors/" + urlEscape(h.bigDonor)
		}, 3},
		{"search", func(r *rand.Rand, h *corpusHints) string {
			return "/v1/search?q=" + urlEscape(h.donorNames[r.Intn(len(h.donorNames))])
		}, 5},
		{"history:team", func(r *rand.Rand, h *corpusHints) string {
			return fmt.Sprintf("/v1/teams/%d/history?granularity=cycle",
				h.teamIDs[r.Intn(len(h.teamIDs))])
		}, 8},
		{"history:donor", func(r *rand.Rand, h *corpusHints) string {
			return "/v1/donors/" + urlEscape(h.donorNames[r.Intn(len(h.donorNames))]) +
				"/history?granularity=cycle"
		}, 8},
	}
	if *only != "" {
		var kept []target
		for _, t := range targets {
			if strings.Contains(t.name, *only) {
				kept = append(kept, t)
			}
		}
		targets = kept
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "no endpoints selected")
		os.Exit(1)
	}

	res := run(*base, targets, hints, *conc, *dur, *warmup)
	report(res, *dur)
}

type result struct {
	mu       sync.Mutex
	byName   map[string]*stat
	errors   atomic.Int64
	statuses sync.Map // int -> *atomic.Int64
	bytes    atomic.Int64
}

type stat struct {
	lat   []time.Duration
	bytes int64
}

func run(base string, targets []target, hints *corpusHints, conc int, dur, warmup time.Duration) *result {
	// A weighted ticket pool turns endpoint weights into a sampling distribution
	// without per-request arithmetic.
	var pool []int
	for i, t := range targets {
		for w := 0; w < t.weight; w++ {
			pool = append(pool, i)
		}
	}

	res := &result{byName: map[string]*stat{}}
	for _, t := range targets {
		res.byName[t.name] = &stat{}
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        conc * 2,
			MaxIdleConnsPerHost: conc * 2,
			MaxConnsPerHost:     conc * 2,
		},
	}

	start := time.Now()
	deadline := start.Add(warmup + dur)
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			local := map[string][]time.Duration{}
			localBytes := map[string]int64{}

			for time.Now().Before(deadline) {
				t := targets[pool[rng.Intn(len(pool))]]
				url := base + t.path(rng, hints)

				t0 := time.Now()
				resp, err := client.Get(url)
				if err != nil {
					res.errors.Add(1)
					continue
				}
				n, _ := io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				elapsed := time.Since(t0)

				counter, _ := res.statuses.LoadOrStore(resp.StatusCode, &atomic.Int64{})
				counter.(*atomic.Int64).Add(1)
				res.bytes.Add(n)

				// Warmup requests are excluded: the first touch of a page pulls it
				// into cache and is not representative of steady state.
				if time.Since(start) > warmup {
					local[t.name] = append(local[t.name], elapsed)
					localBytes[t.name] += n
				}
			}

			res.mu.Lock()
			for name, ds := range local {
				s := res.byName[name]
				s.lat = append(s.lat, ds...)
				s.bytes += localBytes[name]
			}
			res.mu.Unlock()
		}(int64(w) + 1)
	}
	wg.Wait()
	return res
}

func report(res *result, dur time.Duration) {
	names := make([]string, 0, len(res.byName))
	for n := range res.byName {
		names = append(names, n)
	}
	sort.Strings(names)

	var all []time.Duration
	fmt.Printf("%-24s %8s %9s %9s %9s %9s %9s\n",
		"endpoint", "reqs", "p50", "p90", "p99", "max", "avg KB")
	fmt.Println(strings.Repeat("-", 82))
	for _, n := range names {
		s := res.byName[n]
		if len(s.lat) == 0 {
			continue
		}
		sort.Slice(s.lat, func(i, j int) bool { return s.lat[i] < s.lat[j] })
		all = append(all, s.lat...)
		fmt.Printf("%-24s %8d %9s %9s %9s %9s %9.1f\n",
			n, len(s.lat),
			ms(pct(s.lat, 50)), ms(pct(s.lat, 90)), ms(pct(s.lat, 99)),
			ms(s.lat[len(s.lat)-1]),
			float64(s.bytes)/float64(len(s.lat))/1024)
	}

	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	fmt.Println(strings.Repeat("-", 82))
	if len(all) > 0 {
		fmt.Printf("%-24s %8d %9s %9s %9s %9s\n", "ALL", len(all),
			ms(pct(all, 50)), ms(pct(all, 90)), ms(pct(all, 99)), ms(all[len(all)-1]))
		fmt.Printf("\nthroughput: %.0f req/s   %.1f MB/s\n",
			float64(len(all))/dur.Seconds(),
			float64(res.bytes.Load())/dur.Seconds()/(1<<20))
	}
	res.statuses.Range(func(k, v any) bool {
		if k.(int) != http.StatusOK {
			fmt.Printf("status %d: %d responses\n", k.(int), v.(*atomic.Int64).Load())
		}
		return true
	})
	if n := res.errors.Load(); n > 0 {
		fmt.Printf("transport errors: %d\n", n)
	}
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

func urlEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// sample pulls real ids from the running service so requests hit live entities.
// It also finds the largest team and the widest donor, which are the worst cases for
// anything proportional to team size or team count.
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
	var biggest int64
	for _, t := range teams.Data {
		h.teamIDs = append(h.teamIDs, t.TeamID)
		if t.MembersTotal > biggest {
			biggest = t.MembersTotal
			h.bigTeamID = t.TeamID
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
			widest = d.TeamCount
			h.bigDonor = d.Name
		}
	}

	if len(h.teamIDs) == 0 || len(h.donorNames) == 0 {
		return nil, fmt.Errorf("service returned no teams or donors; has it ingested yet?")
	}
	if h.bigTeamID == 0 {
		h.bigTeamID = h.teamIDs[0]
	}
	if h.bigDonor == "" {
		h.bigDonor = h.donorNames[0]
	}
	return h, nil
}
