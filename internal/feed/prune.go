package feed

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RetentionPolicy governs how long verbatim snapshots are kept.
//
// The archive exists so a parser or metrics change can be replayed rather than
// costing history. That value decays with age: recent snapshots may need replaying
// at full resolution, whereas year-old data is only ever read at daily granularity.
// So keep everything recent, then thin to one snapshot per day forever.
type RetentionPolicy struct {
	// FullResolution is how far back every snapshot is kept. Zero means keep all.
	FullResolution time.Duration
	// KeepDailyAfter thins older snapshots to the first of each UTC day. When
	// false, snapshots past FullResolution are deleted outright.
	KeepDailyAfter bool
}

// DefaultRetention keeps 90 days at full hourly resolution, then one snapshot per
// day indefinitely. At the measured ~32.6 MB per snapshot pair and hourly
// publication that is roughly 67 GB for the rolling window plus ~12 GB/year of
// long-term archive — versus ~272 GB/year unthinned.
var DefaultRetention = RetentionPolicy{
	FullResolution: 90 * 24 * time.Hour,
	KeepDailyAfter: true,
}

// PruneResult reports what a prune did.
type PruneResult struct {
	Kept    int
	Deleted int
	Freed   int64
}

// Prune applies the policy to the archive, deleting snapshots that are no longer
// required. now is passed explicitly so the caller controls the clock in tests.
//
// Deletion is per-feed: the team and user feeds publish a minute apart and are thus
// thinned independently, which keeps their day-boundary choices consistent even
// though their timestamps differ.
func (a *Archive) Prune(p RetentionPolicy, now time.Time) (PruneResult, error) {
	var res PruneResult
	if p.FullResolution == 0 {
		return res, nil
	}
	cutoff := now.UTC().Add(-p.FullResolution)

	for _, k := range All() {
		snaps, err := a.List(k)
		if err != nil {
			return res, err
		}
		keptDay := map[string]bool{}
		for _, s := range snaps {
			at := s.Meta.SnapshotAt.UTC()
			if !at.Before(cutoff) {
				res.Kept++
				continue
			}
			// Older than the full-resolution window: keep the first snapshot of
			// each UTC day, drop the rest. Snapshots are listed oldest-first, so
			// "first seen for this day" is deterministic.
			day := at.Format("2006-01-02")
			if p.KeepDailyAfter && !keptDay[day] {
				keptDay[day] = true
				res.Kept++
				continue
			}
			n, err := removeSnapshot(s)
			if err != nil {
				return res, err
			}
			res.Deleted++
			res.Freed += n
		}
	}
	return res, nil
}

// removeSnapshot deletes a payload and its sidecar, returning bytes freed.
//
// The payload goes first, which is what the reasoning here always said and the
// opposite of what the code did. A payload without its sidecar is invisible to List,
// so nothing — not ingest, not a later prune — can ever see or reclaim it: an
// interruption between the two removals leaked disk permanently. A sidecar without a
// payload is merely a broken entry: it stays visible, the next prune removes it, and
// os.IsNotExist makes that second attempt a no-op. One direction is self-healing and
// the other is not.
func removeSnapshot(s Snapshot) (int64, error) {
	var freed int64
	if fi, err := os.Stat(s.Path); err == nil {
		freed = fi.Size()
	}
	if err := os.Remove(s.Path); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	metaPath := strings.TrimSuffix(s.Path, payloadExt) + metaExt
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		return freed, err
	}
	return freed, nil
}

// PruneEmptyDirs removes date directories left empty by pruning, so the archive
// does not accumulate an unbounded tree of empty folders.
func (a *Archive) PruneEmptyDirs() error {
	var dirs []string
	err := filepath.Walk(a.Root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if fi.IsDir() && path != a.Root {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Deepest first, so emptying a day directory lets its month be removed too.
	for i := len(dirs) - 1; i >= 0; i-- {
		if entries, err := os.ReadDir(dirs[i]); err == nil && len(entries) == 0 {
			os.Remove(dirs[i])
		}
	}
	return nil
}
