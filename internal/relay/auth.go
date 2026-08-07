// Package relay forwards frames between a browser and the folding agents it owns.
//
// It exists because a browser cannot reach a folding client directly. The client binds
// loopback and has no authentication, so it must never face the network; and a page
// served over HTTPS cannot open an insecure websocket to a public address at all —
// Chrome rejects it at construction, before any connection is attempted. Even with a
// certificate on every box, most machines worth folding on sit behind NAT with no
// inbound port. An outbound connection from the machine solves all three at once, and
// something has to be on the other end of it.
//
// The relay is deliberately incurious. It verifies that a connection holds a private
// key, that the key belongs to an owner, and then it moves opaque frames between the
// two. It does not parse the folding protocol, store any folding data, or hold a
// credential belonging to anybody.
//
// The wire format lives in relayproto, shared with the agent, because both ends have
// to build the signed bytes identically.
package relay

import (
	"sync"
	"time"
)

// nonces remembers spent enrolment nonces until they expire.
//
// Without it a token is reusable until its own expiry, so one leaked line of
// provisioning output would enrol a machine as often as somebody liked inside that
// window. With it, a token is worth exactly one machine.
type nonces struct {
	mu   sync.Mutex
	seen map[string]int64
}

func newNonces() *nonces { return &nonces{seen: map[string]int64{}} }

// use records a nonce, reporting whether it was fresh.
func (n *nonces) use(nonce string, exp int64, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	// Sweep on write. The map only ever holds nonces from unexpired tokens, which at a
	// thirty-minute ceiling is bounded by how fast anybody can enrol.
	for k, v := range n.seen {
		if v <= now.Unix() {
			delete(n.seen, k)
		}
	}
	if _, ok := n.seen[nonce]; ok {
		return false
	}
	n.seen[nonce] = exp
	return true
}
