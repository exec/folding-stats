package vast

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// The field semantics are the whole value of this package, and every one of them is a
// trap somebody already fell into. These are the field values from real instances.
func TestStateSemantics(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		in                       Instance
		stopped, outbid, booting bool
	}{
		{
			// The case that made an early watcher log zero evictions forever: an
			// evicted box says intended_status=stopped, and so does one somebody
			// stopped by hand. Only cur_state and the bid tell them apart.
			name:    "evicted interruptible",
			in:      Instance{IsBid: true, CurState: "stopped", ActualStatus: "exited", MinBid: 0.0928, DPHBase: 0.069},
			stopped: true, outbid: true,
		},
		{
			name:    "stopped by hand, bid still clears",
			in:      Instance{IsBid: true, CurState: "stopped", ActualStatus: "exited", MinBid: 0.05, DPHBase: 0.069},
			stopped: true, outbid: false,
		},
		{
			// The case that made a bidder bid against itself on every launch: a
			// booting instance is cur_state=running with actual_status=loading, and
			// "actual_status != running" reads that as broken.
			name:    "booting",
			in:      Instance{IsBid: true, CurState: "running", ActualStatus: "loading"},
			stopped: false, outbid: false, booting: true,
		},
		{
			name: "healthy",
			in:   Instance{IsBid: true, CurState: "running", ActualStatus: "running"},
		},
		{
			// On-demand contracts are never outbid, whatever min_bid says.
			name:    "on-demand, stopped",
			in:      Instance{IsBid: false, CurState: "stopped", MinBid: 9.99, DPHBase: 0.01},
			stopped: true, outbid: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Stopped(); got != tc.stopped {
				t.Errorf("Stopped() = %v, want %v", got, tc.stopped)
			}
			if got := tc.in.Outbid(); got != tc.outbid {
				t.Errorf("Outbid() = %v, want %v", got, tc.outbid)
			}
			if got := tc.in.Booting(); got != tc.booting {
				t.Errorf("Booting() = %v, want %v", got, tc.booting)
			}
		})
	}
}

// NextBid must always move, and by enough to matter. min_bid flaps by a factor of two
// between polls, so a raise that lands on it exactly is a raise that achieves nothing.
func TestNextBidAlwaysMoves(t *testing.T) {
	for _, floor := range []float64{0, 0.0001, 0.069, 0.16, 0.3649, 2.668} {
		got := NextBid(floor, 0.10, 0.005)
		if got <= floor {
			t.Errorf("NextBid(%v) = %v, which does not clear the floor", floor, got)
		}
		if floor > 0 && got < floor+0.004999 {
			t.Errorf("NextBid(%v) = %v, too small a step to matter", floor, got)
		}
	}
	// The observed case: $0.1600 floor, our bid $0.3240 already above it and still
	// losing. Raising off the floor would go *down*, so a caller must escalate off its
	// own price — this only guarantees a step above whatever it is given.
	if got := NextBid(0.3240, 0.10, 0.005); got <= 0.3240 {
		t.Errorf("escalating off our own bid did not raise it: %v", got)
	}
}

// Parsing a real payload, so a field rename upstream fails here rather than as a
// dashboard that quietly shows every instance as stopped.
func TestParsesARealInstance(t *testing.T) {
	raw, err := os.ReadFile("testdata/instances.json")
	if err != nil {
		t.Skip("no captured payload")
	}
	var out struct {
		Instances []Instance `json:"instances"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Instances) == 0 {
		t.Fatal("no instances parsed")
	}
	i := out.Instances[0]
	if i.ID == 0 || i.GPU == "" || i.DPHTotal == 0 {
		t.Errorf("fields did not populate: %+v", i)
	}
	// Storage is billed while stopped, which is what makes an unwinnable auction cost
	// money. If this stops parsing, the UI silently claims an idle box is free.
	if i.StorageTotal <= 0 || math.IsNaN(i.StorageTotal) {
		t.Errorf("storage_total_cost did not parse: %v", i.StorageTotal)
	}
}
