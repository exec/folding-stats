// Package vast talks to the Vast.ai marketplace.
//
// It runs in the agent rather than the browser, and not by preference. Vast sends no
// Access-Control-Allow-Origin on any response, so a page cannot call it at all —
// fetch fails before the request leaves. Putting it here turned out better than the
// design it replaced: the API key stays on a machine its owner already runs, our
// server never sees it, and the bidding carries on when the browser is closed. That
// last part is the whole point, because an interruptible instance is outbid at three
// in the morning and a tab that is not open cannot bid.
//
// The field semantics below are not from the documentation, which is silent or wrong
// on most of them. They come from a week of running this live, recorded in
// ~/Developer/vast-fah/HANDOFF.md, and each one cost somebody an afternoon.
package vast

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
)

const base = "https://console.vast.ai/api/v0"

// ErrBadKey is returned for a key Vast will not accept, so a caller can tell "your
// key is wrong" from "Vast is having a bad day".
var ErrBadKey = errors.New("Vast refused the API key")

type Client struct {
	Key  string
	HTTP *http.Client
}

func New(key string) *Client {
	return &Client{Key: key, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Instance is one rented machine, with only the fields that mean something here.
type Instance struct {
	ID       int64  `json:"id"`
	Label    string `json:"label"`
	Template string `json:"template_name"`
	GPU      string `json:"gpu_name"`
	NumGPUs  int    `json:"num_gpus"`
	Machine  int64  `json:"machine_id"`

	// IsBid marks an interruptible contract. Everything about eviction below only
	// applies to these; an on-demand instance is never outbid.
	IsBid bool `json:"is_bid"`

	// CurState is the field to trust. There is no "outbid" status, and
	// intended_status says "stopped" for an eviction and for somebody pressing stop,
	// so it cannot tell them apart. A watcher keyed on intended_status once logged
	// zero evictions forever while boxes sat dead.
	CurState string `json:"cur_state"`
	// ActualStatus is deliberately not used for liveness: a booting instance is
	// cur_state=running with actual_status=loading, and treating that as "not running"
	// makes a bidder bid against itself on every launch.
	ActualStatus string `json:"actual_status"`
	StatusMsg    string `json:"status_msg"`

	DPHBase  float64 `json:"dph_base"`  // our bid, on an interruptible contract
	DPHTotal float64 `json:"dph_total"` // with storage and bandwidth
	MinBid   float64 `json:"min_bid"`

	// StorageTotal is charged whether or not the instance runs, which is why an
	// interruptible box that cannot win its auction costs money to do nothing.
	StorageTotal float64 `json:"storage_total_cost"`

	UptimeMins float64 `json:"uptime_mins"`
	StartDate  float64 `json:"start_date"`
	GPUUtil    float64 `json:"gpu_util"`
	PublicIP   string  `json:"public_ipaddr"`
}

// Stopped reports whether the instance is not running.
//
// cur_state, for the reasons above.
func (i Instance) Stopped() bool { return i.CurState == "stopped" }

// Outbid reports an interruptible instance that has lost its auction.
//
// The bid being under the floor is the only signal there is. Note that min_bid is
// consulted here to *explain* a stop, never to predict that a restart will succeed —
// see NextBid.
func (i Instance) Outbid() bool {
	return i.IsBid && i.Stopped() && i.MinBid > i.DPHBase
}

// Booting distinguishes a machine on its way up from one that is working.
func (i Instance) Booting() bool {
	return i.CurState == "running" && i.ActualStatus != "running"
}

// NextBid is what to raise to.
//
// min_bid is not the price that decides who runs, and it flaps: one instance reported
// $0.3649 and $0.1600 two minutes apart, and sat stopped through nine restarts at a
// price min_bid insisted cleared twice over. Raising to $0.3564 fixed it instantly.
// So this treats min_bid as a hint about magnitude and always adds a margin, and a
// caller that has already failed should escalate off its own last bid rather than off
// this number again.
func NextBid(floor, margin, increment float64) float64 {
	if margin <= 0 {
		margin = 0.10
	}
	if increment <= 0 {
		increment = 0.005
	}
	v := math.Max(floor*(1+margin), floor+increment)
	return math.Round(v*10000) / 10000
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("reaching Vast: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	// Vast answers a bad key with 404 and an auth_error body rather than 401, so the
	// status alone would have this reported as "Vast returned 404: {…}" — which reads
	// like the endpoint moved, not like the key is wrong.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
		bytes.Contains(raw, []byte(`"auth_error"`)) {
		return ErrBadKey
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("Vast is rate limiting this key")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("Vast returned %d: %s", resp.StatusCode, trim(string(raw), 200))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding the Vast response: %w", err)
	}
	return nil
}

// Instances lists what this key rents.
func (c *Client) Instances(ctx context.Context) ([]Instance, error) {
	var out struct {
		Instances []Instance `json:"instances"`
	}
	if err := c.do(ctx, http.MethodGet, "/instances/?owner=me", nil, &out); err != nil {
		return nil, err
	}
	return out.Instances, nil
}

// SetBid changes the price on an interruptible contract.
func (c *Client) SetBid(ctx context.Context, id int64, price float64) error {
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/instances/bid_price/%d/", id),
		map[string]any{"client_id": "me", "price": price}, nil)
}

// Start asks a stopped instance to run again.
//
// Whether it does is a separate question: on-demand renters outrank bids, so a
// machine will accept the contract and then never start it. Callers should confirm
// rather than assume, and give up after a couple of tries.
func (c *Client) Start(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/instances/%d/", id),
		map[string]any{"state": "running"}, nil)
}

func (c *Client) Stop(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/instances/%d/", id),
		map[string]any{"state": "stopped"}, nil)
}

func (c *Client) Destroy(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/instances/%d/", id), nil, nil)
}

// Whoami confirms a key works, and returns the credit left on it.
func (c *Client) Whoami(ctx context.Context) (int64, float64, error) {
	var out struct {
		ID     int64   `json:"id"`
		Credit float64 `json:"credit"`
	}
	if err := c.do(ctx, http.MethodGet, "/users/current/", nil, &out); err != nil {
		return 0, 0, err
	}
	return out.ID, out.Credit, nil
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
