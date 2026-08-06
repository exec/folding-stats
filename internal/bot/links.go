package bot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Links remembers which donor name a Discord account belongs to.
//
// This is the one thing a bot can offer that the API and the website cannot: the site
// has no accounts by design, so it can never know who is asking. Discord already does,
// and binding that identity once turns every subsequent question into `/me`.
//
// A JSON file rather than a database. The whole record is one short string per user,
// it is read far more often than written, and a bot that needs a schema migration to
// remember a name has been overbuilt.
type Links struct {
	path string

	mu sync.RWMutex
	m  map[string]string // discord user id -> donor name
}

func OpenLinks(path string) (*Links, error) {
	l := &Links{path: path, m: map[string]string{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return l, nil
	}
	if err := json.Unmarshal(b, &l.m); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return l, nil
}

func (l *Links) Get(userID string) (string, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	n, ok := l.m[userID]
	return n, ok
}

func (l *Links) Set(userID, donor string) error {
	l.mu.Lock()
	l.m[userID] = donor
	err := l.saveLocked()
	l.mu.Unlock()
	return err
}

func (l *Links) Delete(userID string) error {
	l.mu.Lock()
	delete(l.m, userID)
	err := l.saveLocked()
	l.mu.Unlock()
	return err
}

func (l *Links) Count() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.m)
}

// saveLocked writes through a temporary file and renames.
//
// Writing in place would mean a crash mid-write leaves a truncated file, and the next
// start would find every binding gone — the one piece of state here that users would
// have to re-enter by hand. Rename is atomic on the same filesystem.
func (l *Links) saveLocked() error {
	b, err := json.MarshalIndent(l.m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}
