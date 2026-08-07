package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	p "folding/internal/relayproto"
	"folding/internal/vast"
)

// Rented compute, managed from the machine rather than from the page.
//
// Vast sends no CORS headers, so a browser cannot call it at all — the request fails
// before it leaves. That forced this here, and it is the better place anyway: the API
// key sits on a machine its owner already runs, our server never sees it, and the
// watching carries on when the browser is closed. An interruptible instance is outbid
// at three in the morning, and a tab that is not open cannot notice.
//
// Only one agent should hold a key — the one on a machine that stays on. An agent on a
// rented box would be evicted along with the thing it was supposed to be watching.

// rentalPoll is how often Vast is asked what it has.
//
// Vast rate limits at 500 requests per five minutes per key, so this is nowhere near
// it even with several agents. It is slow enough to be polite and fast enough that an
// eviction is visible within a minute, which is well inside the time it takes anybody
// to do anything about it.
const rentalPoll = 45 * time.Second

// Rentals is the agent's view of what this key rents.
type Rentals struct {
	path string

	mu        sync.Mutex
	key       string
	account   int64
	credit    float64
	instances []vast.Instance
	err       string
	checked   time.Time
	// stoppedAt remembers when an instance was first seen stopped, because Vast does
	// not say. Without it the page could report that a box is outbid but never how
	// long it has been — and "outbid" is not alarming until you learn it has been true
	// for three hours.
	stoppedAt map[int64]time.Time
}

func openRentals(path string) *Rentals {
	r := &Rentals{path: path, stoppedAt: map[int64]time.Time{}}
	if b, err := os.ReadFile(path); err == nil {
		r.key = strings.TrimSpace(string(b))
	}
	return r
}

func (r *Rentals) configured() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.key != ""
}

// setKey stores a key, or clears it. Written 0600 through a temporary file, like the
// machine identity beside it.
func (r *Rentals) setKey(key string) error {
	key = strings.TrimSpace(key)
	r.mu.Lock()
	r.key = key
	r.instances, r.err, r.account, r.credit = nil, "", 0, 0
	r.mu.Unlock()

	if key == "" {
		if err := os.Remove(r.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(key), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

func (r *Rentals) client() *vast.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.key == "" {
		return nil
	}
	return vast.New(r.key)
}

// refresh asks Vast what it has, and remembers when things stopped.
func (r *Rentals) refresh(ctx context.Context) {
	c := r.client()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	insts, err := c.Instances(ctx)
	now := time.Now().UTC()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.checked = now
	if err != nil {
		// Kept rather than cleared: a page showing the last known fleet with "could not
		// reach Vast" is more use than one that suddenly shows nothing, which reads as
		// "you have no machines".
		r.err = err.Error()
		return
	}
	r.err = ""
	r.instances = insts

	seen := map[int64]bool{}
	for _, i := range insts {
		seen[i.ID] = true
		if i.Stopped() {
			if _, ok := r.stoppedAt[i.ID]; !ok {
				r.stoppedAt[i.ID] = now
			}
		} else {
			delete(r.stoppedAt, i.ID)
		}
	}
	for id := range r.stoppedAt {
		if !seen[id] {
			delete(r.stoppedAt, id)
		}
	}

	if r.account == 0 {
		if id, credit, err := c.Whoami(ctx); err == nil {
			r.account, r.credit = id, credit
		}
	}
}

// RentalView is what the page is told about one instance.
type RentalView struct {
	ID       int64   `json:"id"`
	Label    string  `json:"label,omitempty"`
	GPU      string  `json:"gpu"`
	NumGPUs  int     `json:"num_gpus"`
	Bid      bool    `json:"interruptible"`
	State    string  `json:"state"` // folding | outbid | stopped | booting
	Status   string  `json:"status,omitempty"`
	DPH      float64 `json:"dph"`
	Storage  float64 `json:"storage_dph"`
	MyBid    float64 `json:"bid,omitempty"`
	MinBid   float64 `json:"min_bid,omitempty"`
	NextBid  float64 `json:"next_bid,omitempty"`
	StoppedS float64 `json:"stopped_seconds,omitempty"`
	Uptime   float64 `json:"uptime_mins,omitempty"`
}

type RentalReport struct {
	Configured bool         `json:"configured"`
	Account    int64        `json:"account,omitempty"`
	Credit     float64      `json:"credit,omitempty"`
	Error      string       `json:"error,omitempty"`
	CheckedAt  time.Time    `json:"checked_at,omitzero"`
	Instances  []RentalView `json:"instances"`
}

func (r *Rentals) report() RentalReport {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := RentalReport{
		Configured: r.key != "",
		Account:    r.account,
		Credit:     r.credit,
		Error:      r.err,
		CheckedAt:  r.checked,
		Instances:  []RentalView{},
	}
	now := time.Now().UTC()
	for _, i := range r.instances {
		v := RentalView{
			ID: i.ID, Label: i.Label, GPU: i.GPU, NumGPUs: i.NumGPUs,
			Bid: i.IsBid, DPH: i.DPHTotal, Storage: i.StorageTotal,
			Status: i.StatusMsg, Uptime: i.UptimeMins,
		}
		switch {
		case i.Outbid():
			v.State = "outbid"
		case i.Stopped():
			v.State = "stopped"
		case i.Booting():
			v.State = "booting"
		default:
			v.State = "running"
		}
		if i.IsBid {
			v.MyBid, v.MinBid = i.DPHBase, i.MinBid
			v.NextBid = vast.NextBid(i.MinBid, 0.10, 0.005)
			// Escalate off our own price when the floor is behind it, because a raise
			// that lands below what we are already bidding achieves nothing — and that
			// is the exact case where min_bid has been seen lying.
			if v.NextBid <= i.DPHBase {
				v.NextBid = vast.NextBid(i.DPHBase, 0.10, 0.005)
			}
		}
		if t, ok := r.stoppedAt[i.ID]; ok {
			v.StoppedS = now.Sub(t).Seconds()
		}
		out.Instances = append(out.Instances, v)
	}
	return out
}

/* -------------------------------------------------------------- commands --- */

type agentCommand struct {
	Cmd   string  `json:"cmd"`
	Key   string  `json:"key,omitempty"`
	ID    int64   `json:"id,omitempty"`
	Price float64 `json:"price,omitempty"`
}

// handleAgentCommand runs one instruction addressed at the agent itself.
//
// Everything destructive names an instance explicitly. There is no "stop everything",
// deliberately: a mis-click on a fleet page should cost one machine, and anybody who
// genuinely wants to tear down a fleet can afford to press a button per box.
func (a *Agent) handleAgentCommand(ctx context.Context, raw json.RawMessage) json.RawMessage {
	var cmd agentCommand
	if err := json.Unmarshal(raw, &cmd); err != nil {
		return reply(map[string]any{"error": "unreadable command"})
	}

	switch cmd.Cmd {
	case "rentals.status":
		// falls through to the report below

	case "rentals.configure":
		if cmd.Key != "" {
			// Checked before it is stored, so a typo is reported now rather than as an
			// empty fleet later.
			if _, _, err := vast.New(strings.TrimSpace(cmd.Key)).Whoami(ctx); err != nil {
				return reply(map[string]any{"error": err.Error()})
			}
		}
		if err := a.rentals.setKey(cmd.Key); err != nil {
			return reply(map[string]any{"error": err.Error()})
		}
		a.rentals.refresh(ctx)

	case "rentals.bid", "rentals.start", "rentals.stop", "rentals.destroy":
		c := a.rentals.client()
		if c == nil {
			return reply(map[string]any{"error": "no Vast key on this machine"})
		}
		if cmd.ID == 0 {
			return reply(map[string]any{"error": "which instance?"})
		}
		var err error
		switch cmd.Cmd {
		case "rentals.bid":
			if cmd.Price <= 0 {
				return reply(map[string]any{"error": "a bid needs a price"})
			}
			err = c.SetBid(ctx, cmd.ID, cmd.Price)
			if err == nil {
				// A raised bid does nothing on its own: the instance is stopped and has
				// to be asked to run. Vast will refuse if the price still does not
				// clear, which is information rather than a failure.
				_ = c.Start(ctx, cmd.ID)
			}
		case "rentals.start":
			err = c.Start(ctx, cmd.ID)
		case "rentals.stop":
			err = c.Stop(ctx, cmd.ID)
		case "rentals.destroy":
			err = c.Destroy(ctx, cmd.ID)
		}
		if err != nil {
			a.log.Warn("rental command failed", "cmd", cmd.Cmd, "id", cmd.ID, "err", err)
			return reply(map[string]any{"error": err.Error()})
		}
		a.log.Info("rental command", "cmd", cmd.Cmd, "id", cmd.ID, "price", cmd.Price)
		a.rentals.refresh(ctx)

	default:
		return reply(map[string]any{"error": "unknown command " + cmd.Cmd})
	}

	return reply(map[string]any{"rentals": a.rentals.report()})
}

func reply(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{"error":"could not encode the reply"}`)
	}
	return b
}

// watchRentals keeps the picture current and pushes it up as it changes.
func (a *Agent) watchRentals(ctx context.Context, up upSender) {
	if a.rentals == nil {
		return
	}
	t := time.NewTicker(rentalPoll)
	defer t.Stop()
	var last string
	for {
		if a.rentals.configured() {
			a.rentals.refresh(ctx)
			// Only when something changed. A fleet sitting still should not cost a
			// frame every forty-five seconds for the life of the connection.
			if b, err := json.Marshal(a.rentals.report()); err == nil && string(b) != last {
				last = string(b)
				if err := up.send(p.Frame{Type: p.TypeFromAgent,
					Data: reply(map[string]any{"rentals": a.rentals.report()})}); err != nil {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type upSender interface{ send(p.Frame) error }

var errNoSender = errors.New("no relay connection")

func describeInstance(v RentalView) string {
	if v.Label != "" {
		return v.Label
	}
	if v.NumGPUs > 1 {
		return fmt.Sprintf("%d× %s", v.NumGPUs, v.GPU)
	}
	return v.GPU
}
