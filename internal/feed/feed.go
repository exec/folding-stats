// Package feed fetches and archives the upstream Folding@home summary feeds.
//
// The feeds are cumulative lifetime snapshots: they carry only score and work-unit
// totals, so every rate, rank and delta the site displays must be derived by
// differencing successive snapshots. That makes the archive the one asset we cannot
// backfill — history accrues in real time and a gap is permanent.
//
// Archived snapshots are stored verbatim (gzipped, never parsed) so that a parser or
// metrics change costs a replay rather than the history itself. Both the live ingest
// loop and the replay tool read snapshots through this package.
package feed

import "time"

// Kind identifies one of the two upstream summary feeds.
type Kind string

const (
	Teams Kind = "team"
	Users Kind = "user"

	// Hard ceilings sit several times above the observed feeds while keeping a
	// compromised origin or imported archive from consuming the host without bound.
	MaxWireBytes    int64 = 128 << 20
	MaxDecodedBytes int64 = 256 << 20
	MaxTeamRows           = 500_000
	MaxUserRows           = 5_000_000
)

// Kept separate from the public policy constants so boundary tests can exercise the
// streaming paths with small payloads instead of allocating hundreds of megabytes.
var (
	maxWireBytes    = MaxWireBytes
	maxDecodedBytes = MaxDecodedBytes
)

// All returns every feed kind, in the order they should be fetched. Teams is
// fetched first because it is ~17x smaller, so a failure surfaces cheaply.
func All() []Kind { return []Kind{Teams, Users} }

// URL is the upstream location of the feed.
func (k Kind) URL() string {
	switch k {
	case Teams:
		return "https://apps.foldingathome.org/daily_team_summary.txt"
	case Users:
		return "https://apps.foldingathome.org/daily_user_summary.txt"
	}
	return ""
}

func (k Kind) String() string { return string(k) }

// Valid reports whether k is a known feed.
func (k Kind) Valid() bool { return k.URL() != "" }

// Meta describes a single archived snapshot. It is written alongside the payload as
// JSON so the archive is self-describing without decompressing anything.
type Meta struct {
	Feed Kind   `json:"feed"`
	URL  string `json:"url"`

	// FetchedAt is when we retrieved it; SnapshotAt is when upstream published it.
	// They differ by however long it took us to notice, so SnapshotAt is the one
	// that identifies the snapshot.
	FetchedAt  time.Time `json:"fetched_at"`
	SnapshotAt time.Time `json:"snapshot_at"`

	LastModified string `json:"last_modified,omitempty"`
	ETag         string `json:"etag,omitempty"`

	// FeedTimestamp is line 1 of the payload verbatim, e.g.
	// "Sun Aug 02 21:29:05 GMT 2026". Authoritative per the feed itself, and
	// retained unparsed so a future format change cannot silently corrupt it.
	FeedTimestamp string `json:"feed_timestamp,omitempty"`

	Bytes       int64  `json:"bytes"`                // uncompressed payload size
	WireBytes   int64  `json:"wire_bytes,omitempty"` // what upstream actually sent
	StoredBytes int64  `json:"stored_bytes"`         // size on disk after gzip
	SHA256      string `json:"sha256"`               // over the uncompressed payload
}
