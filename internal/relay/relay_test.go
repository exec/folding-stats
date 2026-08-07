package relay

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

type party struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newParty(t *testing.T) party {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return party{pub, priv}
}

func (p party) id() string { return b64(p.pub) }

// token mints an enrolment the way a browser would.
func (p party) token(exp time.Time, nonce string) *Enrolment {
	e := &Enrolment{Owner: p.id(), Exp: exp.Unix(), Nonce: nonce}
	e.Sig = b64(ed25519.Sign(p.priv, enrolMessage(e.Owner, e.Exp, e.Nonce)))
	return e
}

func testHub(t *testing.T) (*httptest.Server, *Hub) {
	t.Helper()
	st, err := OpenStore(t.TempDir() + "/machines.json")
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, quiet())
	return httptest.NewServer(h.Handler()), h
}

// dial connects and completes the challenge, returning the socket and any refusal.
func dial(t *testing.T, srv *httptest.Server, path string, p party, role string, f func(*Frame)) (*websocket.Conn, string) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + path
	ws, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	var hello Frame
	if err := ws.ReadJSON(&hello); err != nil {
		t.Fatalf("hello: %v", err)
	}
	auth := Frame{
		Type: "auth", Role: role, Key: p.id(),
		Sig: b64(ed25519.Sign(p.priv, authMessage(role, hello.Nonce))),
	}
	if f != nil {
		f(&auth)
	}
	if err := ws.WriteJSON(auth); err != nil {
		t.Fatalf("auth: %v", err)
	}
	var reply Frame
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := ws.ReadJSON(&reply); err != nil {
		return ws, "no reply: " + err.Error()
	}
	if reply.Type == "error" {
		return ws, reply.Error
	}
	if reply.Type != "ready" {
		return ws, "expected ready, got " + reply.Type
	}
	return ws, ""
}

// TestOwnerCannotReachAnotherOwnersMachine is the property the whole relay exists to
// enforce. Everything else here is plumbing; this is the part that must never be
// wrong, so it is checked from the outside rather than by unit-testing the lookup.
func TestOwnerCannotReachAnotherOwnersMachine(t *testing.T) {
	srv, _ := testHub(t)
	defer srv.Close()

	alice, bob, machine := newParty(t), newParty(t), newParty(t)

	// Alice enrols a machine.
	ag, refusal := dial(t, srv, "/relay/agent", machine, "agent", func(f *Frame) {
		f.Enrol = alice.token(time.Now().Add(5*time.Minute), "n1")
		f.Name = "alices-box"
	})
	if refusal != "" {
		t.Fatalf("enrolment refused: %s", refusal)
	}
	defer ag.Close()

	// Bob authenticates fine — anyone may hold a key — and is then told nothing about
	// machines that are not his.
	bws, refusal := dial(t, srv, "/relay/browser", bob, "owner", nil)
	if refusal != "" {
		t.Fatalf("bob could not connect: %s", refusal)
	}
	defer bws.Close()
	var fleet Frame
	_ = bws.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := bws.ReadJSON(&fleet); err != nil {
		t.Fatal(err)
	}
	if len(fleet.Machines) != 0 {
		t.Errorf("bob was shown %d machines that are not his", len(fleet.Machines))
	}

	// And addressing it directly is refused rather than delivered.
	if err := bws.WriteJSON(Frame{Type: "to", Machine: machine.id(), Data: json.RawMessage(`{"cmd":"dump"}`)}); err != nil {
		t.Fatal(err)
	}
	var reply Frame
	_ = bws.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := bws.ReadJSON(&reply); err != nil {
		t.Fatal(err)
	}
	if reply.Type != "error" || !strings.Contains(reply.Error, "not your machine") {
		t.Fatalf("bob reached alice's machine: %+v", reply)
	}

	// Nothing arrived at the agent.
	_ = ag.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	var leaked Frame
	if err := ag.ReadJSON(&leaked); err == nil {
		t.Fatalf("a frame reached the wrong machine: %+v", leaked)
	}
}

func TestOwnerReachesItsOwnMachine(t *testing.T) {
	srv, _ := testHub(t)
	defer srv.Close()
	alice, machine := newParty(t), newParty(t)

	ag, refusal := dial(t, srv, "/relay/agent", machine, "agent", func(f *Frame) {
		f.Enrol = alice.token(time.Now().Add(5*time.Minute), "n1")
		f.Name = "box"
	})
	if refusal != "" {
		t.Fatal(refusal)
	}
	defer ag.Close()

	bws, refusal := dial(t, srv, "/relay/browser", alice, "owner", nil)
	if refusal != "" {
		t.Fatal(refusal)
	}
	defer bws.Close()

	// The fleet listing names it and reports it online.
	var fleet Frame
	_ = bws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		if err := bws.ReadJSON(&fleet); err != nil {
			t.Fatal(err)
		}
		if fleet.Type == "machines" && len(fleet.Machines) > 0 {
			break
		}
	}
	if fleet.Machines[0].Key != machine.id() || !fleet.Machines[0].Online || fleet.Machines[0].Name != "box" {
		t.Fatalf("unexpected fleet: %+v", fleet.Machines[0])
	}

	// A command goes down untouched, and a reply comes back tagged with its machine.
	if err := bws.WriteJSON(Frame{Type: "to", Machine: machine.id(),
		Data: json.RawMessage(`{"cmd":"state","state":"fold"}`)}); err != nil {
		t.Fatal(err)
	}
	var down Frame
	_ = ag.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := ag.ReadJSON(&down); err != nil {
		t.Fatal(err)
	}
	if down.Type != "down" || string(down.Data) != `{"cmd":"state","state":"fold"}` {
		t.Fatalf("frame altered in transit: %+v", down)
	}

	if err := ag.WriteJSON(Frame{Type: "up", Data: json.RawMessage(`["units",0,"eta","1h"]`)}); err != nil {
		t.Fatal(err)
	}
	var up Frame
	_ = bws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		if err := bws.ReadJSON(&up); err != nil {
			t.Fatal(err)
		}
		if up.Type == "from" {
			break
		}
	}
	if up.Machine != machine.id() || string(up.Data) != `["units",0,"eta","1h"]` {
		t.Fatalf("reply altered or mislabelled: %+v", up)
	}
}

func TestAuthenticationRefusals(t *testing.T) {
	srv, _ := testHub(t)
	defer srv.Close()
	alice, machine, impostor := newParty(t), newParty(t), newParty(t)

	for _, tc := range []struct {
		name  string
		mut   func(*Frame)
		party party
		want  string
	}{
		{"signature by the wrong key", func(f *Frame) {
			f.Key = impostor.id() // signed by machine, claims to be impostor
			f.Enrol = alice.token(time.Now().Add(5*time.Minute), "a")
		}, machine, "signature does not match"},
		{"no enrolment at all", func(f *Frame) {}, machine, "not enrolled"},
		{"expired token", func(f *Frame) {
			f.Enrol = alice.token(time.Now().Add(-time.Minute), "b")
		}, machine, "expired"},
		{"token valid for a month", func(f *Frame) {
			f.Enrol = alice.token(time.Now().Add(720*time.Hour), "c")
		}, machine, "too long"},
		{"token signed by nobody", func(f *Frame) {
			e := alice.token(time.Now().Add(5*time.Minute), "d")
			e.Sig = b64(make([]byte, ed25519.SignatureSize))
			f.Enrol = e
		}, machine, "does not match the owner key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, refusal := dial(t, srv, "/relay/agent", tc.party, "agent", tc.mut)
			ws.Close()
			if !strings.Contains(refusal, tc.want) {
				t.Errorf("refusal = %q, want something about %q", refusal, tc.want)
			}
		})
	}
}

// A token is worth one machine. Vast publishes instance logs to an unauthenticated
// bucket, so one that survived its first use would be an open invitation.
func TestEnrolmentTokenIsSingleUse(t *testing.T) {
	srv, _ := testHub(t)
	defer srv.Close()
	alice, first, second := newParty(t), newParty(t), newParty(t)

	tok := alice.token(time.Now().Add(5*time.Minute), "shared-nonce")
	ws, refusal := dial(t, srv, "/relay/agent", first, "agent", func(f *Frame) { f.Enrol = tok })
	if refusal != "" {
		t.Fatalf("first use refused: %s", refusal)
	}
	defer ws.Close()

	ws2, refusal := dial(t, srv, "/relay/agent", second, "agent", func(f *Frame) { f.Enrol = tok })
	ws2.Close()
	if !strings.Contains(refusal, "already been used") {
		t.Errorf("a token enrolled a second machine: %q", refusal)
	}
}

// A machine belongs to whoever enrolled it. Somebody else presenting their own valid
// token for a key they do not hold must not be able to take it over.
func TestMachineCannotBeReassigned(t *testing.T) {
	st, err := OpenStore(t.TempDir() + "/m.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := st.Enrol("mach", "alice", "box", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enrol("mach", "bob", "box", now); err == nil {
		t.Error("a machine was reassigned to another owner")
	}
	if m, _ := st.Get("mach"); m.Owner != "alice" {
		t.Errorf("owner is now %q", m.Owner)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	path := t.TempDir() + "/m.json"
	st, _ := OpenStore(path)
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := st.Enrol("k1", "owner", "one", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enrol("k2", "owner", "two", now); err != nil {
		t.Fatal(err)
	}
	if err := st.Forget("k1", "wrong-owner"); err == nil {
		t.Error("forgot a machine for the wrong owner")
	}
	if err := st.Forget("k1", "owner"); err != nil {
		t.Fatal(err)
	}
	again, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Count() != 1 {
		t.Fatalf("after reopen: %d machines", again.Count())
	}
	if got := again.Owned("owner"); len(got) != 1 || got[0].Name != "two" {
		t.Errorf("owned = %+v", got)
	}
}

func TestHealthReportsCounts(t *testing.T) {
	srv, _ := testHub(t)
	defer srv.Close()
	res, err := http.Get(srv.URL + "/relay/health")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"agents_online", "owners_online", "machines_enrolled"} {
		if _, ok := out[k]; !ok {
			t.Errorf("health is missing %s", k)
		}
	}
}
