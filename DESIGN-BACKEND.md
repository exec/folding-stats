# Backend Design

Companion to `EOC-FEATURE-INVENTORY.md` (referenced below as §). Go backend, in-memory hot state,
SQLite history, single 2–4 GB VPS.

---

## 1. Principles

1. **The feed gives us two numbers; everything else we derive.** `score` and `wu` are cumulative
   lifetime totals (§3). Ranks, rates, deltas and changes all come from differencing successive
   snapshots. The ingest pipeline is a **differencing engine**, not a loader.
2. **Storage grain is `(name, team)`** — the feed's native grain (§3, R11). Donor aggregation is a
   **read-time view**, never a schema commitment. Merge policy stays changeable without re-ingest.
3. **A cycle produces an immutable snapshot.** Everything served between cycles is a pure function
   of that snapshot, so we compute once and serve from memory (R6).
4. **Honest field names** (R7). We do not inherit `24hr Avg` for a 7-day average.

---

## 2. Identity and IDs

Three ID spaces, all internal surrogates except `team_id`:

| Entity | ID | Source |
|---|---|---|
| Team | `team_id` | **F@H's own** team number — stable, use directly |
| Name | `name_id` | ours, interned string → int32 |
| Member | `member_id` | ours, unique per `(name_id, team_id)` |

`member_id` is the primary key of the whole system. Assigned on first sighting, never reused.
There is no F@H donor ID (R12) — the donor is `name_id`, and a "donor" in the API is the set of
members sharing a `name_id`.

**Duplicate `(name, team)` rows are summed at parse time** (§3) — 6,984 exist, and summing is what
reconciles against the authoritative team totals.

---

## 3. Storage

### 3a. Hot state (RAM, rebuilt at boot)

```go
type Hot struct {
    Teams   []Team      // indexed by dense team slot
    Members []Member    // indexed by member_id
    Names   NameArena   // one []byte + offsets, not 2.1M Go strings

    memberIdx map[nameTeamKey]int32  // (name_id,team_id) -> member_id
    teamIdx   map[int32]int32        // f@h team_id -> slot

    RankedMembers []int32  // member_id, sorted by score desc
    RankedTeams   []int32
    RankedDonors  []int32  // name_id, sorted by aggregated score desc

    Snapshot SnapshotMeta
}

type Member struct {
    NameID, TeamID   int32
    Score, WUs       int64
    LastUpdate       int64  // points in most recent cycle
    Today, ThisWeek  int64
    Last24h, Last7d  int64
    RankGlobal       int32
    RankInTeam       int32
    RankChange24h    int32
    RankChange7d     int32
}   // 64 B
```

**Memory — measured, not estimated** (`TestCorpusMemoryBudget`, real 2026-08-02 corpus):

```
members = 2,710,066   teams = 129,952   names = 2,237,758
live heap after Apply = 223.2 MB        Apply took 1.14 s
```

**223 MB**, against a 455 MB projection — the arena outperformed the estimate. The test
asserts a 900 MB ceiling so a regression in the arena or the member index surfaces here
rather than on the box.

Note `members` (2,710,066) is below parsed user rows (2,720,047): 9,981 rows collapse into
6,984 duplicate `(name, team)` pairs, which are **summed**, not deduplicated. And `names`
(2,237,758) exceeds distinct donor names (2,123,785) because team names share the arena.

The `NameArena` is what makes this fit: names live in one `[]byte` with `[]uint32` offsets and
an open-addressed `[]int32` index, so the GC sees three pointers instead of 2.2M strings.

### 3b. History (SQLite, `modernc.org/sqlite` — pure Go, no cgo)

```sql
-- identity
CREATE TABLE names   (name_id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);
CREATE TABLE teams   (team_id INTEGER PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE members (member_id INTEGER PRIMARY KEY,
                      name_id INTEGER NOT NULL,
                      team_id INTEGER NOT NULL,
                      first_seen INTEGER NOT NULL,   -- unix ts of first snapshot
                      UNIQUE(name_id, team_id));

-- raw deltas, 0-90 days
CREATE TABLE member_deltas (
    member_id INTEGER NOT NULL,
    ts        INTEGER NOT NULL,   -- snapshot unix ts
    d_score   INTEGER NOT NULL,
    d_wu      INTEGER NOT NULL,
    PRIMARY KEY (member_id, ts)
) WITHOUT ROWID;                  -- clustered: per-entity range scan is a seek + sequential read

CREATE TABLE team_deltas (... same shape, PRIMARY KEY (team_id, ts)) WITHOUT ROWID;

-- rollups, 90 days+
CREATE TABLE member_daily   (member_id, day,   points, wus, PRIMARY KEY(member_id, day))   WITHOUT ROWID;
CREATE TABLE member_weekly  (member_id, week,  points, wus, PRIMARY KEY(member_id, week))  WITHOUT ROWID;
CREATE TABLE member_monthly (member_id, month, points, wus, PRIMARY KEY(member_id, month)) WITHOUT ROWID;
-- team_daily / weekly / monthly identical

-- ingest audit log
CREATE TABLE snapshots (ts INTEGER PRIMARY KEY, source_mtime TEXT, etag TEXT,
                        rows INTEGER, malformed INTEGER, duration_ms INTEGER);
```

`WITHOUT ROWID` matters: it makes `(entity, ts)` the clustered key, so "give me this member's last
90 days" is one seek plus a sequential read — exactly the graph query.

**Only non-zero deltas are written** (R14). At ~1% of members changing per cycle that is ~27k
rows/cycle rather than 2.7M — the difference between ~7 GB/year and ~500 GB/year.

**Identity ids are assigned by the model, not by SQLite.** They are dense slot indices into the
in-memory arrays, persisted verbatim, so restoring state is an append in id order — no translation
layer, and array indexing on the read path. `LoadIdentity` asserts each row lands on its original
slot: a silent reassignment would reattribute history to the wrong donor, which nothing downstream
could detect.

### Measured (real corpus, `internal/store` corpus tests)

| Operation | Time | Notes |
|---|---|---|
| **First ingest** (2.71M identity rows) | **8.6 s** | one-time cold start; DB 141 MB |
| **Reload identity on restart** | **2.1 s** | 2.71M members + 2.24M names |
| **Steady-state cycle** — apply | **1.29 s** | 27,198 deltas from a 1% active set |
| **Steady-state cycle** — write | **52 ms** | single transaction |
| **Total per cycle** | **≈ 1.3 s** | against EOC's **12m 2s** (§2l) |

Roughly **550× faster than EOC**, inside an hourly publish interval — leaving the rest of the
hour idle for ranking, metrics and serving.

### 3c. Raw snapshot archive (built — `internal/feed`)

Separate from, and upstream of, the SQLite store. Snapshots are kept **verbatim and
never parsed at capture**, so a parser or metrics change costs a replay rather than the
history itself. Live ingest and replay both read through `Snapshot.Open()`, so there is
one code path regardless of whether a snapshot arrived seconds or months ago.

**Measured compression** (real 2026-08-02 snapshot, 66,345,881 B user feed):

| Codec | Size | Time |
|---|---|---|
| gzip −9 | 32.6 MB | 3.3 s |
| zstd `SpeedBetterCompression` | 33.2 MB | 0.7 s |
| **zstd `SpeedBestCompression`** ← chosen | **30.6 MB** | 3.5 s |
| zstd −19 (CLI) | 27.8 MB | 16.9 s |

Compression runs at most hourly, so its cost is irrelevant; size and *decompression*
speed are what matter, and zstd wins both against gzip. Text this high-entropy (2.1M
mostly-unique donor names) only compresses ~2.2×, so storage is a real constraint.

**Per snapshot pair: ~32.6 MB** (30.6 user + 2.0 team).

**Cadence is confirmed hourly** (§3), so:

| | Per day | Per year |
|---|---|---|
| Hourly (actual) | **744 MB** | **~272 GB** |

Provisioned 600 GB — roughly 2 years of unthinned capture, on a thin-provisioned ZFS subvol so
it costs nothing until used.

**Thinning policy (implemented — `feed.DefaultRetention`).** Keep every snapshot for 90 days,
then one per day indefinitely. Two separate stores are easy to conflate, so to be explicit:

| Store | 0–90 days | After 90 days | Steady state |
|---|---|---|---|
| **Raw snapshot archive** (§3c) | every snapshot | 1/day | ~67 GB window + **~12 GB/yr** |
| **SQLite deltas** (§3b, §3d) | per-cycle deltas | daily rollups | ~3 GB window + **~1 GB/yr** |

So the **272 GB/year figure is the *unthinned* raw archive only**. With thinning, total
growth is **~13 GB/year on top of a ~70 GB rolling window** — roughly 135 GB after five
years, 200 GB after ten. The 600 GB volume is comfortable indefinitely.

### 3d. Retention (chosen policy)

| Age | Granularity |
|---|---|
| 0–90 days | raw per-cycle deltas |
| 90 d – 2 yr | daily rollups |
| 2 yr+ | weekly / monthly rollups |

A nightly compaction job rolls up and prunes. ⚠️ **Rollups must be written before the raw rows are
pruned, in that order, in one transaction** — the reverse loses data irrecoverably.

---

## 4. Ingest pipeline

Runs on a ticker. The feeds publish **hourly** (§3), so poll more often than that and let
conditional GETs absorb the misses.

### ⚠️ The two feeds are NOT an atomic pair
Measured: the team feed publishes at **:29** and the user feed at **:30**. Points banked in
that 60-second window land in one file and not the other.

Confirmed empirically — summing user rows per team against the team feed's own score gives
**129,750/129,952 exact (99.84%), 201 under, 1 over**. The single overshoot is 1 point: a
donor who scored between the two publishes. Consequences:

- **Never assume a team total equals the sum of its members.** Use the team feed's `score` as
  authoritative (§3) and treat the user rows as the breakdown.
- A reconciliation check is still valuable, but must tolerate small discrepancies **in both
  directions**, not just one.
- When pairing snapshots for ingest, match on the *nearest* team+user pair rather than
  requiring identical timestamps — they will never be identical.

```
1. Conditional GET both files (If-None-Match on stored ETag)
   └─ 304? done, no work.
2. Stream-parse into next[] indexed by member_id
   ├─ reassemble records across embedded newlines (§3 parser gotcha)
   ├─ sum duplicate (name,team) rows
   └─ intern new names / assign new member_ids
3. Diff next[] vs cur[] -> sparse delta list [(member_id, d_score, d_wu)]
4. Persist: one SQLite transaction (deltas + snapshot row)
5. Recompute derived metrics (§5) and ranks (§6)
6. Atomically swap the *Hot pointer
```

Steps 2–6 build entirely into fresh buffers; readers keep serving the old snapshot until the
pointer swap. No locks on the read path — `atomic.Pointer[Hot]`.

**Parser requirements** (non-negotiable, from §3):
- Split on `\t` **only**, never on whitespace — team names contain runs of double-spaces.
- Accumulate physical lines until the field count matches; names contain literal `\n`.
- Tolerate stray `''`, `'/'`, `'//'` lines and rows with unexpected field counts; count them into
  `snapshots.malformed` rather than failing the cycle.
- Treat the file's line-1 timestamp as authoritative for the snapshot `ts`.

**Expected cost:** parse 63 MB + diff + sort 2.7M ≈ **2–5 s** in Go. EOC takes 12m 2s (§2l).

**Failure handling** (R13): upstream feed outages are "a common occurrence". On fetch failure or a
malformed-row count above a threshold, **keep serving the previous snapshot** and mark
`status.stale = true` with the age. Never serve a partial ingest.

---

## 5. Derived metrics — the sliding-window ring

The tempting design (keep N full snapshots to diff against) does not fit: 24 hourly snapshots ×
2.72M × 8 B = **522 MB** just for `Last24h`. Instead, exploit sparsity.

Keep a **ring buffer of sparse delta lists**, one per cycle, covering 7 days:

```go
type DeltaList struct {
    TS      int64
    Entries []struct{ MemberID int32; DScore, DWU int64 }  // ~35k, not 2.7M
}
ring [168]DeltaList   // 7 days hourly; ~420 KB each => ~70 MB
```

Maintain running totals incrementally — on each cycle, **add the entering list and subtract the
leaving one**:

| Field | Maintenance |
|---|---|
| `LastUpdate` | the newest delta list, directly |
| `Last24h` | `+= entering`, `-= list falling out of the 24 h window` |
| `Last7d` | `+= entering`, `-= list falling out of the 168-cycle ring` |
| `Today` | accumulate; **reset to 0 at 00:00 UTC** |
| `ThisWeek` | accumulate; **reset at 00:00 UTC Monday** |

Cost per cycle is O(changed rows), not O(members) — microseconds, not seconds.

**`points_per_day_7d_avg` = `round(Last7d / 7)`** (§2f, confirmed verbatim by EOC's FAQ §2j).

⚠️ **The division rounds to nearest, not truncates.** Checked against three independently
captured EOC pages, truncation reproduces *none* of them and rounding reproduces *all three*:

| Source | `Last 7days` | ÷7 | truncated | rounded | EOC published |
|---|---|---|---|---|---|
| Wisconsin team | 51,364,842 | 7,337,834.57 | 7,337,834 ✗ | **7,337,835** ✓ | 7,337,835 |
| DH (user) | 49,559,068 | 7,079,866.86 | 7,079,866 ✗ | **7,079,867** ✓ | 7,079,867 |
| Site aggregate | 123,079,584,757 | …822.43 | …822 ✓ | **…822** ✓ | 17,582,797,822 |

A one-point discrepancy sounds trivial, but this figure is what donors compare against EOC
directly — matching exactly keeps the two sites reconcilable.

**Measured at corpus scale** (`TestWindowMemoryAtCorpusScale`): 2.71M entities, 27k active per
cycle, a full week retained (168 cycles) → **208.9 MB heap, 1.35 ms per push**. Against the
~522 MB that keeping 24 full snapshots would cost for a single field.

⚠️ Two honesty notes we should surface in the API rather than paper over:
- Until the ring is full, `Last7d` covers less than 7 days. Expose
  `avg_window_complete: false` plus the actual window length during the first week (§ cold start).
- `Today`/`ThisWeek` are **UTC calendar buckets**. We deliberately do *not* reproduce EOC's
  Central-time midnight quirk (§2j) — but the semantics must be documented explicitly, because
  this is exactly where integrators go wrong.

---

## 6. Ranking

After each ingest:

**Measured** (`TestCorpusRanking`, real corpus): the whole table — 2,710,066 members, 129,952
teams, 2,123,785 donors — builds in **142 ms**. Comfortably inside the cycle budget.

⚠️ **The radix key is inverted (`^score`), not the result reversed.** Radix is stable and sorts
ascending, so seeding with ids in order leaves ties in ascending id order. Reversing the finished
array fixes the score direction but flips ties to *descending* id order — and with millions of
donors tied on zero, unstable tie ordering makes rank movement pure noise between cycles. Sorting
on `^score` gives descending scores while preserving ascending ids within a tie. This was a real
bug caught by a test asserting tie stability.

In-team rank is counted by walking the *global* order, so the two rankings agree by construction:
a member ahead globally can never fall behind within a shared team. Team lookup is a dense array
indexed by upstream team number (~5 MB) rather than a map, turning 2.7M map lookups per cycle into
array increments.

`RankChange24h` / `RankChange7d` need historical *ranks*, not scores. Keep two `[]int32` rank
snapshots (24 h and 7 d ago) — 10.9 MB each, refreshed on schedule. Cheap enough to just hold.

**Donor aggregation (R1)** is computed here: group members by `name_id`, sum score/WUs, rank.
A donor's per-team breakdown (needed by R9/R10 in one call) is stored CSR-style — one flat
`[]int32` of member slots plus offsets, rather than 2.1M individual slices.

Pseudo-identity flagging is a **derived boolean**, never a filter:

```go
donor.LikelyNotAPerson = donor.TeamCount > cfg.PseudoIdentityTeams  // default 50
```

**Measured against the real corpus: 358 donors flagged of 2,123,785 (0.017%)**, worst being
`PS3` on **10,426 teams** with 3.2 billion aggregated points. The threshold is not sensitive —
legitimate multi-team folders sit in the low tens, the pseudo-identities three orders of magnitude
above. Flag, never hide: the points are real even when the person is not. Threshold lives in
config so the policy can change without re-ingesting.

---

## 7. API surface (v1)

All JSON, unauthenticated (R3), every response carrying a `status` block (R14).

```
GET /api/v1/status

GET /api/v1/teams?page=&per_page=&sort=&order=
GET /api/v1/teams/{team_id}
GET /api/v1/teams/{team_id}/members?page=&per_page=&sort=&active_only=
GET /api/v1/teams/{team_id}/history?metric=&granularity=&from=&to=

GET /api/v1/donors?page=&per_page=&sort=&order=
GET /api/v1/donors/{name}                     # aggregated + per-team breakdown, ONE call (R10)
GET /api/v1/donors/{name}/history?metric=&granularity=&from=&to=&team_id=

GET /api/v1/search?q=&type=team|donor|team_id|donor_id
```

`metric` ∈ `points | wus`; `granularity` ∈ `cycle | daily | weekly | monthly`.

**Naming** (R7) — deliberately not EOC's vocabulary:

```jsonc
{
  "points_total": 3306629142,
  "wus_total": 48854,
  "points_last_update": 10347920,
  "points_today_utc": 33469829,
  "points_this_week_utc": 33469829,
  "points_last_24h": 34760640,
  "points_last_7d": 51364842,
  "points_per_day_7d_avg": 7337835,     // NOT "24hr avg" (§2f)
  "rank_global": 747,
  "rank_change_24h": 4,
  "first_seen": "2025-03-06T00:00:00Z",  // first seen BY US, not by F@H
  "status": { "snapshot_at": "...", "next_update_estimate": "...", "stale": false }
}
```

**R10 enforcement:** `/donors/{name}` returns donor totals + the full per-team breakdown + one
default series. Request count never scales with team count. Additional granularities are separate
single calls — the rule is "no N+1 across teams", not "everything in one payload".

**Caching:** `ETag` per snapshot, `Cache-Control: public, max-age=<seconds to next cycle>`. Since
the data is immutable between cycles, most repeat traffic should never reach application code.

### Implemented — deviations from the sketch above

- **`/v1/donors/{name}/teams`** added, and the inline breakdown capped at 100 rows. `PS3` spans
  10,426 teams, and embedding them all produced a **2.77 MB** response; capped — ordered by points
  first, so the dropped rows matter least — it is **27.6 KB**, with `teams_truncated: true`
  pointing at the paginated endpoint. R10's "one request ≠ one giant payload" caveat turned out to
  be load-bearing rather than theoretical.
- **Every response is enveloped** as `{snapshot, data, page?}` — one shape for every route, so
  consumers learn the structure once and freshness always travels with the data.
- ⚠️ **Members and teams need separate metrics windows.** Their slot numbering is independent and
  both start at zero, so a shared window reports one entity's production as the other's. Caught
  only on real data, where team slot 0 ("Default") displayed member slot 0's ("Anonymous") rate
  and every other team read zero. A unit test with one entity cannot see this.

**Measured end-to-end** (two real cycles, 2.71M members, served from the in-memory snapshot):

| | |
|---|---|
| Cold ingest (first cycle) | 12.5 s |
| Steady cycle | 1.8 s |
| Publish (rank rebuild) | 189 ms |
| API latency, any endpoint | **6–10 ms** (including curl startup) |

---

## 8. Testing strategy

The backend is mostly arithmetic over a well-understood feed, so it's unusually testable — and we
already hold a **validated fixture**: the 2026-08-02 21:29 GMT snapshot, cross-checked against EOC
captures where idle donors matched byte-for-byte (§3).

| Layer | Tests |
|---|---|
| Parser | golden tests on the real 63 MB file; explicit cases for embedded newlines, double-spaced team names, stray `/` lines, 5-field rows, duplicate `(name,team)` |
| Invariants | `sum(members by team) == team.score` for 99.84% of teams; the 202 known mismatches asserted as *expected*, direction-checked (team file always ≥ sum) |
| Differencing | synthetic snapshot pairs: new member, departed member, renamed, zero-delta, score decrease (shouldn't happen — assert and alarm) |
| Metrics | assert `points_per_day_7d_avg == points_last_7d/7`; assert window arithmetic against a hand-built 168-cycle fixture |
| Regression vs EOC | reproduce EOC's published figures for the captured teams/users — we have exact expected values for Wisconsin (§2h), USA (§2e), DH (§2f) |
| Ranking | radix sort result identical to `sort.Slice`; in-team ranks consistent with global order |
| Retention | rollup-then-prune ordering; assert no data loss across a simulated 2-year compaction |

**Load target:** the whole hot set is in RAM and responses are precomputed — a leaderboard page
should be sub-millisecond. Benchmark to confirm we're serving from memory and not accidentally
hitting SQLite on the read path.

---

## 8a. Bugs found by deep testing

Recorded because each was invisible to the tests that existed at the time, and the
pattern is worth remembering.

| Bug | Why it hid | How it was caught |
|---|---|---|
| **Missed team publish dropped an hour of donor production.** The pairing index consumed team snapshots instead of reusing them, so a user snapshot with no *newer* team snapshot was skipped entirely. | Upstream had never missed a publish during development. | A pairing test written for the outage case the FAQ warns is "a common occurrence". |
| **Restart served zeros for everything.** Identity came back but cumulative totals did not, and every cycle was already marked applied, so nothing ever restored them. | Restart was never exercised end to end; unit tests built state in one process. | A test that ingests, closes the store, reopens and compares. |
| **Restart lost all rate windows.** Fixed alongside the above: totals are restored from the identity tables, rates by replaying the last 7 days of stored deltas. | Totals looked right, so the failure was invisible unless rates were checked specifically. | Asserting `points_last_24h` survives a restart, not just `points_total`. |
| **Data race between ingest and the API.** The published snapshot points at the live `State` and windows, which the next cycle mutates in place and may reallocate. | Single-threaded tests never overlap a read with a cycle. | `-race` with four goroutines hammering the API during ingest. |
| **Team and member rates aliased.** Their slot spaces are independent and both start at zero, so one shared metrics window reported a member's production as a team's. | A fixture with one entity cannot distinguish the two id spaces. | Running the real pipeline: team slot 0 ("Default") displayed member slot 0's ("Anonymous") rate. |
| **Donor history issued one query per team.** `PS3` spans 10,426 teams, so a single API request meant 10,426 round trips. | Test fixtures had two teams per donor. | Reviewing against the known corpus shape rather than the fixture. |
| **Donor breakdown returned 2.77 MB.** Embedding all 10,426 teams inline. | Same. | Querying the real service. |
| **`Archive.List` parsed every sidecar ever written, hourly.** ~17k files a year, on every ingest. | The archive was hours old, not years. | Reasoning about the two-year state, then adding `ListSince` with date-shard pruning. |
| **Daily rollups were never pruned, and the monthly recompute rescanned all of them.** Both unbounded, against a policy that says daily collapses to monthly after two years. | Compaction had only ever run over a few test rows. | Comparing the implementation against the retention table in this document. |
| **`HAVING 2 >= ?` compared the integer literal 2.** SQLite accepts ordinal column references in `GROUP BY` but not in `HAVING`, so the bounded monthly recompute silently inserted nothing. | Same query worked unbounded; the ordinal only appeared when adding the filter. | A test asserting a pruned month keeps its production. |
| **The freshness probe was cacheable.** `/v1/status` carried the same `max-age` as data routes, so a client polling "has anything changed yet" was handed its own previous answer. The countdown could sit at "checking" through an actual publish. | Every test harness stripped `Cache-Control`, so the browser cache never entered the picture. | Watching the real server in a real browser: `fetch` reported `stale:false` while `curl` reported `true`. Fixed with `no-store` on that route, `cache: reload` on a refresh re-fetch, and a proxy that forwards caching verbatim. |
| **A deploy could mix module versions.** Assets were `max-age=300` with no fingerprint, so a browser could run a new `app.js` against a cached old `countdown.js` — which fails as a blank page, not an error. | Nothing had been deployed twice yet. | Adding `countdown.js` and watching the browser serve a stale copy. Fixed by stamping every internal module specifier with one content hash. |
| **Staleness flapped every cycle.** `stale = now > at+1h`, but upstream's interval is 3606–3613s and we poll every 10 minutes, so the flag was true for minutes of every single hour. | The assumption "exactly an hour apart" was written from two observations. | Measuring five consecutive publishes. Fixed with a 20-minute grace so the flag means a missed publish, not routine lateness. |
| **The overview's project chart plotted team 0.** Team 0 is the "no team specified" bucket, about a seventh of the project — the card was labelled "Project production" and showed 108 M/hr against the project's 740 M/hr. | There was no project-wide history endpoint, and team 0 renders a plausible-looking curve. | Comparing the chart against `summary.points_last_update` on real data. Fixed by adding `GET /v1/summary/history`. |
| **Bucketed history ranges used an exclusive upper bound.** Asking for a single month at monthly granularity returned nothing, because `bucket < MonthBucket(to)` excludes the month containing `to`. | Test ranges always spanned several buckets. | Writing a test for a range narrower than one bucket. |

**The recurring theme:** almost every one needed *real data*, *a second process*, or
*concurrency* to surface. The exceptions are the last three, which came from checking the
implementation against this document's own stated policy — a reminder that the design doc
is a test oracle, not just a record.

Buckets are periods, so history queries use an **inclusive** upper bound on bucketed
granularities and an exclusive one on `hourly`, whose timestamps are instants.

### Upstream cadence is measured, not assumed

Upstream does not publish on a wall clock. Five consecutive team publishes:

| Publish (`Last-Modified`) | Interval |
|---|---|
| 22:29:07 | — |
| 23:29:13 | +3606s |
| 00:29:25 | +3612s |
| 01:29:37 | +3612s |
| 02:29:46 | +3609s |

Every interval is **longer** than an hour, by a mean of ten seconds. Three consequences:

1. The publish time creeps forward, sweeping a full hour about every two weeks. So a
   UTC day has ~23.94 publishes: usually 24 hourly buckets, occasionally 23. Totals stay
   correct because each delta is attributed exactly once, but nothing may assume 24.
2. Nothing can be scheduled against `at + 1h`. `next_expected_at` comes from the median
   of the last 24 observed intervals (`internal/service/cadence.go`), clamped to
   50–75 min so a backfill or a missed publish cannot poison it. The median, not the
   mean: one skipped publish is a single two-hour interval that would drag a mean past
   the truth and hold it there for a day.
3. Because the interval is never *under* an hour, two publishes can never collide in one
   bucket — the benign direction to drift.

Both feeds come from **one generation run**: they carry an identical `feed_timestamp`,
and the user file's `Last-Modified` trails the team file's by ~63s only because it is
66 MB against 3.8 MB and takes that much longer to write.

**Polling follows the prediction.** A fixed ten-minute tick spreads the gap between
upstream publishing and us capturing it uniformly across ten minutes — five on average.
That lag is invisible in the data and very visible to a client counting down to the next
update, which would reach zero and then wait out the rest of a tick. So the archiver
polls slowly while nothing is due and every 60s once the window opens; conditional GETs
answer 304 with no body, so the extra polls cost almost nothing and bound capture lag to
about a minute.

### Load: the ceiling, measured properly

Two containers on one 16-core host, pinned to disjoint cores so the test rig cannot
steal from the thing it measures: the site on cores 0-3, the generator on cores 4-7.
Everything else on the box floats. 60 s per rate, real donor and team lookups over the
bridge.

| target | achieved | requests | errors | svc p50 | svc p99 | queue p99 | watcher |
|---|---|---|---|---|---|---|---|
| 20,000 | 19,995 | 1,200,000 | 0 | 0.10 ms | 0.61 ms | 1.07 ms | 0.20 ms |
| 24,000 | 23,993 | 1,440,000 | 0 | 0.10 ms | 0.68 ms | 1.12 ms | 0.13 ms |
| 28,000 | 27,992 | 1,680,000 | 0 | 0.11 ms | 0.92 ms | 1.28 ms | 0.14 ms |
| **32,000** | 31,992 | 1,920,000 | 0 | 0.11 ms | **4.63 ms** | 4.89 ms | 0.16 ms |
| 36,000 | 35,992 | 2,160,000 | 0 | 0.12 ms | 15.41 ms | 15.56 ms | 0.12 ms |
| 40,000 | 39,995 | 2,400,000 | 0 | 0.15 ms | 38.44 ms | 161.93 ms | 11.09 ms |
| 48,000 | 47,987 | 2,880,000 | 0 | 14.82 ms | 37.03 ms | 171.59 ms | 15.76 ms |
| 52,000 | *generator fell behind* | — | — | — | — | — | — |

**Zero errors at every rate**, up to 2.88 M requests in a single stage. The run ended
because the *generator* could not issue 52,000 req/s on four cores — 138,495 requests
were never sent — not because the service failed. The tool says so explicitly rather
than reporting the rig's limit as the server's.

`foldingd` averaged **2.75 of its 4 cores** across the whole ramp.

#### What isolation was worth

The same measurement, contaminated and clean:

| | shared cores, no gzip | shared cores, gzip | pinned, gzip |
|---|---|---|---|
| p99 at 24,000 | 6.89 ms | 2.51 ms | **0.68 ms** |
| p99 at 32,000 | — | 33.40 ms | **4.63 ms** |
| knee | ~22,000 | ~26,000 | **~32,000** |

The earlier knees were substantially the load generator starving the server it was
measuring. Both prior runs understated the service, and "26,000 is a floor, not the
knee" turned out to be the right reading — the real one is around 32,000.

#### The numbers to quote

- **~28,000 req/s** is the comfortable ceiling: p99 under a millisecond, watcher 0.14 ms,
  0.30 Gbit/s. A person cannot tell the difference from an idle server.
- **48,000 req/s** is the highest rate served cleanly, but p50 has reached 14.8 ms by
  then — working, not comfortable. 0.51 Gbit/s.
- Nothing errored at any rate tested. The failure mode is latency, not refusal.

Both figures now sit inside a gigabit line, which they did not before compression.

### A sticky element cannot escape its containing block

`body { height: 100% }` caps the body box at one viewport. Content overflows it and
the page scrolls, but a `position: sticky` child can only hold position *within its
containing block* — so the header stayed put for one screen and then scrolled away
with the box it belonged to.

Nothing looked broken at the top of a page, which is why it survived. It only showed
on the long leaderboards: the sticky table heading went on reserving `--header-now`
pixels for a header that was no longer on screen, leaving an empty band exactly one
header tall with rows sliding invisibly behind it.

`html { height: 100% }` with `body { min-height: 100% }` keeps whatever the full-height
rule was for and lets the box grow with the content.

### Freshness metadata rides inside cacheable bodies

Every response carries a `snapshot` block, and data responses are cacheable — both
deliberate, and together they are a trap worth writing down, because it has now been
walked into three separate ways.

A cached body is a *frozen moment*. Its `server_time` says when that body was
generated, which is honest and useful to an API consumer holding it. It is not "now",
and anything that treats it as now inherits whenever the cache was filled. The visible
symptom was every page reporting a different freshness — one tab saying the data was
eight minutes old, another forty-nine, both from the same snapshot — because each had
cached its response at a different moment and was reckoning against it.

Three rules follow, and all three are now enforced:

1. **A snapshot block is adopted only if its `server_time` is newer than the newest
   already held.** A cached replay still supplies perfectly good data — same cycle —
   it just does not get to say what time it is.
2. **One guaranteed-fresh response per page load.** `/v1/status` is `no-store` and
   served from memory, so it is fetched on boot purely to anchor the clock. Without
   it, a page whose every request was answered from cache would never learn the time.
3. **Content that changes on deploy rather than on publish gets its own cache
   identity.** Posts are keyed to a content fingerprint with `no-cache`, not to the
   data snapshot's schedule — which had left an edited article hidden for up to an
   hour.

### Being a good citizen of someone else's server

The feeds come from `apps.foldingathome.org`, and `cf-cache-status` reports `DYNAMIC`
— Cloudflare fronts it but does not cache it, so every request reaches their nginx
origin. What we take is therefore worth measuring rather than assuming.

| | per cycle | per day (24 cycles) |
|---|---|---|
| Uncompressed (what we used to take) | 70.1 MB | 1.68 GB |
| gzip (what we take now) | 34.7 MB | 833 MB |

The `Accept-Encoding: identity` header that caused the first row was written on the
reasoning that we compress for storage ourselves. That conflated storage with
transfer: upstream serves these gzipped at roughly half the size, and declining it
cost their origin a byte for every byte we saved ourselves nothing on.

**Polling frequency is not the cost.** Polls are conditional GETs answered `304` with
no body; a payload only crosses the wire when the content actually changes. So the
download volume equals the publish volume — one copy of each published file — no
matter whether we check every ten minutes or every three hours. Fetching less often
would not reduce bytes per publish; it would only skip publishes, and the hourly
resolution that skipping costs is the thing this site has that the alternatives do
not.

Backing off is honoured properly: `429` and `503` return a `BackoffError` carrying
`Retry-After`, which overrides the poll schedule entirely — including the once-a-minute
cadence near a predicted publish, which is exactly the wrong response to being asked
to slow down. An absent or unparseable header errs long, because the point is to stop
asking.

### Timezones: instants follow the reader, buckets keep their name

Two kinds of timestamp cross the API and they are not the same kind of thing.

An **instant** — a publish time, an hourly point — is a moment that happened. It
renders in whatever timezone the browser reports, because that is the reader's own
clock and both renderings name the same moment.

A **calendar bucket** — a day, a month — is a named period, not a moment. The server
aggregates one set of rollups on UTC boundaries for every reader; there is no
per-viewer "July". Rendering the bucket's start instant through a timezone does not
translate its name, it changes it: `2026-07-01T00:00:00Z` formats as "Jun 30" west of
Greenwich, so the tooltip says June while the axis tick beside it says July.

Per-viewer buckets are not an option — it would mean a set of daily and monthly
rollups per timezone offset rather than one.

So the chart footnote names which is in play (`Times are CDT` / `Days are UTC`), and
timestamps in the header carry their zone abbreviation. Naming the zone is what keeps
a local time beside a UTC-labelled chart from looking like a disagreement — and it
makes a misconfigured client clock visible rather than quietly wrong.

### Caching: three different answers

| What | Policy | Why |
|---|---|---|
| `index.html` | `no-cache` | Carries the build stamp that invalidates everything else. Must never be held. |
| `/app.js?v=<build>` etc. | `immutable`, one year | The stamp is a hash of every asset, applied to every internal module specifier, so a URL's content can never change. A deploy changes every URL in the graph at once, which is what makes it atomic — a browser has either the whole old set or the whole new one, never a mix. |
| `/v1/status` | `no-store` | It answers "has anything changed yet". A cached answer to that is not cheap, it is wrong: a client polling for a publish is handed its own last result and waits forever. It is also the cheapest route we have, so there is nothing to save. |
| other `/v1/*` | `max-age` until the next expected publish, floor 30s | Data is genuinely immutable between cycles. The floor keeps an overdue client checking back rather than pinning a stale copy. |

The frontend's in-place refresh re-fetches with `cache: reload` for the same reason the
probe is `no-store`: it is triggered *by* a new cycle, so the previous cycle's cached
body is precisely the wrong answer. Without it the page announces fresh data over old
numbers — verified by tagging the payload per generation and watching the headline
figure change.

### Rank history is derived, not stored

Rank movement over 24 hours is reconstructed each cycle rather than recorded. A
cumulative total minus that entity's own last-24h production **is** its total a day
ago, so the earlier leaderboard comes back from a second pass of the radix sort Build
already runs. Nothing is persisted and no migration was needed to start reporting it —
the rolling windows are replayed from the delta tables at boot, so the feature works
against history that was already on disk.

Storing ranks instead would have meant a rank per entity per cycle: ~11 MB a cycle for
members, ~92 GB a year, to answer a question the deltas already contain.

Two things the reconstruction has to get right:

1. **Entities younger than the baseline have no earlier rank.** The window records the
   corpus size at each cycle, and slots are assigned densely in first-seen order, so
   `id < count` is exactly "existed then". The earlier ranking covers only those, and a
   newer entity reports nothing rather than a position it never held.
2. **The reconstructed total is clamped at zero.** `sortDescByScore` orders on inverted
   key bits, which only holds for non-negative values. A feed glitch producing deltas
   larger than an entity's lifetime total would otherwise not misplace one row — it
   would scramble the entire ranking.

Measured cost on a synthetic 47k-member, 36k-donor corpus: publish went from 10 ms to
12 ms. That is a second sort over a corpus 57× smaller than production, so it bounds
nothing about the real one — it only says the shape is right. The work is off the read
path either way, and lands on publish rather than on any request.

Verified by reconstructing all 36,012 donors' historical positions from their own
published figures — zero mismatches, and the movements sum to zero, as they must when
every place gained is a place lost.

### Guard: reads take a lock

The published `Snapshot` shares the live ingest structures rather than copying ~300 MB
per cycle, so `Snapshot.Guard` (an `RWMutex` owned by the service) is held for reading
throughout every request and for writing during the ~1.3 s a cycle spends mutating —
about 0.04% of each hour.

---

## 8b. Benchmarking on synthetic data

The archive only accumulates in real time, so `cmd/gendata` produces
feeds in the real format with the real corpus's *proportions* — active fraction,
multi-team share, team-size skew, pseudo-identities, pathological names — and write
them through the same archive the upstream fetcher uses. `cmd/loadtest` then drives
the API with a weighted endpoint mix, reporting per-endpoint tails.

Calibration against the measured corpus (1/10 scale): teams 1.00×, members 0.95×,
donors 0.90×, total points 0.55×. Deltas per cycle came out at **1,138** against the
real 1,149.

### Three optimisations, measured end to end

Full scale: 2.51M members, 130k teams, 1.63M donors, 42 hourly cycles, 16 workers.

| | req/s | p99 | max | `history:donor` p99 |
|---|---|---|---|---|
| Baseline | 17,694 | 3.31 ms | 269 ms | 82.5 ms |
| + split read/write SQLite pools | 41,616 | 1.49 ms | 366 ms | 13.6 ms |
| + prepared-statement cache | 42,008 | 1.46 ms | 366 ms | 13.3 ms |
| + `sort.Slice` in donor breakdown | **63,666** | **1.11 ms** | **10.4 ms** | **0.84 ms** |

**3.6× throughput, 26× better worst case.** Every endpoint now sits under a
millisecond at p99.

1. **`SetMaxOpenConns(1)` serialised every read behind the writer.** WAL allows
   concurrent readers alongside one writer, but `database/sql` cannot express that
   through a single handle, so reads and writes now use separate pools.
2. **The statement cache was neutral here** and is kept only because these tables are
   still small; prepare cost is masked at 46k rows and will not stay that way.
3. **The donor breakdown insertion-sorted before capping.** A comment claimed
   breakdowns are "short in every realistic case" — but the cap is applied *after* the
   sort, so the widest donors are sorted in full. At 10,426 teams that is O(n²) over
   random offsets into a 160 MB array. This was the entire 318 ms tail.

Also removed along the way: `State.Apply` allocated two maps of ~2.5M entries per
cycle (~250 MB of garbage hourly). Slots are dense, so reusable slices indexed by slot
do the same work — **Apply went from 1.14 s to 510 ms**, at the cost of ~62 MB
retained.

⚠️ Steady-state RSS is **~1.1 GB** at full scale against a ~290 MB live heap. That gap
is Go's GC holding freed spans, and it is comfortable on the 4 GB container but worth
watching. `GOGC` is the lever if it ever is not.

---

## 9. Build order

1. **Parser + fixture tests** — get the gnarly input handling right first, in isolation.
2. **Storage layer** — SQLite schema, delta writes, rollup/compaction.
3. **Ingest loop** — fetch, diff, persist, with the atomic swap.
4. **Derived metrics** — the sliding-window ring, the piece most likely to harbour subtle bugs.
5. **Ranking.**
6. **HTTP API** + caching.
7. **Backfill/replay tooling** — replay stored snapshots to rebuild derived state after a logic
   change. Essential: it means a metrics bug doesn't cost us history.

⚠️ **Start collecting snapshots now**, before step 1 is finished. History is the one thing we
cannot backfill (§3 cold start) — a cron writing both files to disk costs nothing and every day of
delay is a day we never get back.
