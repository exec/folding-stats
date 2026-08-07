package relay

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	p "folding/internal/relayproto"
)

// Frame and MachineView are the shared wire types, re-exported so callers of this
// package do not need to name two packages to use one protocol.
type (
	Frame       = p.Frame
	MachineView = p.MachineView
	Enrolment   = p.Enrolment
)

func b64(b []byte) string                          { return p.B64(b) }
func unb64(s string) ([]byte, error)               { return p.UnB64(s) }
func parseKey(s string) (ed25519.PublicKey, error) { return p.ParseKey(s) }
func authMessage(role, nonce string) []byte        { return p.AuthMessage(role, nonce) }
func enrolMessage(owner string, exp int64, nonce string) []byte {
	return p.EnrolMessage(owner, exp, nonce)
}

// MaxEnrolLifetime is re-exported for the same reason.
const MaxEnrolLifetime = p.MaxEnrolLifetime

// Timings.
//
// pingEvery is the load-bearing one. Cloudflare closes an idle websocket after about
// a hundred seconds, and a paused machine sends nothing at all — so without traffic of
// our own a healthy idle agent would be disconnected every couple of minutes and
// reconnect forever. Thirty seconds is comfortably inside that with room for a slow
// link.
const (
	pingEvery   = 30 * time.Second
	pongWait    = 90 * time.Second
	writeWait   = 10 * time.Second
	authTimeout = 15 * time.Second
	// A folding client's whole state snapshot is a few kilobytes; the viz topology it
	// can be asked for is megabytes, but that is not something this relay carries.
	maxFrame = 1 << 20
)

// conn is one authenticated websocket, either an agent or an owner's browser.
type conn struct {
	ws    *websocket.Conn
	send  chan []byte
	key   string // this party's public key
	owner string // the owner it belongs to; for a browser, itself
	agent bool
	once  sync.Once
}

func (c *conn) close() {
	c.once.Do(func() { close(c.send) })
}

// push queues a frame, dropping the connection rather than blocking the hub.
//
// A browser on a bad link that stops reading must not be able to stall every agent
// writing to it. The channel is the backpressure, and overflowing it is fatal to that
// one connection and nothing else.
func (c *conn) push(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	select {
	case c.send <- b:
	default:
		c.close()
	}
}

// Hub routes frames between owners and their machines.
type Hub struct {
	store  *Store
	log    *slog.Logger
	nonces *nonces

	mu      sync.RWMutex
	agents  map[string]*conn   // machine key -> agent
	owners  map[string][]*conn // owner key  -> browsers
	upgrade websocket.Upgrader
}

func New(store *Store, log *slog.Logger) *Hub {
	return &Hub{
		store:  store,
		log:    log,
		nonces: newNonces(),
		agents: map[string]*conn{},
		owners: map[string][]*conn{},
		upgrade: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// Origin is not a security boundary here and pretending otherwise would be
			// worse than useless: agents are not browsers and send no Origin at all,
			// and every request that reaches a browser-facing endpoint has already had
			// to produce an ed25519 signature. The signature is the boundary.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
}

func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/relay/agent", func(w http.ResponseWriter, r *http.Request) { h.serve(w, r, true) })
	mux.HandleFunc("/relay/browser", func(w http.ResponseWriter, r *http.Request) { h.serve(w, r, false) })
	mux.HandleFunc("/relay/health", func(w http.ResponseWriter, r *http.Request) {
		h.mu.RLock()
		a, o := len(h.agents), len(h.owners)
		h.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]any{
			"agents_online": a, "owners_online": o, "machines_enrolled": h.store.Count(),
		})
	})
	return mux
}

func (h *Hub) serve(w http.ResponseWriter, r *http.Request, agent bool) {
	ws, err := h.upgrade.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(maxFrame)

	c, err := h.authenticate(ws, agent)
	if err != nil {
		// The reason goes to the client because every one of them is actionable: a bad
		// signature, an expired token, a machine that belongs to somebody else. None of
		// it tells an attacker anything they did not already supply.
		_ = ws.SetWriteDeadline(time.Now().Add(writeWait))
		_ = ws.WriteJSON(Frame{Type: "error", Error: err.Error()})
		ws.Close()
		return
	}

	// Success has to say so. Without an acknowledgement a client cannot tell an
	// accepted connection from one still being considered, and the only way to find out
	// would be to wait and see whether anything ever arrives — which for an idle agent
	// is indistinguishable from working correctly.
	c.push(Frame{Type: "ready", Key: c.key})

	h.add(c)
	defer h.remove(c)

	go h.writer(c)
	h.reader(c)
}

// authenticate runs the challenge before anything else is allowed to happen.
func (h *Hub) authenticate(ws *websocket.Conn, agent bool) (*conn, error) {
	nb := make([]byte, 32)
	if _, err := rand.Read(nb); err != nil {
		return nil, errText("relay could not generate a challenge")
	}
	nonce := b64(nb)

	_ = ws.SetWriteDeadline(time.Now().Add(writeWait))
	if err := ws.WriteJSON(Frame{Type: "hello", Nonce: nonce}); err != nil {
		return nil, errText("could not send challenge")
	}

	_ = ws.SetReadDeadline(time.Now().Add(authTimeout))
	var f Frame
	if err := ws.ReadJSON(&f); err != nil {
		return nil, errText("no authentication received")
	}
	if f.Type != "auth" {
		return nil, errText("expected an auth frame")
	}

	role := "owner"
	if agent {
		role = "agent"
	}
	key, err := parseKey(f.Key)
	if err != nil {
		return nil, err
	}
	sig, err := unb64(f.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, errText("malformed signature")
	}
	if !ed25519.Verify(key, authMessage(role, nonce), sig) {
		return nil, errText("signature does not match the key")
	}

	now := time.Now().UTC()
	c := &conn{ws: ws, send: make(chan []byte, 64), key: f.Key, agent: agent}

	if !agent {
		// A browser owns itself. There is nothing to enrol and nothing to look up: the
		// key it just proved it holds is the identity its fleet hangs from.
		c.owner = f.Key
		return c, nil
	}

	m, ok := h.store.Get(f.Key)
	if !ok {
		if f.Enrol == nil {
			return nil, errText("this machine is not enrolled and sent no enrolment token")
		}
		owner, err := f.Enrol.Verify(now)
		if err != nil {
			return nil, err
		}
		if !h.nonces.use(f.Enrol.Nonce, f.Enrol.Exp, now) {
			return nil, errText("enrolment token has already been used")
		}
		if m, err = h.store.Enrol(f.Key, b64(owner), f.Name, now); err != nil {
			return nil, err
		}
		h.log.Info("machine enrolled", "machine", short(f.Key), "owner", short(m.Owner), "name", f.Name)
	} else if f.Name != "" && f.Name != m.Name {
		_, _ = h.store.Enrol(f.Key, m.Owner, f.Name, now)
	}

	c.owner = m.Owner
	h.store.Touch(f.Key, now)
	return c, nil
}

func (h *Hub) add(c *conn) {
	h.mu.Lock()
	if c.agent {
		if old := h.agents[c.key]; old != nil {
			// One machine, one connection. A box that reconnects before the relay has
			// noticed the old socket died would otherwise be present twice, and frames
			// would go to whichever copy the map happened to hold.
			old.close()
		}
		h.agents[c.key] = c
	} else {
		h.owners[c.owner] = append(h.owners[c.owner], c)
	}
	h.mu.Unlock()

	if c.agent {
		h.notifyOwner(c.owner)
	} else {
		c.push(h.fleetFrame(c.owner))
	}
}

func (h *Hub) remove(c *conn) {
	h.mu.Lock()
	if c.agent {
		if h.agents[c.key] == c {
			delete(h.agents, c.key)
		}
	} else {
		list := h.owners[c.owner][:0]
		for _, o := range h.owners[c.owner] {
			if o != c {
				list = append(list, o)
			}
		}
		if len(list) == 0 {
			delete(h.owners, c.owner)
		} else {
			h.owners[c.owner] = list
		}
	}
	h.mu.Unlock()
	c.close()
	if c.agent {
		h.store.Touch(c.key, time.Now().UTC())
		h.notifyOwner(c.owner)
	}
}

func (h *Hub) fleetFrame(owner string) Frame {
	machines := h.store.Owned(owner)
	views := make([]MachineView, 0, len(machines))
	h.mu.RLock()
	for _, m := range machines {
		_, online := h.agents[m.Key]
		views = append(views, MachineView{Key: m.Key, Name: m.Name, Online: online, LastSeen: m.LastSeen})
	}
	h.mu.RUnlock()
	return Frame{Type: "machines", Machines: views}
}

func (h *Hub) notifyOwner(owner string) {
	f := h.fleetFrame(owner)
	h.mu.RLock()
	conns := append([]*conn(nil), h.owners[owner]...)
	h.mu.RUnlock()
	for _, c := range conns {
		c.push(f)
	}
}

func (h *Hub) reader(c *conn) {
	defer c.ws.Close()
	_ = c.ws.SetReadDeadline(time.Now().Add(pongWait))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		var f Frame
		if err := c.ws.ReadJSON(&f); err != nil {
			return
		}
		_ = c.ws.SetReadDeadline(time.Now().Add(pongWait))

		switch {
		case f.Type == "up" && c.agent:
			// From a machine, to every browser its owner has open.
			out := Frame{Type: "from", Machine: c.key, Data: f.Data}
			h.mu.RLock()
			conns := append([]*conn(nil), h.owners[c.owner]...)
			h.mu.RUnlock()
			for _, o := range conns {
				o.push(out)
			}

		case f.Type == "to" && !c.agent:
			// From a browser, to one machine — but only if that machine is theirs. This
			// single check is the whole authorisation model, so it reads the store
			// rather than trusting anything in the frame.
			m, ok := h.store.Get(f.Machine)
			if !ok || m.Owner != c.owner {
				c.push(Frame{Type: "error", Machine: f.Machine, Error: "not your machine"})
				continue
			}
			h.mu.RLock()
			a := h.agents[f.Machine]
			h.mu.RUnlock()
			if a == nil {
				c.push(Frame{Type: "error", Machine: f.Machine, Error: "machine is offline"})
				continue
			}
			a.push(Frame{Type: "down", Data: f.Data})

		case f.Type == p.TypeResync && !c.agent:
			// A listener that attached after the machine did has missed the snapshot the
			// folding client only sends on connect, and patches are meaningless without
			// it. Asking is the listener's job — the relay holds no state and must not
			// start caching payloads it is supposed to be unable to read.
			m, ok := h.store.Get(f.Machine)
			if !ok || m.Owner != c.owner {
				c.push(Frame{Type: p.TypeError, Machine: f.Machine, Error: "not your machine"})
				continue
			}
			h.mu.RLock()
			a := h.agents[f.Machine]
			h.mu.RUnlock()
			if a != nil {
				a.push(Frame{Type: p.TypeResync})
			}

		case f.Type == "forget" && !c.agent:
			if err := h.store.Forget(f.Machine, c.owner); err != nil {
				c.push(Frame{Type: "error", Machine: f.Machine, Error: err.Error()})
				continue
			}
			h.mu.RLock()
			a := h.agents[f.Machine]
			h.mu.RUnlock()
			if a != nil {
				a.close()
			}
			h.notifyOwner(c.owner)

		case f.Type == "ping":
			c.push(Frame{Type: "pong"})
		}
	}
}

func (h *Hub) writer(c *conn) {
	t := time.NewTicker(pingEvery)
	defer func() {
		t.Stop()
		c.ws.Close()
	}()
	for {
		select {
		case b, ok := <-c.send:
			if !ok {
				_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
				_ = c.ws.WriteMessage(websocket.CloseMessage, nil)
				return
			}
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteMessage(websocket.TextMessage, b); err != nil {
				return
			}
		case <-t.C:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

type textErr string

func (e textErr) Error() string { return string(e) }
func errText(s string) error    { return textErr(s) }

// short abbreviates a key for logs. A full key is 43 characters and identifies a
// machine; the first eight are enough to follow one through a log file.
func short(key string) string {
	if len(key) <= 8 {
		return key
	}
	return key[:8]
}
