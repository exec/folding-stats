package store

// schema is applied on every open; every statement is idempotent.
//
// Two decisions carry most of the weight here:
//
//   - Delta tables are WITHOUT ROWID with (entity, ts) as the primary key. That
//     makes the key clustered, so "this member's last 90 days" is one seek plus a
//     sequential read instead of an index lookup per row. It is the difference
//     between SQLite being adequate for the graph queries and being a bottleneck.
//
//   - Identity ids are assigned by the model, not by SQLite. They are dense slot
//     indices into the in-memory arrays, so persisting them verbatim lets state be
//     rebuilt by appending in id order — no translation layer, and array indexing
//     rather than a map lookup on the read path.
const schema = `
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS names (
    name_id INTEGER PRIMARY KEY,
    name    TEXT NOT NULL
);

-- score/wu are the current cumulative totals, carried here so a restart can restore
-- them directly. They are updated only for entities that changed in a cycle (~1k of
-- 2.7M), which is far cheaper than rewriting every row, and far more reliable than
-- reconstructing totals by replaying the newest snapshot: a donor that vanished from
-- the feed would come back as zero and wreck the leaderboard.
CREATE TABLE IF NOT EXISTS teams (
    slot    INTEGER PRIMARY KEY,        -- dense index into State.Teams
    team_id INTEGER NOT NULL UNIQUE,    -- upstream F@H team number
    name_id INTEGER NOT NULL,
    first_seen INTEGER NOT NULL,
    score   INTEGER NOT NULL DEFAULT 0,
    wu      INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS members (
    member_id  INTEGER PRIMARY KEY,     -- dense index into State.Members
    name_id    INTEGER NOT NULL,
    team_id    INTEGER NOT NULL,        -- upstream team number, not a slot
    first_seen INTEGER NOT NULL,
    score      INTEGER NOT NULL DEFAULT 0,
    wu         INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS members_pair ON members(name_id, team_id);

-- Raw per-cycle production. Only non-zero deltas are stored: ~99% of donors are
-- idle in any given hour, and that sparsity is what keeps this table tractable.
CREATE TABLE IF NOT EXISTS member_deltas (
    member_id INTEGER NOT NULL,
    ts        INTEGER NOT NULL,
    d_score   INTEGER NOT NULL,
    d_wu      INTEGER NOT NULL,
    PRIMARY KEY (member_id, ts)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS team_deltas (
    slot    INTEGER NOT NULL,
    ts      INTEGER NOT NULL,
    d_score INTEGER NOT NULL,
    d_wu    INTEGER NOT NULL,
    PRIMARY KEY (slot, ts)
) WITHOUT ROWID;

-- The clustered (entity, ts) key answers per-entity history, but restoring the
-- rolling windows after a restart reads by time across all entities. These indexes
-- make that a range scan rather than a full table sort.
CREATE INDEX IF NOT EXISTS member_deltas_ts ON member_deltas(ts);
CREATE INDEX IF NOT EXISTS team_deltas_ts ON team_deltas(ts);

-- Rollups. bucket is a UTC day number (unix/86400) for daily, the day number of
-- the week's Sunday for weekly, and year*12+month for monthly. Weekly has no table:
-- it is summed from daily on read, since a week is exactly seven day buckets.
CREATE TABLE IF NOT EXISTS member_daily (
    member_id INTEGER NOT NULL, bucket INTEGER NOT NULL,
    points INTEGER NOT NULL, wus INTEGER NOT NULL,
    PRIMARY KEY (member_id, bucket)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS member_monthly (
    member_id INTEGER NOT NULL, bucket INTEGER NOT NULL,
    points INTEGER NOT NULL, wus INTEGER NOT NULL,
    PRIMARY KEY (member_id, bucket)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS team_daily (
    slot INTEGER NOT NULL, bucket INTEGER NOT NULL,
    points INTEGER NOT NULL, wus INTEGER NOT NULL,
    PRIMARY KEY (slot, bucket)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS team_monthly (
    slot INTEGER NOT NULL, bucket INTEGER NOT NULL,
    points INTEGER NOT NULL, wus INTEGER NOT NULL,
    PRIMARY KEY (slot, bucket)
) WITHOUT ROWID;

-- One row per ingested snapshot pair. Doubles as the audit log and as the record
-- of which snapshots have already been applied, so replay is idempotent.
CREATE TABLE IF NOT EXISTS cycles (
    ts               INTEGER PRIMARY KEY,
    team_snapshot_at INTEGER NOT NULL,
    user_snapshot_at INTEGER NOT NULL,
    team_rows        INTEGER NOT NULL,
    user_rows        INTEGER NOT NULL,
    malformed        INTEGER NOT NULL,
    new_members      INTEGER NOT NULL,
    new_teams        INTEGER NOT NULL,
    member_deltas    INTEGER NOT NULL,
    team_deltas      INTEGER NOT NULL,
    regressions      INTEGER NOT NULL,
    duration_ms      INTEGER NOT NULL
);
`

// pragmas are set per connection.
//
// synchronous=NORMAL rather than FULL: with WAL, NORMAL only risks losing the last
// commit on OS crash, and a lost cycle is recoverable by replaying the raw archive.
// Paying FULL's fsync on every one of ~35k row inserts per cycle is not worth it.
var pragmas = []string{
	"PRAGMA journal_mode = WAL",
	"PRAGMA synchronous = NORMAL",
	"PRAGMA temp_store = MEMORY",
	"PRAGMA cache_size = -65536", // 64 MB page cache
	"PRAGMA busy_timeout = 10000",
	"PRAGMA foreign_keys = OFF",
}
