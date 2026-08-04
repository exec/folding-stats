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

-- The clustered (entity, ts) key answers per-entity history, but three paths read by
-- time across all entities: restoring the rolling windows at startup, the hourly
-- rollup recompute, and the project-wide hourly history. These indexes make that a
-- range scan rather than a full table sort.
--
-- Covering, for the same reason as the rollup indexes below: every one of those
-- readers wants d_score and d_wu, and a bare index on ts orders the rows but then
-- pays a random primary-key descent to fetch them. Measured over 2M rows, the
-- project-wide hourly history took 1670ms through a plain index and 282ms through
-- this one, for 5.8 MB. The primary key rides along automatically, so member_id and
-- slot come with it and the aggregate never touches the table.
-- Renamed rather than redefined. CREATE INDEX IF NOT EXISTS matches on NAME alone,
-- so reusing member_deltas_ts would have found the old bare-ts index already there
-- and silently done nothing — the schema would claim the covering index while every
-- database in existence kept the slow one. The DROP clears the old name and is a
-- no-op on every open after the first.
DROP INDEX IF EXISTS member_deltas_ts;
DROP INDEX IF EXISTS team_deltas_ts;
CREATE INDEX IF NOT EXISTS member_deltas_by_ts ON member_deltas(ts, d_score, d_wu);
CREATE INDEX IF NOT EXISTS team_deltas_by_ts   ON team_deltas(ts, d_score, d_wu);

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

-- Rollup lookups by bucket alone. The WITHOUT ROWID primary key is (entity, bucket),
-- so a predicate on bucket with no entity cannot use it: the project-wide history and
-- the hourly monthly-rollup recompute both degraded to a full table scan, and the cost
-- tracked the size of the table rather than the size of the range asked for.
--
-- These are deliberately COVERING — (bucket, points, wus) rather than (bucket) —
-- because a plain index on bucket is measurably *worse* than the scan it replaces. It
-- orders the rows but still has to fetch points and wus for each one, which on a
-- WITHOUT ROWID table means a random primary-key descent per row. Measured over 2M
-- rows: 908ms scanning, 1669ms via a plain index, 268ms via the covering index; and
-- over the 90-day default window, 238ms / 374ms / 60ms. The primary-key columns are
-- appended to every secondary index automatically, so member_id and slot come along
-- and the aggregate never touches the table at all.
CREATE INDEX IF NOT EXISTS member_daily_bucket   ON member_daily(bucket, points, wus);
CREATE INDEX IF NOT EXISTS member_monthly_bucket ON member_monthly(bucket, points, wus);
CREATE INDEX IF NOT EXISTS team_daily_bucket     ON team_daily(bucket, points, wus);
CREATE INDEX IF NOT EXISTS team_monthly_bucket   ON team_monthly(bucket, points, wus);

-- Project-wide production, one row per period, no entity column.
--
-- These are pure duplication — every figure is the sum of the team tables — and they
-- exist because that sum is the one query whose cost has no ceiling. Per-entity
-- history reads a bounded slice of one clustered key; the project's reads every team
-- that produced in the range, which is ~130k rows per bucket and grows with the
-- project. Summing five years of team_daily is a quarter of a billion rows for an
-- answer that is 1,825 numbers.
--
-- INTEGER PRIMARY KEY and no WITHOUT ROWID: with a single integer key the primary key
-- *is* the rowid, so the table is already clustered on it and the WITHOUT ROWID that
-- the per-entity tables need would buy nothing here.
--
-- Never pruned. project_deltas gains 8,760 rows a year and project_daily 365, so
-- keeping every one of them forever costs less than a megabyte a decade, and it lets
-- project history answer past the raw-delta retention that bounds the per-team tables.
CREATE TABLE IF NOT EXISTS project_deltas (
    ts      INTEGER PRIMARY KEY,
    d_score INTEGER NOT NULL,
    d_wu    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS project_daily (
    bucket INTEGER PRIMARY KEY,
    points INTEGER NOT NULL,
    wus    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS project_monthly (
    bucket INTEGER PRIMARY KEY,
    points INTEGER NOT NULL,
    wus    INTEGER NOT NULL
);

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
