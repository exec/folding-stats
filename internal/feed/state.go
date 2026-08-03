package feed

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// State persists cache validators between runs so a restart doesn't re-download
// feeds that haven't changed. It is a cache, not a source of truth: losing it costs
// one redundant fetch per feed, and Archive.Has still prevents a duplicate write.
type State struct {
	mu    sync.Mutex
	path  string
	Feeds map[Kind]Validator `json:"feeds"`
}

// LoadState reads state from path, returning an empty State if it does not exist.
func LoadState(path string) (*State, error) {
	s := &State{path: path, Feeds: map[Kind]Validator{}}
	err := readJSON(path, s)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		// Corrupt state is recoverable — discard it and re-fetch rather than
		// refusing to start.
		s.Feeds = map[Kind]Validator{}
	}
	if s.Feeds == nil {
		s.Feeds = map[Kind]Validator{}
	}
	return s, nil
}

func (s *State) Get(k Kind) Validator {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Feeds[k]
}

func (s *State) Set(k Kind, v Validator) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Feeds[k] = v
}

// Save writes state atomically.
func (s *State) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := writeJSON(tmp, s); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
