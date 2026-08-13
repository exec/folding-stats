package relay

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Machine is one enrolled folding client.
//
// Note what is absent: no address, no folding data, no credential. The relay knows
// that a key belongs to an owner and when it last spoke. Everything else it merely
// passes through, and could not read even if it wanted to once the frames are sealed.
type Machine struct {
	// Key is the machine's ed25519 public key, and its identity. A machine that
	// regenerates its key is a new machine, which is the correct behaviour: whoever
	// holds the old private key should not inherit the new box.
	Key      string    `json:"key"`
	Owner    string    `json:"owner"`
	Name     string    `json:"name,omitempty"`
	Enrolled time.Time `json:"enrolled"`
	LastSeen time.Time `json:"last_seen,omitzero"`
}

// Store is the enrolment register.
//
// A JSON file, rewritten whole, in the manner of everything else that persists here.
// It is read on every connection and written only when a machine enrols or is
// forgotten, so the cost is a few hundred bytes an hour on a busy fleet.
type Store struct {
	path string

	mu     sync.RWMutex
	m      map[string]*Machine
	nonces map[string]int64
	recent []time.Time
}

type storeFile struct {
	Machines []*Machine       `json:"machines"`
	Nonces   map[string]int64 `json:"spent_nonces,omitempty"`
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, m: map[string]*Machine{}, nonces: map[string]int64{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) || (err == nil && len(b) == 0) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var data storeFile
	if err := json.Unmarshal(b, &data); err != nil {
		// Backward compatibility with the original top-level machine list.
		if err := json.Unmarshal(b, &data.Machines); err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
	}
	for _, m := range data.Machines {
		s.m[m.Key] = m
	}
	if data.Nonces != nil {
		s.nonces = data.Nonces
	}
	return s, nil
}

// MaxPerOwner caps a fleet.
//
// Not a resource limit — a machine costs a socket and ten bytes a second. It bounds
// what one leaked enrolment token can do before anybody notices.
const MaxPerOwner = 500

var (
	MaxMachines            = 10_000
	MaxNameBytes           = 128
	MaxNonceBytes          = 64
	MaxSpentNonces         = 20_000
	MaxEnrolmentsPerMinute = 120
)

// Get returns a copy.
//
// Handing out the pointer let callers read fields outside the lock while Touch wrote
// LastSeen under it — a race the detector found the moment a test connected sixty
// machines at once. The struct is small and read far more often than written, so a
// copy costs nothing worth measuring and removes the whole class of mistake.
func (s *Store) Get(key string) (Machine, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.m[key]
	if !ok {
		return Machine{}, false
	}
	return *m, true
}

// Enrol records a machine against an owner, or confirms it already is.
func (s *Store) Enrol(key, owner, name string, now time.Time) (Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enrolLocked(key, owner, name, now)
}

// EnrolToken consumes an owner-signed token and records the machine in one durable
// store update, so a restart or later machine revocation cannot make the token fresh.
func (s *Store) EnrolToken(key, owner, name, nonce string, exp int64, now time.Time) (Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(nonce) == 0 || len(nonce) > MaxNonceBytes {
		return Machine{}, fmt.Errorf("enrolment nonce is %d bytes, limit is %d", len(nonce), MaxNonceBytes)
	}
	for n, expiry := range s.nonces {
		if expiry <= now.Unix() {
			delete(s.nonces, n)
		}
	}
	if _, used := s.nonces[nonce]; used {
		return Machine{}, fmt.Errorf("enrolment token has already been used")
	}
	if len(s.nonces) >= MaxSpentNonces {
		return Machine{}, fmt.Errorf("relay already has %d live enrolment nonces, which is the limit", MaxSpentNonces)
	}
	cutoff := now.Add(-time.Minute)
	keep := 0
	for _, at := range s.recent {
		if at.After(cutoff) {
			s.recent[keep] = at
			keep++
		}
	}
	s.recent = s.recent[:keep]
	if len(s.recent) >= MaxEnrolmentsPerMinute {
		return Machine{}, fmt.Errorf("relay enrolment rate limit reached")
	}
	s.nonces[nonce] = exp
	m, err := s.enrolLocked(key, owner, name, now)
	if err != nil {
		delete(s.nonces, nonce)
	} else {
		s.recent = append(s.recent, now)
	}
	return m, err
}

func (s *Store) enrolLocked(key, owner, name string, now time.Time) (Machine, error) {
	if len(name) > MaxNameBytes {
		return Machine{}, fmt.Errorf("machine name is %d bytes, limit is %d", len(name), MaxNameBytes)
	}

	if m, ok := s.m[key]; ok {
		// Re-enrolling an existing machine must not silently move it between owners:
		// that would let anyone who can reach the relay reassign a box by presenting
		// their own token for a key they do not control. They cannot sign as the
		// machine, so this is defence in depth rather than the only guard.
		if m.Owner != owner {
			return Machine{}, fmt.Errorf("machine already belongs to another owner")
		}
		if name != "" {
			m.Name = name
		}
		return *m, s.saveLocked()
	}
	if len(s.m) >= MaxMachines {
		return Machine{}, fmt.Errorf("relay already has %d machines, which is the limit", MaxMachines)
	}

	var count int
	for _, m := range s.m {
		if m.Owner == owner {
			count++
		}
	}
	if count >= MaxPerOwner {
		return Machine{}, fmt.Errorf("owner already has %d machines, which is the limit", MaxPerOwner)
	}

	m := &Machine{Key: key, Owner: owner, Name: name, Enrolled: now}
	s.m[key] = m
	return *m, s.saveLocked()
}

// Forget removes a machine, which is how a compromised one is revoked.
func (s *Store) Forget(key, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.m[key]
	if !ok {
		return fmt.Errorf("no such machine")
	}
	if m.Owner != owner {
		return fmt.Errorf("not your machine")
	}
	delete(s.m, key)
	return s.saveLocked()
}

func (s *Store) Owned(owner string) []Machine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Machine
	for _, m := range s.m {
		if m.Owner == owner {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Enrolled.Before(out[j].Enrolled) })
	return out
}

func (s *Store) Touch(key string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.m[key]; ok {
		m.LastSeen = now
	}
}

func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.m)
}

func (s *Store) saveLocked() error {
	list := make([]*Machine, 0, len(s.m))
	for _, m := range s.m {
		list = append(list, m)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
	b, err := json.MarshalIndent(storeFile{Machines: list, Nonces: s.nonces}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
