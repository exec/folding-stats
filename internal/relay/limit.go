package relay

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Rate limiting, of the little that is worth limiting.
//
// The obvious design — a cap on connections per address — is the wrong one here, and
// would break the case this relay exists for. A fleet of two hundred machines in one
// building arrives from one address, and after a relay restart they all reconnect at
// once. A per-address connection cap would refuse most of a legitimate fleet at
// precisely the moment it was trying to come back.
//
// What is worth bounding is work that nobody has paid for with a signature:
//
//   - A connection that never authenticates costs a goroutine and a read deadline.
//     Opening thousands is free for the other end and not for us, so the number of
//     handshakes in flight at once is capped globally, whatever address they come
//     from.
//
//   - A connection that authenticates *badly* is somebody guessing, and that is worth
//     slowing down per address. A correct authentication costs nothing, so a fleet
//     reconnecting is never touched by it however large it is.
//
// Both numbers are deliberately far above anything real use produces. This is a fence
// around the cliff, not a turnstile.
var (
	// MaxHandshakes is how many unauthenticated connections may be in progress.
	MaxHandshakes = 256
	// FailWindow and MaxFailures bound bad authentications from one address.
	FailWindow  = time.Minute
	MaxFailures = 20
	// Active connections remain charged for their full lifetime. Owner keys are free
	// to mint, so a valid signature is authentication, not a resource quota.
	MaxActiveConnections = 4096
	MaxActivePerAddress  = 512
	MaxActiveOwners      = 1024
)

// realIP is the address to hold responsible.
//
// Behind the DMZ proxy the connection always comes from nginx, so RemoteAddr is
// useless and X-Real-IP is what nginx resolved — which, with the Cloudflare real-ip
// snippet in front of it, is the actual client. Through a Cloudflare tunnel there is
// no nginx and RemoteAddr is cloudflared on loopback; the edge sets CF-Connecting-IP
// instead, and without reading it every client on earth would share one failure
// bucket and twenty bad handshakes would lock out the world. Both headers are only
// trustworthy because this listener is on an internal interface and the proxy or
// tunnel is the only route to it; neither is something to believe on a public socket.
func realIP(r *http.Request) string {
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return v
	}
	if v := r.Header.Get("CF-Connecting-IP"); v != "" {
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limiter tracks failed authentications and handshakes in flight.
type limiter struct {
	mu       sync.Mutex
	failures map[string]*failCount
	active   map[string]int
	inFlight int
	activeN  int
	ownersN  int
}

type failCount struct {
	n     int
	since time.Time
}

func newLimiter() *limiter {
	return &limiter{failures: map[string]*failCount{}, active: map[string]int{}}
}

func (l *limiter) admit(ip string, agent bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.activeN >= MaxActiveConnections || l.active[ip] >= MaxActivePerAddress || (!agent && l.ownersN >= MaxActiveOwners) {
		return false
	}
	l.activeN++
	l.active[ip]++
	if !agent {
		l.ownersN++
	}
	return true
}

func (l *limiter) release(ip string, agent bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[ip] > 1 {
		l.active[ip]--
	} else {
		delete(l.active, ip)
	}
	if l.activeN > 0 {
		l.activeN--
	}
	if !agent && l.ownersN > 0 {
		l.ownersN--
	}
}

// begin reserves a handshake slot, reporting whether there was one.
func (l *limiter) begin() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight >= MaxHandshakes {
		return false
	}
	l.inFlight++
	return true
}

func (l *limiter) done() {
	l.mu.Lock()
	if l.inFlight > 0 {
		l.inFlight--
	}
	l.mu.Unlock()
}

// blocked reports whether an address has failed too often lately.
func (l *limiter) blocked(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.failures[ip]
	if !ok {
		return false
	}
	if now.Sub(f.since) > FailWindow {
		delete(l.failures, ip)
		return false
	}
	return f.n >= MaxFailures
}

// failed records a rejected authentication.
func (l *limiter) failed(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Sweep on write, which is rare — this map only ever holds addresses that have
	// recently failed, and an address that stops failing disappears from it.
	for k, v := range l.failures {
		if now.Sub(v.since) > FailWindow {
			delete(l.failures, k)
		}
	}

	f, ok := l.failures[ip]
	if !ok || now.Sub(f.since) > FailWindow {
		l.failures[ip] = &failCount{n: 1, since: now}
		return
	}
	f.n++
}

// succeeded clears an address's record.
//
// A machine that mistypes its way through a few attempts and then gets it right is not
// what this is for, and leaving the count standing would punish it on its next
// reconnection for something already resolved.
func (l *limiter) succeeded(ip string) {
	l.mu.Lock()
	delete(l.failures, ip)
	l.mu.Unlock()
}

func (l *limiter) stats() (inFlight, watched int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inFlight, len(l.failures)
}
