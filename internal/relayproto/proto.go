// Package relayproto is the wire protocol shared by the relay and the agent.
//
// It lives in one place because the two ends must agree byte for byte. A signature is
// computed over a string built here, and if the two sides built it differently — one
// character of domain separator, one field in a different order — every signature
// would fail and the error would read "signature does not match the key". That points
// at key handling, which is the wrong place to look, and it is exactly the kind of
// duplicated fact that has already gone wrong twice in this project when it was
// written down in two places.
package relayproto

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Domain separation. A signature only means something alongside a statement of what it
// was for: without these prefixes an enrolment token and a login challenge are both
// "a signature over some bytes", and either could be replayed as the other.
const (
	AuthContext  = "folding-relay-auth\x00"
	EnrolContext = "folding-relay-enrol\x00"
)

// MaxEnrolLifetime bounds how long an enrolment token may be valid for.
//
// Short by design. On Vast a token travels in an environment variable, and instance
// logs are published to an unauthenticated bucket — so a token that outlives its
// provisioning run is a token somebody will find later.
const MaxEnrolLifetime = 30 * time.Minute

// Roles, as they appear in a signed challenge.
const (
	RoleAgent = "agent"
	RoleOwner = "owner"
)

func B64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func UnB64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// ParseKey reads a public key, refusing anything that is not one.
func ParseKey(s string) (ed25519.PublicKey, error) {
	b, err := UnB64(s)
	if err != nil {
		return nil, fmt.Errorf("public key is not base64: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key is %d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// AuthMessage is what a connecting party signs to prove it holds the key it claims.
func AuthMessage(role, nonce string) []byte {
	return []byte(AuthContext + role + "\x00" + nonce)
}

// EnrolMessage is what an owner signs to authorise a machine.
func EnrolMessage(owner string, exp int64, nonce string) []byte {
	return []byte(EnrolContext + owner + "\x00" + strconv.FormatInt(exp, 10) + "\x00" + nonce)
}

// Enrolment is a machine's claim to belong to an owner.
//
// Signed by the owner rather than issued by the relay, which is what keeps the relay
// out of the credential business: it stores no tokens and can mint none.
type Enrolment struct {
	Owner string `json:"owner"`
	Exp   int64  `json:"exp"`
	Nonce string `json:"nonce"`
	Sig   string `json:"sig"`
}

// Mint creates a token authorising one machine, signed by the owner's key.
func Mint(owner ed25519.PublicKey, priv ed25519.PrivateKey, life time.Duration) (*Enrolment, error) {
	if life <= 0 || life > MaxEnrolLifetime {
		return nil, fmt.Errorf("token lifetime must be between 0 and %s", MaxEnrolLifetime)
	}
	nb := make([]byte, 12)
	if _, err := rand.Read(nb); err != nil {
		return nil, err
	}
	e := &Enrolment{Owner: B64(owner), Exp: time.Now().Add(life).Unix(), Nonce: B64(nb)}
	e.Sig = B64(ed25519.Sign(priv, EnrolMessage(e.Owner, e.Exp, e.Nonce)))
	return e, nil
}

// Verify checks a token and returns the owner it authorises for.
func (e Enrolment) Verify(now time.Time) (ed25519.PublicKey, error) {
	owner, err := ParseKey(e.Owner)
	if err != nil {
		return nil, err
	}
	if e.Exp <= now.Unix() {
		return nil, fmt.Errorf("enrolment token expired")
	}
	if e.Exp > now.Add(MaxEnrolLifetime).Unix() {
		return nil, fmt.Errorf("enrolment token valid for too long")
	}
	sig, err := UnB64(e.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("malformed enrolment signature")
	}
	if !ed25519.Verify(owner, EnrolMessage(e.Owner, e.Exp, e.Nonce), sig) {
		return nil, fmt.Errorf("enrolment signature does not match the owner key")
	}
	return owner, nil
}

// Frame is everything either end understands.
//
// Data is json.RawMessage and stays that way at every hop: neither the relay nor the
// agent looks inside. That is what lets the folding protocol — or later, sealed
// ciphertext — travel the same pipe without anything in the middle learning either.
type Frame struct {
	Type    string          `json:"type"`
	Nonce   string          `json:"nonce,omitempty"`
	Role    string          `json:"role,omitempty"`
	Key     string          `json:"pubkey,omitempty"`
	Sig     string          `json:"sig,omitempty"`
	Name    string          `json:"name,omitempty"`
	Enrol   *Enrolment      `json:"enrol,omitempty"`
	Machine string          `json:"machine,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`

	Machines []MachineView `json:"machines,omitempty"`
}

// Frame types.
const (
	TypeHello    = "hello"    // relay -> either: here is a challenge
	TypeAuth     = "auth"     // either -> relay: here is my signature
	TypeReady    = "ready"    // relay -> either: you are connected
	TypeMachines = "machines" // relay -> owner: your fleet, and who is online
	TypeUp       = "up"       // agent -> relay: from my folding client
	TypeFrom     = "from"     // relay -> owner: from one of your machines
	TypeTo       = "to"       // owner -> relay: for one of my machines
	TypeDown     = "down"     // relay -> agent: for your folding client
	TypeResync   = "resync"   // owner -> relay -> agent: I just attached, send me everything
	TypeForget   = "forget"   // owner -> relay: revoke a machine
	TypeError    = "error"
)

// MachineView is what an owner is told about one of their machines.
type MachineView struct {
	Key      string    `json:"key"`
	Name     string    `json:"name,omitempty"`
	Online   bool      `json:"online"`
	LastSeen time.Time `json:"last_seen,omitzero"`
}
