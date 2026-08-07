// Package agent connects a machine's folding client to the relay.
//
// It is the piece that makes a fleet possible. A browser can reach the folding client
// on the computer it is running on and nowhere else — the client binds loopback, an
// HTTPS page cannot open an insecure websocket to a public address, and most machines
// worth folding on have no inbound port anyway. This runs on the machine, holds a
// connection out to the relay, and forwards frames in both directions.
//
// It is as incurious as the relay. Whatever the folding client says goes up verbatim
// and whatever the browser sends comes down verbatim; the agent adds a signature and
// an envelope and reads neither payload.
package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	p "folding/internal/relayproto"
)

// Config is everything the agent needs to run.
type Config struct {
	Relay string // wss://…/relay/agent
	Local string // ws://127.0.0.1:7396/api/websocket
	// KeyPath holds this machine's identity. It is generated on first run and never
	// leaves the machine — an enrolment token is what travels, and only once.
	KeyPath string
	Name    string
	// Token is an owner-signed enrolment, needed only until the relay knows this key.
	Token *p.Enrolment
	Log   *slog.Logger
}

// Agent runs until its context is cancelled.
type Agent struct {
	cfg  Config
	log  *slog.Logger
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey

	mu    sync.Mutex
	local *websocket.Conn
}

func New(cfg Config) (*Agent, error) {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	pub, priv, err := loadOrCreateKey(cfg.KeyPath)
	if err != nil {
		return nil, err
	}
	return &Agent{cfg: cfg, log: cfg.Log, pub: pub, priv: priv}, nil
}

// Key is this machine's identity, which is also what an owner sees in their fleet.
func (a *Agent) Key() string { return p.B64(a.pub) }

/*
loadOrCreateKey reads the machine's private key, generating one if there is none.

Generated here rather than handed in, deliberately. A private key injected through a
provisioning environment is a private key that ends up in whatever logs that
environment produces — and on Vast those are published to an unauthenticated bucket.
A key that has never left the machine cannot leak that way, and losing it costs one
re-enrolment rather than an impersonation nobody can revoke.
*/
func loadOrCreateKey(path string) (ed25519.PublicKey, ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		raw, decErr := p.UnB64(strings.TrimSpace(string(b)))
		if decErr != nil || len(raw) != ed25519.PrivateKeySize {
			return nil, nil, fmt.Errorf("%s does not contain a usable key", path)
		}
		priv := ed25519.PrivateKey(raw)
		return priv.Public().(ed25519.PublicKey), priv, nil
	}
	if !os.IsNotExist(err) {
		return nil, nil, err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, err
	}
	// 0600 and written through a temporary file: a half-written key on a machine that
	// lost power mid-generation would be indistinguishable from a corrupt one, and the
	// recovery for both is the same but the confusion is not.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(p.B64(priv)), 0o600); err != nil {
		return nil, nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, nil, err
	}
	return pub, priv, nil
}

// Run keeps a connection to the relay for as long as the context lives.
func (a *Agent) Run(ctx context.Context) error {
	a.log.Info("agent starting", "machine", a.Key(), "name", a.cfg.Name, "relay", a.cfg.Relay)

	var attempts int
	for ctx.Err() == nil {
		err := a.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		attempts++
		// A refused enrolment is not a transient failure and retrying it forever only
		// fills a log. Everything else — the relay restarting, a flapping link, a
		// machine that suspended — is worth waiting out.
		if errors.Is(err, errRejected) {
			return err
		}
		wait := time.Duration(min(attempts, 6)) * 10 * time.Second
		a.log.Warn("relay session ended, retrying", "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
	return nil
}

var errRejected = errors.New("relay rejected this machine")

// session is one connection to the relay, and one to the local client beneath it.
func (a *Agent) session(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	up, _, err := websocket.DefaultDialer.DialContext(ctx, a.cfg.Relay, http.Header{})
	if err != nil {
		return fmt.Errorf("dialling the relay: %w", err)
	}
	defer up.Close()

	if err := a.authenticate(up); err != nil {
		return err
	}
	a.log.Info("relay connected", "machine", a.Key())

	// The folding client sends its whole state exactly once, when something connects,
	// and everything after that is a patch against it. So a listener that attaches
	// later gets patches with nothing to apply them to — which is not a hypothetical:
	// the first end-to-end test showed an owner watching a machine it could see was
	// online, receiving nothing but keepalives.
	//
	// The fix is to redial the local client when a listener asks, which makes it send
	// the snapshot again. Redialling rather than caching keeps the agent as incurious
	// as the relay: it never has to know which payloads are snapshots and which are
	// patches, and it will still be right once those payloads are sealed.
	resync := make(chan struct{}, 1)

	errs := make(chan error, 2)
	go func() { errs <- a.localLoop(ctx, up, resync) }()
	go func() { errs <- a.pumpDown(up, resync) }()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errs:
		return err
	}
}

func (a *Agent) authenticate(up *websocket.Conn) error {
	_ = up.SetReadDeadline(time.Now().Add(20 * time.Second))
	var hello p.Frame
	if err := up.ReadJSON(&hello); err != nil {
		return fmt.Errorf("no challenge from the relay: %w", err)
	}
	if hello.Type != p.TypeHello {
		return fmt.Errorf("expected a challenge, got %q", hello.Type)
	}

	auth := p.Frame{
		Type: p.TypeAuth,
		Role: p.RoleAgent,
		Key:  a.Key(),
		Sig:  p.B64(ed25519.Sign(a.priv, p.AuthMessage(p.RoleAgent, hello.Nonce))),
		Name: a.cfg.Name,
		// Sent every time and ignored once the relay knows this key, so a machine that
		// is re-provisioned with the same token does not need different arguments.
		Enrol: a.cfg.Token,
	}
	if err := up.WriteJSON(auth); err != nil {
		return err
	}

	var ack p.Frame
	if err := up.ReadJSON(&ack); err != nil {
		return fmt.Errorf("no answer to authentication: %w", err)
	}
	if ack.Type == p.TypeError {
		return fmt.Errorf("%w: %s", errRejected, ack.Error)
	}
	if ack.Type != p.TypeReady {
		return fmt.Errorf("unexpected reply %q", ack.Type)
	}
	_ = up.SetReadDeadline(time.Time{})
	return nil
}

// localLoop keeps a connection to the folding client, redialling it on request.
func (a *Agent) localLoop(ctx context.Context, up *websocket.Conn, resync <-chan struct{}) error {
	for ctx.Err() == nil {
		local, _, err := websocket.DefaultDialer.DialContext(ctx, a.cfg.Local, http.Header{})
		if err != nil {
			return fmt.Errorf("dialling the folding client: %w", err)
		}
		a.setLocal(local)

		done := make(chan error, 1)
		go func() { done <- a.pumpUp(local, up) }()

		select {
		case <-ctx.Done():
			local.Close()
			return nil

		case err := <-done:
			// The folding client going away is worth ending the session over: the
			// machine has nothing to report until it comes back, and the outer loop's
			// backoff is the right place to wait.
			local.Close()
			a.setLocal(nil)
			return err

		case <-resync:
			a.log.Debug("resyncing: a listener asked for the full state")
			local.Close()
			<-done // let the pump unwind before replacing the connection
			a.setLocal(nil)
		}
	}
	return nil
}

func (a *Agent) setLocal(c *websocket.Conn) {
	a.mu.Lock()
	a.local = c
	a.mu.Unlock()
}

func (a *Agent) getLocal() *websocket.Conn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.local
}

// pumpUp forwards everything the folding client says to the relay.
func (a *Agent) pumpUp(local, up *websocket.Conn) error {
	for {
		_, msg, err := local.ReadMessage()
		if err != nil {
			return fmt.Errorf("folding client closed: %w", err)
		}
		if !json.Valid(msg) {
			continue
		}
		if err := up.WriteJSON(p.Frame{Type: p.TypeUp, Data: json.RawMessage(msg)}); err != nil {
			return fmt.Errorf("relay closed: %w", err)
		}
	}
}

// pumpDown forwards everything the browser sends to the folding client.
func (a *Agent) pumpDown(up *websocket.Conn, resync chan<- struct{}) error {
	for {
		var f p.Frame
		if err := up.ReadJSON(&f); err != nil {
			return fmt.Errorf("relay closed: %w", err)
		}
		switch f.Type {
		case p.TypeResync:
			// Coalesced: three browsers attaching at once want one reconnection, not
			// three, and the snapshot the first one triggers serves all of them.
			select {
			case resync <- struct{}{}:
			default:
			}

		case p.TypeDown:
			if len(f.Data) == 0 {
				continue
			}
			local := a.getLocal()
			if local == nil {
				a.log.Warn("dropping a command: no connection to the folding client")
				continue
			}
			if err := local.WriteMessage(websocket.TextMessage, f.Data); err != nil {
				return fmt.Errorf("folding client closed: %w", err)
			}
		case p.TypeError:
			// The relay only says this about something we asked for, and the agent asks
			// for nothing after authenticating — so it is worth seeing.
			a.log.Warn("relay reported an error", "err", f.Error)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
