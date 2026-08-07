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

	mu sync.RWMutex
	m  map[string]*Machine
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, m: map[string]*Machine{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) || (err == nil && len(b) == 0) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*Machine
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	for _, m := range list {
		s.m[m.Key] = m
	}
	return s, nil
}

// MaxPerOwner caps a fleet.
//
// Not a resource limit — a machine costs a socket and ten bytes a second. It bounds
// what one leaked enrolment token can do before anybody notices.
const MaxPerOwner = 500

func (s *Store) Get(key string) (*Machine, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.m[key]
	return m, ok
}

// Enrol records a machine against an owner, or confirms it already is.
func (s *Store) Enrol(key, owner, name string, now time.Time) (*Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if m, ok := s.m[key]; ok {
		// Re-enrolling an existing machine must not silently move it between owners:
		// that would let anyone who can reach the relay reassign a box by presenting
		// their own token for a key they do not control. They cannot sign as the
		// machine, so this is defence in depth rather than the only guard.
		if m.Owner != owner {
			return nil, fmt.Errorf("machine already belongs to another owner")
		}
		if name != "" {
			m.Name = name
		}
		return m, s.saveLocked()
	}

	var count int
	for _, m := range s.m {
		if m.Owner == owner {
			count++
		}
	}
	if count >= MaxPerOwner {
		return nil, fmt.Errorf("owner already has %d machines, which is the limit", MaxPerOwner)
	}

	m := &Machine{Key: key, Owner: owner, Name: name, Enrolled: now}
	s.m[key] = m
	return m, s.saveLocked()
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

func (s *Store) Owned(owner string) []*Machine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Machine
	for _, m := range s.m {
		if m.Owner == owner {
			out = append(out, m)
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
	b, err := json.MarshalIndent(list, "", "  ")
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
