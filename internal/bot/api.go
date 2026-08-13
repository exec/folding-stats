// Package bot is a Discord client for the Folding@home statistics API.
//
// It is also the reference implementation of a well-behaved consumer. The API asks
// callers to cache against the snapshot rather than poll, and this is what that looks
// like in practice — which matters more than usual, because a Discord bot is exactly
// the traffic shape the API was built to invite.
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client talks to the statistics API.
//
// It is pointed at the service's private address, not its public hostname: the bot
// runs a bridge away from the origin, so going out through DNS, Cloudflare and back
// in through the uplink would spend the household's bandwidth to reach a machine two
// hops away. Same data, none of the congestion, and no TLS handshake per call.
type Client struct {
	Base string
	HTTP *http.Client

	mu    sync.RWMutex
	at    string // the snapshot every cached entry belongs to
	cache map[string][]byte
}

func NewClient(base string) *Client {
	return &Client{
		Base: strings.TrimRight(base, "/"),
		HTTP: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
				DialContext: (&net.Dialer{
					Timeout:   3 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
			},
		},
		cache: map[string][]byte{},
	}
}

// Envelope is the shape every response shares.
type Envelope struct {
	Snapshot Snapshot        `json:"snapshot"`
	Data     json.RawMessage `json:"data"`
	Page     *Page           `json:"page,omitempty"`
}

type Snapshot struct {
	At             time.Time `json:"at"`
	NextExpectedAt time.Time `json:"next_expected_at"`
	Stale          bool      `json:"stale"`
	ServerTime     time.Time `json:"server_time"`
	WarmingUp      *struct {
		HistorySpanSec    int64 `json:"history_span_sec,omitempty"`
		IntervalEstimated bool  `json:"interval_estimated,omitempty"`
	} `json:"warming_up,omitempty"`
}

type Page struct {
	Page       int `json:"page"`
	PerPage    int `json:"per_page"`
	TotalItems int `json:"total_items"`
	TotalPages int `json:"total_pages"`
}

// APIError is a refusal the service explained.
type APIError struct {
	Status  int
	Kind    string `json:"error"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("the statistics service returned %d", e.Status)
}

// NotFound reports whether the service said the entity does not exist, as opposed to
// failing. The two deserve different replies: one is the user's typo, the other is ours.
func NotFound(err error) bool {
	var a *APIError
	if ok := asAPIError(err, &a); !ok {
		return false
	}
	return a.Status == http.StatusNotFound
}

func asAPIError(err error, out **APIError) bool {
	for err != nil {
		if a, ok := err.(*APIError); ok {
			*out = a
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Get fetches a path and decodes data into out, returning the snapshot it belongs to.
//
// Responses are cached until the snapshot changes rather than for a fixed period. The
// data is immutable between publishes — that is the service's own guarantee — so a
// duration would be either wrong or a guess, while the snapshot time is exact. A busy
// channel firing the same command repeatedly costs one request an hour.
func (c *Client) Get(ctx context.Context, path string, out any) (Snapshot, error) {
	env, err := c.envelope(ctx, path)
	if err != nil {
		return env.Snapshot, err
	}
	if out != nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return env.Snapshot, fmt.Errorf("decoding the data: %w", err)
		}
	}
	return env.Snapshot, nil
}

// GetEnvelope is Get for callers that also want the snapshot or the page block.
func (c *Client) GetEnvelope(ctx context.Context, path string) (Envelope, error) {
	return c.envelope(ctx, path)
}

// envelope returns the whole response for a path, from cache when it is still good.
//
// One fetch path for both callers. They used to differ, and the difference was a bug:
// the cache handed back the inner data object, GetEnvelope unmarshalled that into an
// Envelope, and every field it wanted — the snapshot above all — came back zero. The
// alert watcher compares snapshot times to decide whether a publish happened, so with
// a zero time it compared zero against zero on every tick and no alert could ever fire.
func (c *Client) envelope(ctx context.Context, path string) (Envelope, error) {
	var env Envelope
	if body, ok := c.cached(path); ok {
		if err := json.Unmarshal(body, &env); err != nil {
			return env, fmt.Errorf("decoding the cached response: %w", err)
		}
		return env, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+path, nil)
	if err != nil {
		return env, err
	}
	req.Header.Set("Accept", "application/json")
	// Accept-Encoding is deliberately not set. net/http negotiates gzip and
	// decompresses transparently, but only while it owns the header — set it by hand
	// and the body arrives still compressed, which decodes as "invalid character
	// '\x1f'" and looks like the service is broken rather than the client.
	req.Header.Set("User-Agent", "folding-discord/0.1 (+https://github.com/exec/folding-stats)")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return env, fmt.Errorf("reaching the statistics service: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return env, err
	}

	if resp.StatusCode != http.StatusOK {
		e := &APIError{Status: resp.StatusCode}
		_ = json.Unmarshal(body, e)
		return Envelope{}, e
	}

	if err := json.Unmarshal(body, &env); err != nil {
		return Envelope{}, fmt.Errorf("decoding the response: %w", err)
	}
	c.store(path, env.Snapshot, body)
	return env, nil
}

// statusPath is the freshness probe, and it is the one route never answered from cache.
//
// Its entire job is to report that everything else is out of date, so serving it from
// the cache is a loop with no way out: the watcher polls it, is handed the copy stored
// under the snapshot it is trying to move on from, concludes nothing has published, and
// therefore never makes the request that would have refreshed anything. The bot then
// serves one snapshot until the process restarts — observed on 11 August 2026 answering
// with figures nine and a half hours old.
//
// The origin already says so: it marks this route no-store precisely so that pollers
// see publishes. Caching it was ignoring the one instruction it sends.
const statusPath = "/v1/status"

func cacheable(path string) bool {
	return path != statusPath && !strings.HasPrefix(path, "/v1/search")
}

// cached returns the stored response body — the whole envelope, not the data inside it.
func (c *Client) cached(path string) ([]byte, bool) {
	if !cacheable(path) {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	body, ok := c.cache[path]
	return body, ok
}

// store keeps the entry, dropping everything older first.
//
// A new snapshot invalidates the whole cache at once, which is both correct and the
// cheapest possible policy: there is no per-entry expiry to track, because every entry
// has exactly the same lifetime.
func (c *Client) store(path string, snap Snapshot, body []byte) {
	if !cacheable(path) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if at := snap.At.UTC().Format(time.RFC3339); at != c.at {
		c.at = at
		c.cache = make(map[string][]byte, len(c.cache))
	}
	c.cache[path] = body
}

/* ----------------------------------------------------------------- shapes --- */

type Standing struct {
	Lifetime *struct {
		TopPercent float64 `json:"top_percent"`
		Of         int64   `json:"of"`
	} `json:"lifetime,omitempty"`
}

type Streak struct {
	Current    int    `json:"current"`
	Longest    int    `json:"longest"`
	ActiveDays int    `json:"active_days"`
	Since      string `json:"since,omitempty"`
	// AtCollectionFloor marks a run reaching back to the first day this service
	// recorded anything, which makes the figure a lower bound rather than a fact about
	// the folder: someone who has folded daily for a decade reports the age of the
	// site. Printing it unqualified is not a rounding error, it is a wrong number.
	AtCollectionFloor bool `json:"at_collection_floor,omitempty"`
}

type Team struct {
	TeamID         int64     `json:"team_id"`
	Name           string    `json:"name"`
	Rank           int64     `json:"rank"`
	RankChange24h  int64     `json:"rank_change_24h"`
	MembersTotal   int64     `json:"members_total"`
	MembersActive  int64     `json:"members_active"`
	PointsTotal    int64     `json:"points_total"`
	WUsTotal       int64     `json:"wus_total"`
	PointsLast24h  int64     `json:"points_last_24h"`
	PointsPerDay   int64     `json:"points_per_day_24h_avg"`
	PointsPerDay7d int64     `json:"points_per_day_7d_avg"`
	PointsToday    int64     `json:"points_today_utc"`
	PointsPerWU    int64     `json:"points_per_wu"`
	Standing       *Standing `json:"standing,omitempty"`
	Streak         *Streak   `json:"streak,omitempty"`
}

type Donor struct {
	Name           string `json:"name"`
	Rank           int64  `json:"rank"`
	RankChange24h  int64  `json:"rank_change_24h"`
	TeamCount      int64  `json:"team_count"`
	PointsTotal    int64  `json:"points_total"`
	WUsTotal       int64  `json:"wus_total"`
	PointsLast24h  int64  `json:"points_last_24h"`
	PointsPerDay   int64  `json:"points_per_day_24h_avg"`
	PointsPerDay7d int64  `json:"points_per_day_7d_avg"`
	PointsToday    int64  `json:"points_today_utc"`
	PointsPerWU    int64  `json:"points_per_wu"`
	// LikelyNotAPerson marks names shared by implausibly many teams, which the
	// bot has to surface: replying with Anonymous's aggregate as though it were one
	// folder would be the most misleading thing it could do.
	LikelyNotAPerson bool         `json:"likely_not_a_person,omitempty"`
	Standing         *Standing    `json:"standing,omitempty"`
	Streak           *Streak      `json:"streak,omitempty"`
	Teams            []Membership `json:"teams,omitempty"`
}

// Membership is one donor's record on one team.
//
// The service stores at the grain (name, team), so every row here is a membership and
// not a team. That makes the field names a trap worth naming: "name" is the *donor's*
// name, identical on all of them, and the team is under "team_name". Binding the
// former and calling it the team renders as the donor's own name listed once per
// team, which is exactly what it looked like.
type Membership struct {
	TeamID         int64  `json:"team_id"`
	TeamName       string `json:"team_name"`
	Donor          string `json:"name"`
	RankInTeam     int64  `json:"rank_in_team"`
	PointsTotal    int64  `json:"points_total"`
	PointsPerDay   int64  `json:"points_per_day_24h_avg"`
	PointsPerDay7d int64  `json:"points_per_day_7d_avg"`
}

type SearchResult struct {
	Teams []struct {
		TeamID      int64  `json:"team_id"`
		Name        string `json:"name"`
		PointsTotal int64  `json:"points_total"`
	} `json:"teams"`
	Donors []struct {
		Name        string `json:"name"`
		PointsTotal int64  `json:"points_total"`
	} `json:"donors"`
	ExactTeam  bool `json:"exact_team,omitempty"`
	ExactDonor bool `json:"exact_donor,omitempty"`
}

type Summary struct {
	PointsTotal   int64 `json:"points_total"`
	WUsTotal      int64 `json:"wus_total"`
	PointsLast24h int64 `json:"points_last_24h"`
	DonorsTotal   int64 `json:"donors_total"`
	DonorsActive  int64 `json:"donors_active"`
	TeamsTotal    int64 `json:"teams_total"`
	TeamsActive   int64 `json:"teams_active"`
}

/* ---------------------------------------------------------------- helpers --- */

func esc(s string) string { return url.PathEscape(s) }

// Search is the entry point for every name a user types, and the source of
// autocomplete. Donor names are not unique and not guessable, so a bot that demanded
// exact spelling would be unusable.
func (c *Client) Search(ctx context.Context, q string, limit int) (SearchResult, Snapshot, error) {
	var r SearchResult
	if strings.TrimSpace(q) == "" {
		return r, Snapshot{}, nil
	}
	snap, err := c.Get(ctx, fmt.Sprintf("/v1/search?q=%s&limit=%d", url.QueryEscape(q), limit), &r)
	return r, snap, err
}

func (c *Client) Donor(ctx context.Context, name string) (Donor, Snapshot, error) {
	var d Donor
	snap, err := c.Get(ctx, "/v1/donors/"+esc(name), &d)
	return d, snap, err
}

func (c *Client) Team(ctx context.Context, id int64) (Team, Snapshot, error) {
	var t Team
	snap, err := c.Get(ctx, fmt.Sprintf("/v1/teams/%d", id), &t)
	return t, snap, err
}

func (c *Client) Summary(ctx context.Context) (Summary, Snapshot, error) {
	var s Summary
	snap, err := c.Get(ctx, "/v1/summary", &s)
	return s, snap, err
}

// Rivals is the ranking either side of one entity, with projected overtakes.
type Rivals struct {
	Rank        int64   `json:"rank"`
	Name        string  `json:"name"`
	HorizonDays float64 `json:"horizon_days"`
	Rivals      []Rival `json:"rivals"`
}

type Rival struct {
	Rank           int64  `json:"rank"`
	Name           string `json:"name"`
	TeamID         int64  `json:"team_id,omitempty"`
	PointsTotal    int64  `json:"points_total"`
	PointsPerDay   int64  `json:"points_per_day_24h_avg"`
	PointsPerDay7d int64  `json:"points_per_day_7d_avg"`
	// PointsGap is unsigned: the distance between the two, whichever is ahead.
	PointsGap int64 `json:"points_gap"`
	// OvertakeDays is absent when no crossing is projected inside the horizon —
	// which is most of them, because most of the field is not producing at all.
	OvertakeDays *float64 `json:"overtake_days"`
}

func (c *Client) Rivals(ctx context.Context, kind, id string) (Rivals, Snapshot, error) {
	var r Rivals
	path := "/v1/donors/" + esc(id) + "/rivals"
	if kind == "teams" {
		path = "/v1/teams/" + esc(id) + "/rivals"
	}
	snap, err := c.Get(ctx, path, &r)
	return r, snap, err
}

func (c *Client) TopTeams(ctx context.Context, sort string, limit int) ([]Team, Snapshot, error) {
	var t []Team
	snap, err := c.Get(ctx, fmt.Sprintf("/v1/teams?per_page=%d&sort=%s", limit, url.QueryEscape(sort)), &t)
	return t, snap, err
}

func (c *Client) TopDonors(ctx context.Context, sort string, limit int) ([]Donor, Snapshot, error) {
	var d []Donor
	snap, err := c.Get(ctx, fmt.Sprintf("/v1/donors?per_page=%d&sort=%s", limit, url.QueryEscape(sort)), &d)
	return d, snap, err
}
