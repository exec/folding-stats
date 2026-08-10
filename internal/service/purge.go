package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Cache purging tells the CDN to drop what it is holding the moment a new snapshot
// exists, rather than waiting for the TTL to lapse.
//
// It is an optimisation on top of a correct expiry, never the mechanism itself.
// Cache-Control already expires at the next expected publish, so a purge that never
// fires — no token, an API outage, a revoked key — costs freshness measured in the
// tens of seconds and nothing else. Nothing here may fail a publish or delay one.
const (
	// purgeTimeout bounds the call. Missing a purge is a slightly stale edge; a call
	// that hangs would leak goroutines once an hour forever.
	purgeTimeout = 10 * time.Second
	// purgeMinInterval is a floor on how often a purge can fire regardless of what
	// the caller asks for. The publish path has been called on a timer before — a
	// five-minute republish existed until it was removed — and a purge wired to that
	// would have meant 288 calls a day against an API with its own rate limits.
	purgeMinInterval = time.Minute
)

// purgePaths are the URLs purged on publish, and they are exactly the ones the site
// itself requests on its busiest pages.
//
// Purging by URL rather than everything. That began as a hard constraint — the zone
// was a shared one holding a dozen unrelated services, and purge_everything would have
// evicted all of them hourly, forever. On a zone of this site's own it would now be
// merely wasteful rather than destructive, but the URL list is still the better tool:
// it evicts exactly what a publish invalidated and leaves every other object warm,
// where purge_everything would throw away a full cache once an hour to replace nine
// objects. Purge by hostname or prefix is the tool that would beat both, and is
// Enterprise-only.
//
// The query strings must match byte for byte: the CDN's cache key includes them, so
// "?per_page=10" and "?per_page=10&page=1" are different objects. That couples this
// list to what web/views.js asks for. A drift is not silent breakage though — a URL
// that matches nothing simply is not purged, and it still expires on its own TTL
// within the publish margin, so the failure mode is a slightly later update rather
// than a wrong one. Everything not listed here relies on that TTL by design.
var purgePaths = []string{
	"/v1/summary",
	"/v1/teams?per_page=10",  // overview
	"/v1/donors?per_page=10", // overview
	"/v1/summary/history?granularity=hourly&metric=points",
	"/v1/summary/history?granularity=daily&metric=points",
	"/v1/summary/history?granularity=weekly&metric=points",
	"/v1/summary/history?granularity=monthly&metric=points",
	"/v1/teams?page=1&per_page=100&sort=lifetime",  // leaderboard, first page
	"/v1/donors?page=1&per_page=100&sort=lifetime", // leaderboard, first page
}

// purgeMaxFiles is the per-call ceiling below an Enterprise plan.
const purgeMaxFiles = 30

// purger drops the CDN's cached copies when a new snapshot is published.
type purger struct {
	endpoint string
	token    string
	files    []string
	client   *http.Client
	log      *slog.Logger

	mu      sync.Mutex
	lastAt  time.Time // snapshot instant most recently purged for
	lastRun time.Time
}

// newPurger returns nil when purging is not configured, which is the normal case for
// a local run and for anyone else running this. A nil *purger is a working no-op, so
// callers never branch on it.
func newPurger(zoneID, token, site string, log *slog.Logger) *purger {
	if zoneID == "" || token == "" || site == "" {
		return nil
	}
	site = strings.TrimSuffix(site, "/")
	files := make([]string, 0, len(purgePaths))
	for _, p := range purgePaths {
		if len(files) == purgeMaxFiles {
			log.Warn("purge list truncated to the per-call limit", "limit", purgeMaxFiles)
			break
		}
		files = append(files, site+p)
	}
	return &purger{
		endpoint: fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/purge_cache", zoneID),
		token:    token,
		files:    files,
		client:   &http.Client{Timeout: purgeTimeout},
		log:      log,
	}
}

// Purge asks the CDN to drop its cache for the snapshot taken at `at`.
//
// Returns immediately: the request runs on its own goroutine with its own deadline,
// so an unreachable API cannot hold the publish lock or slow a cycle.
//
// Repeat calls for a snapshot already purged do nothing. publish() runs on restart
// and on republish as well as on new data, and every one of those would otherwise
// spend a purge on a cache that is already correct.
func (p *purger) Purge(at time.Time) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if !at.After(p.lastAt) || time.Since(p.lastRun) < purgeMinInterval {
		p.mu.Unlock()
		return
	}
	p.lastAt, p.lastRun = at, time.Now()
	p.mu.Unlock()

	go p.send(at)
}

func (p *purger) send(at time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), purgeTimeout)
	defer cancel()

	body, _ := json.Marshal(map[string][]string{"files": p.files})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		p.log.Warn("cache purge: building request", "err", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		p.log.Warn("cache purge failed; the edge will expire on its own TTL instead",
			"at", at.Format(time.RFC3339), "err", err)
		return
	}
	defer resp.Body.Close()

	// Cloudflare answers 200 with {"success": false} for an authorisation or quota
	// problem, so the status code alone does not say whether anything was purged.
	var out struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || !out.Success {
		msg := "no error detail"
		if len(out.Errors) > 0 {
			msg = fmt.Sprintf("%d %s", out.Errors[0].Code, out.Errors[0].Message)
		}
		p.log.Warn("cache purge rejected; the edge will expire on its own TTL instead",
			"status", resp.StatusCode, "detail", msg)
		return
	}
	p.log.Info("cache purged", "at", at.Format(time.RFC3339), "urls", len(p.files))
}
