package feed

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"time"
)

// Archiver polls the upstream feeds and archives anything new.
//
// It is deliberately the whole of the collection layer: the eventual server runs an
// Archiver in-process and hangs ingest off the snapshots it produces, so there is
// only ever one implementation of "what does upstream have, and have we stored it".
type Archiver struct {
	Archive *Archive
	Fetcher *Fetcher
	State   *State
	Log     *slog.Logger

	// OnSnapshot, if set, is called after a snapshot is durably archived. The
	// server uses this to trigger ingest; the standalone archiver leaves it nil.
	// It runs synchronously, so a slow handler delays the next feed.
	OnSnapshot func(Snapshot)

	// Delay, if set, chooses how long to wait before each poll, given the current
	// time. It lets the caller schedule polls around a predicted publish instead of
	// spreading the capture lag uniformly across a fixed interval. Nil means the
	// fixed interval passed to Run.
	Delay func(now time.Time) time.Duration

	// quietUntil is set when upstream asks us to back off, and overrides any
	// schedule until it passes.
	quietUntil time.Time
}

// New builds an Archiver rooted at dir, storing state alongside the archive.
func New(dir, userAgent string, log *slog.Logger) (*Archiver, error) {
	st, err := LoadState(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	return &Archiver{
		Archive: &Archive{Root: filepath.Join(dir, "raw")},
		Fetcher: NewFetcher(userAgent),
		State:   st,
		Log:     log,
	}, nil
}

// Once polls every feed a single time. Errors on individual feeds are logged and
// do not abort the pass — upstream outages are routine, and a failure on the user
// feed shouldn't cost us the team feed.
func (a *Archiver) Once(ctx context.Context) (stored int) {
	for _, k := range All() {
		select {
		case <-ctx.Done():
			return stored
		default:
		}
		if a.fetch(ctx, k) {
			stored++
		}
	}
	if err := a.State.Save(); err != nil {
		a.Log.Warn("saving state", "err", err)
	}
	return stored
}

func (a *Archiver) fetch(ctx context.Context, k Kind) bool {
	start := time.Now()
	res, err := a.Archive.Store(ctx, a.Fetcher, k, a.State.Get(k))
	if err != nil {
		var backoff *BackoffError
		if errors.As(err, &backoff) {
			a.quietUntil = time.Now().Add(backoff.RetryAfter)
			a.Log.Warn("backing off at upstream's request",
				"feed", k, "status", backoff.Status,
				"retry_after", backoff.RetryAfter,
				"quiet_until", a.quietUntil.UTC().Format(time.RFC3339))
			return false
		}
		a.Log.Error("fetch failed", "feed", k, "err", err)
		return false
	}
	if res.NotModified {
		a.Log.Debug("unchanged", "feed", k)
		return false
	}

	a.State.Set(k, Validator{ETag: res.Meta.ETag, LastModified: res.Meta.LastModified})
	a.Log.Info("archived",
		"feed", k,
		"snapshot_at", res.Meta.SnapshotAt.Format(time.RFC3339),
		"feed_timestamp", res.Meta.FeedTimestamp,
		"wire_bytes", res.Meta.WireBytes,
		"bytes", res.Meta.Bytes,
		"stored", res.Meta.StoredBytes,
		"ratio", ratio(res.Meta.Bytes, res.Meta.StoredBytes),
		"took", time.Since(start).Round(time.Millisecond),
	)

	if a.OnSnapshot != nil {
		dir := a.Archive.dir(res.Meta.SnapshotAt)
		base := a.Archive.base(k, res.Meta.SnapshotAt)
		a.OnSnapshot(Snapshot{Meta: res.Meta, Path: filepath.Join(dir, base+payloadExt)})
	}
	return true
}

// Run polls until ctx is cancelled. It polls immediately on start rather than
// waiting out the first interval, so a restart doesn't open a gap in the archive.
//
// Poll interval is decoupled from the publish cadence on purpose: conditional GETs
// make an unchanged feed almost free, so polling more often than upstream publishes
// costs little and shortens the window between publish and capture.
//
// If Delay is set it chooses each wait instead, which lets a caller that knows when
// the next publish is due poll slowly while nothing is expected and quickly once it
// is. Without it, the interval is used uniformly.
func (a *Archiver) Run(ctx context.Context, interval time.Duration) error {
	a.Log.Info("archiver started", "interval", interval, "root", a.Archive.Root)
	a.Once(ctx)

	t := time.NewTimer(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			a.Log.Info("archiver stopped")
			return ctx.Err()
		case <-t.C:
			a.Once(ctx)
			t.Reset(a.nextDelay(interval))
		}
	}
}

// nextDelay is how long to wait before the next poll, clamped so a misbehaving Delay
// can neither hammer upstream nor stall the archiver indefinitely.
func (a *Archiver) nextDelay(interval time.Duration) time.Duration {
	// A backoff request outranks every other consideration, including a publish we
	// are expecting imminently. Missing a cycle is recoverable; ignoring upstream
	// telling us to stop is not the sort of thing you get asked twice.
	if wait := time.Until(a.quietUntil); wait > 0 {
		return wait
	}
	if a.Delay == nil {
		return interval
	}
	d := a.Delay(time.Now().UTC())
	if d < minPollDelay {
		return minPollDelay
	}
	if d > maxPollDelay {
		return maxPollDelay
	}
	return d
}

const (
	minPollDelay = 15 * time.Second
	maxPollDelay = time.Hour
)

func ratio(raw, stored int64) string {
	if stored == 0 {
		return "-"
	}
	r := float64(raw) / float64(stored)
	return formatFloat(r) + "x"
}

func formatFloat(f float64) string {
	i := int(f*10 + 0.5)
	return itoa(i/10) + "." + itoa(i%10)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
