package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"strings"
	"testing"
	"time"

	p "folding/internal/relayproto"
)

// TestKeyIsGeneratedOnceAndKept is the property the security model rests on: the
// private key is made here and never travels. If it were regenerated on each start,
// every restart would be a new machine and every fleet would fill with ghosts.
func TestKeyIsGeneratedOnceAndKept(t *testing.T) {
	path := t.TempDir() + "/nested/machine.key"

	a1, err := New(Config{KeyPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if a1.Key() == "" {
		t.Fatal("no key generated")
	}

	a2, err := New(Config{KeyPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if a1.Key() != a2.Key() {
		t.Errorf("restart produced a different identity:\n  %s\n  %s", a1.Key(), a2.Key())
	}

	// A key readable by other users on a shared box is a key that can be copied.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode is %v, want 0600", fi.Mode().Perm())
	}
}

func TestRefusesAnUnusableKeyFile(t *testing.T) {
	path := t.TempDir() + "/machine.key"
	if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{KeyPath: path}); err == nil {
		t.Error("accepted a corrupt key file instead of saying so")
	} else if !strings.Contains(err.Error(), "usable key") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// The signed bytes must match what the relay builds. This is the one place a silent
// incompatibility could hide: a mismatch fails as "signature does not match the key",
// which points at key handling rather than at the protocol.
func TestSignedBytesMatchTheProtocol(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	msg := p.AuthMessage(p.RoleAgent, "a-nonce")
	if got := string(msg); got != "folding-relay-auth\x00agent\x00a-nonce" {
		t.Errorf("auth message shape changed: %q", got)
	}
	if !ed25519.Verify(pub, msg, ed25519.Sign(priv, msg)) {
		t.Error("a signature over the agreed bytes did not verify")
	}

	// An owner's token, minted and checked by the same rules both ends use.
	opub, opriv, _ := ed25519.GenerateKey(rand.Reader)
	tok, err := p.Mint(opub, opriv, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := tok.Verify(time.Now())
	if err != nil {
		t.Fatalf("freshly minted token did not verify: %v", err)
	}
	if p.B64(owner) != p.B64(opub) {
		t.Error("token verified as the wrong owner")
	}
	if _, err := p.Mint(opub, opriv, 48*time.Hour); err == nil {
		t.Error("minted a token that outlives the ceiling")
	}
}
