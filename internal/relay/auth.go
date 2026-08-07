// Package relay forwards frames between a browser and the folding agents it owns.
//
// It exists because a browser cannot reach a folding client directly. The client
// binds loopback and has no authentication, so it must never face the network; and a
// page served over HTTPS cannot open an insecure websocket to a public address at all
// — Chrome rejects it at construction, before any connection is attempted. Even with a
// certificate on every box, most machines worth folding on sit behind NAT with no
// inbound port. An outbound connection from the machine solves all three at once, and
// something has to be on the other end of it.
//
// The relay is deliberately incurious. It verifies that a connection holds a private
// key, that the key belongs to an owner, and then it moves opaque frames between the
// two. It does not parse the folding protocol, store any folding data, or hold a
// credential belonging to anybody.
package relay

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Domain separation. A signature is only meaningful together with a statement of what
// it was for: without these prefixes an enrolment token and a login challenge are both
// "a signature over some bytes", and one could be replayed as the other.
const (
	authContext  = "folding-relay-auth\x00"
	enrolContext = "folding-relay-enrol\x00"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// parseKey reads a public key, refusing anything that is not one.
func parseKey(s string) (ed25519.PublicKey, error) {
	b, err := unb64(s)
	if err != nil {
		return nil, fmt.Errorf("public key is not base64: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key is %d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// authMessage is what a connecting party signs to prove it holds the key it claims.
func authMessage(role, nonce string) []byte {
	return []byte(authContext + role + "\x00" + nonce)
}

// Enrolment is a machine's claim to belong to an owner.
//
// Signed by the owner rather than issued by the relay, which is what keeps the relay
// out of the credential business: it stores no tokens and can mint none. Anyone
// holding the owner's private key can authorise a machine; the relay only checks the
// arithmetic.
type Enrolment struct {
	Owner string `json:"owner"`
	Exp   int64  `json:"exp"`
	Nonce string `json:"nonce"`
	Sig   string `json:"sig"`
}

func enrolMessage(owner string, exp int64, nonce string) []byte {
	return []byte(enrolContext + owner + "\x00" + strconv.FormatInt(exp, 10) + "\x00" + nonce)
}

// Verify checks the token and returns the owner it authorises for.
func (e Enrolment) Verify(now time.Time) (ed25519.PublicKey, error) {
	owner, err := parseKey(e.Owner)
	if err != nil {
		return nil, err
	}
	if e.Exp <= now.Unix() {
		return nil, fmt.Errorf("enrolment token expired")
	}
	// A token good for a week is a token that will be found in a log a week later.
	// Vast publishes instance logs to an unauthenticated bucket, so anything that
	// travels in an environment variable has to be short-lived by construction.
	if e.Exp > now.Add(MaxEnrolLifetime).Unix() {
		return nil, fmt.Errorf("enrolment token valid for too long")
	}
	sig, err := unb64(e.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("malformed enrolment signature")
	}
	if !ed25519.Verify(owner, enrolMessage(e.Owner, e.Exp, e.Nonce), sig) {
		return nil, fmt.Errorf("enrolment signature does not match the owner key")
	}
	return owner, nil
}

// MaxEnrolLifetime bounds how long an enrolment token may be valid for.
const MaxEnrolLifetime = 30 * time.Minute

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
	// Sweep on write. The map only ever holds nonces from unexpired tokens, which at
	// a thirty-minute ceiling is bounded by how fast anybody can enrol.
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
