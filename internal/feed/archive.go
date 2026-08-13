package feed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Archive is an on-disk store of verbatim feed snapshots.
//
// Layout, keyed by upstream publish time:
//
//	<root>/2026/08/02/20260802T213006Z-user.txt.zst
//	<root>/2026/08/02/20260802T213006Z-user.json
//
// Date-sharded so no directory grows unbounded, and sortable lexicographically so
// listing a range is a directory walk rather than an index lookup.
type Archive struct {
	Root string
}

// Snapshot is one archived payload plus its metadata.
type Snapshot struct {
	Meta Meta
	Path string // path to the compressed payload
}

const (
	stampLayout = "20060102T150405Z"
	payloadExt  = ".txt.zst"
	metaExt     = ".json"
)

// Store fetches k conditionally and, if it changed, writes it to the archive.
// Returns a Result with NotModified set when there was nothing new.
//
// Writes go to a temporary file and are renamed into place only after a successful
// fsync, so an interrupted run can leave a stray .tmp but never a truncated snapshot
// that later looks complete.
func (a *Archive) Store(ctx context.Context, f *Fetcher, k Kind, prev Validator) (Result, error) {
	tmp, err := os.CreateTemp(a.Root, ".partial-*")
	if err != nil {
		if err := os.MkdirAll(a.Root, 0o755); err != nil {
			return Result{}, err
		}
		if tmp, err = os.CreateTemp(a.Root, ".partial-*"); err != nil {
			return Result{}, err
		}
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once renamed away
	}()

	// zstd over gzip: ~15% smaller and several times faster to decompress, which
	// matters because replaying the archive is a routine operation, not a rare one.
	gz, err := zstd.NewWriter(tmp, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	if err != nil {
		return Result{}, err
	}

	res, err := f.Fetch(ctx, k, prev, gz)
	if err != nil {
		return Result{}, err
	}
	if res.NotModified {
		return res, nil
	}
	if err := gz.Close(); err != nil {
		return Result{}, fmt.Errorf("feed %s: closing compressor: %w", k, err)
	}
	if err := tmp.Sync(); err != nil {
		return Result{}, fmt.Errorf("feed %s: syncing payload: %w", k, err)
	}

	st, err := tmp.Stat()
	if err != nil {
		return Result{}, err
	}
	res.Meta.StoredBytes = st.Size()

	dir := a.dir(res.Meta.SnapshotAt)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, err
	}
	base := a.base(k, res.Meta.SnapshotAt)

	if err := tmp.Close(); err != nil {
		return Result{}, err
	}
	payloadPath := filepath.Join(dir, base+payloadExt)
	if err := os.Link(tmpName, payloadPath); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return Result{}, err
		}
		var existing Meta
		if err := readJSON(filepath.Join(dir, base+metaExt), &existing); err != nil {
			return Result{}, fmt.Errorf("feed %s: snapshot collision without readable metadata: %w", k, err)
		}
		if existing.SHA256 != res.Meta.SHA256 {
			return Result{}, fmt.Errorf("feed %s: conflicting snapshot already exists at %s", k, res.Meta.SnapshotAt.Format(time.RFC3339))
		}
		return Result{Feed: k, NotModified: true, Meta: existing}, nil
	}
	if err := writeJSON(filepath.Join(dir, base+metaExt), res.Meta); err != nil {
		_ = os.Remove(payloadPath)
		return Result{}, err
	}
	return res, nil
}

// Has reports whether a snapshot for this feed and publish time is already stored.
// Guards against re-archiving when validators are lost — a fresh checkout, or an
// upstream that stops sending ETags.
func (a *Archive) Has(k Kind, at time.Time) bool {
	_, err := os.Stat(filepath.Join(a.dir(at), a.base(k, at)+payloadExt))
	return err == nil
}

// List returns every archived snapshot of kind k, oldest first. Passing an empty
// Kind lists all feeds.
//
// Prefer ListSince in the hot path: the archive grows by ~17k snapshots a year, and
// listing all of them means parsing that many sidecar files.
func (a *Archive) List(k Kind) ([]Snapshot, error) {
	return a.ListSince(k, time.Time{})
}

// ListSince returns archived snapshots of kind k published at or after since,
// oldest first.
//
// The date-sharded layout is what makes this cheap: whole year, month and day
// directories that fall entirely before the cutoff are skipped without being opened,
// so an hourly ingest reads a handful of sidecars rather than every one ever written.
func (a *Archive) ListSince(k Kind, since time.Time) ([]Snapshot, error) {
	since = since.UTC()
	var out []Snapshot
	err := filepath.WalkDir(a.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(a.Root, path, since) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, metaExt) {
			return nil
		}
		var m Meta
		if err := readJSON(path, &m); err != nil {
			// A single unreadable sidecar shouldn't hide the rest of the archive.
			return nil
		}
		if k != "" && m.Feed != k {
			return nil
		}
		if m.SnapshotAt.Before(since) {
			return nil
		}
		out = append(out, Snapshot{
			Meta: m,
			Path: strings.TrimSuffix(path, metaExt) + payloadExt,
		})
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Meta.SnapshotAt.Equal(out[j].Meta.SnapshotAt) {
			return out[i].Meta.Feed < out[j].Meta.Feed
		}
		return out[i].Meta.SnapshotAt.Before(out[j].Meta.SnapshotAt)
	})
	return out, nil
}

// Latest returns the most recent snapshot of kind k, or false if none is stored.
func (a *Archive) Latest(k Kind) (Snapshot, bool, error) {
	all, err := a.List(k)
	if err != nil || len(all) == 0 {
		return Snapshot{}, false, err
	}
	return all[len(all)-1], true, nil
}

// Open returns the decompressed payload. The caller closes it.
//
// This is the replay entry point: the ingest pipeline consumes snapshots through
// Open whether they arrived seconds ago or are being re-read from months back, so
// live ingest and historical replay share one code path.
func (s Snapshot) Open() (io.ReadCloser, error) {
	f, err := os.Open(s.Path)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &zstdReadCloser{dec: dec, f: f, meta: s.Meta, digest: sha256.New()}, nil
}

// skipDir reports whether a date-shard directory lies entirely before since.
// Directories are named by year, month and day, so a prefix comparison suffices.
func skipDir(root, path string, since time.Time) bool {
	if since.IsZero() {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	switch len(parts) {
	case 1:
		return parts[0] < since.Format("2006")
	case 2:
		return parts[0]+parts[1] < since.Format("200601")
	case 3:
		return parts[0]+parts[1]+parts[2] < since.Format("20060102")
	}
	return false
}

func (a *Archive) dir(at time.Time) string {
	at = at.UTC()
	return filepath.Join(a.Root, at.Format("2006"), at.Format("01"), at.Format("02"))
}

func (a *Archive) base(k Kind, at time.Time) string {
	return at.UTC().Format(stampLayout) + "-" + string(k)
}

type zstdReadCloser struct {
	dec    *zstd.Decoder
	f      *os.File
	meta   Meta
	digest hash.Hash
	n      int64
	err    error
}

func (z *zstdReadCloser) Read(p []byte) (int, error) {
	if z.err != nil {
		return 0, z.err
	}
	remaining := maxDecodedBytes - z.n
	if remaining < 0 {
		z.err = fmt.Errorf("archive payload exceeds %d decoded bytes", maxDecodedBytes)
		return 0, z.err
	}
	if int64(len(p)) > remaining+1 {
		p = p[:remaining+1]
	}
	n, err := z.dec.Read(p)
	if z.n+int64(n) > maxDecodedBytes {
		z.err = fmt.Errorf("archive payload exceeds %d decoded bytes", maxDecodedBytes)
		return 0, z.err
	}
	z.n += int64(n)
	_, _ = z.digest.Write(p[:n])
	if err == io.EOF {
		if z.meta.Bytes > 0 && z.n != z.meta.Bytes {
			z.err = fmt.Errorf("archive payload is %d bytes, metadata says %d", z.n, z.meta.Bytes)
			return n, z.err
		}
		if z.meta.SHA256 != "" && hex.EncodeToString(z.digest.Sum(nil)) != z.meta.SHA256 {
			z.err = fmt.Errorf("archive payload checksum does not match metadata")
			return n, z.err
		}
	}
	return n, err
}
func (z *zstdReadCloser) Close() error {
	z.dec.Close() // returns nothing; releases decoder goroutines
	return z.f.Close()
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Put stores a payload directly, bypassing the network.
//
// Used for tests and for importing an archive captured elsewhere. The snapshot is
// keyed by at, exactly as a fetched one would be, so imported and fetched snapshots
// are indistinguishable downstream.
func (a *Archive) Put(k Kind, at time.Time, r io.Reader) (Meta, error) {
	dir := a.dir(at)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Meta{}, err
	}
	base := a.base(k, at)

	f, err := os.CreateTemp(dir, ".partial-*")
	if err != nil {
		return Meta{}, err
	}
	tmpName := f.Name()
	defer func() { f.Close(); os.Remove(tmpName) }()

	enc, err := zstd.NewWriter(f, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	if err != nil {
		return Meta{}, err
	}
	digest := sha256.New()
	counter := &countingWriter{}
	n, err := io.Copy(io.MultiWriter(enc, digest, counter), r)
	if err != nil {
		return Meta{}, err
	}
	if err := enc.Close(); err != nil {
		return Meta{}, err
	}
	if err := f.Sync(); err != nil {
		return Meta{}, err
	}
	st, err := f.Stat()
	if err != nil {
		return Meta{}, err
	}
	meta := Meta{
		Feed: k, URL: k.URL(),
		FetchedAt: at.UTC(), SnapshotAt: at.UTC(),
		Bytes: n, StoredBytes: st.Size(),
		SHA256: hex.EncodeToString(digest.Sum(nil)),
	}
	if err := f.Close(); err != nil {
		return Meta{}, err
	}
	payloadPath := filepath.Join(dir, base+payloadExt)
	metaPath := filepath.Join(dir, base+metaExt)
	if err := os.Link(tmpName, payloadPath); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return Meta{}, err
		}
		var existing Meta
		if err := readJSON(metaPath, &existing); err != nil {
			return Meta{}, fmt.Errorf("feed %s: snapshot collision without readable metadata: %w", k, err)
		}
		if existing.SHA256 != meta.SHA256 {
			return Meta{}, fmt.Errorf("feed %s: conflicting snapshot already exists at %s", k, at.UTC().Format(time.RFC3339))
		}
		return existing, nil
	}
	if err := writeJSON(metaPath, meta); err != nil {
		_ = os.Remove(payloadPath)
		return Meta{}, err
	}
	return meta, nil
}
