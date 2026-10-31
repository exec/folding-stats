**Final validation — 31 October 2026**

The measured comparison below uses the earliest compatible revision in this
repository, `d699cfb` (3 August 2026), and the current working tree. Both binaries
served copies of the same SQLite snapshot on this Apple M3 Pro with Go 1.26.4.
These results supersede the untested-area statements in the historical notes below.

| Measurement | Earliest revision | Current | Change |
| --- | ---: | ---: | ---: |
| Mixed HTTP throughput | 15,618 req/s | 46,408 req/s | 2.97× higher |
| Mixed HTTP p50 | 0.38 ms | 0.30 ms | 21% lower |
| Mixed HTTP p99 | 14.31 ms | 1.15 ms | 92% lower |
| Search p50 | 12.83 ms | 0.16 ms | 80× faster |
| Search p99 | 21.52 ms | 0.74 ms | 29× faster |

Values are medians of three trials per version, alternating order. Each trial had
16 concurrent workers, a three-second warmup, and ten measured seconds. The existing
load-test endpoint weights were retained; both versions used the same fixed list of
100 teams, 100 donors, largest team 0, and widest donor PS3. The mix covers summary,
collections and deep pages, detail, rosters, search, and cycle history. All six runs
had zero HTTP or transport errors. Baseline throughput ranged 15,577–15,645 req/s;
current throughput ranged 45,629–46,936 req/s. Timing trials ran without other test
jobs. This is a local closed-loop comparison, not a production capacity ceiling.
The revisions also differ in features and response sizes, so the gains cannot be
attributed solely to the latest audit batches.

**Full-corpus and browser checks**

- One real upstream team/user feed pair contained 130,208 teams, 2,722,471 user rows,
  and 2,125,416 donor names; aggregation produced 2,712,482 memberships. Parser,
  model, ranking, persistence, and sparse-update corpus tests passed. First Apply
  took 375 ms with 299.7 MiB live heap; rank construction took 1.245 s. Cold SQLite
  persistence took 6.717 s, and a 27,219-member changed cycle took 302 ms to apply
  plus 363 ms to persist. These are current-code checks, not comparative timings.
- Five archived cycles used that real corpus plus two controlled donors and one
  team to exercise production from zero and a rename. Restart and clean replay
  produced exactly equal data and snapshot timestamps across 14 API routes,
  including summary, detail, histories, active rosters, search, movers, changes,
  and PS3's large team breakdown. The extra cycles are synthetic temporal changes,
  not five different upstream snapshots.
- Chromium 151 passed 17 browser checks with 39 API requests and zero page errors.
  Checks covered rendered charts, shared history requests, contributor controls,
  active roster, donor detail, a single initial rivals fetch, superseded navigation,
  and dismissal of a delayed autocomplete response. The rendered team page was
  also inspected visually.

**Consistency failure found and fixed during ingest validation**

The full-corpus probe reproduced 146 responses whose old snapshot timestamp
accompanied newer team totals or donor breakdowns. Applying state, committing
SQLite, updating windows, building ranks, and publishing now share the ingest
write lock. Waiting HTTP, MCP, badge, and metadata readers reselect the published
snapshot after acquiring the read lock. A failed commit withdraws the unsafe
snapshot until restart. Response writes, including conditional 304 headers, release
the lock first so a slow client cannot hold up ingest.

The repeat probe sampled 6,160 responses across publication with **zero mixed
snapshots**. Its p99 was 2.37 ms and its maximum was 2.534 s; observed peak process
RSS was 1,685 MiB. The corresponding mixed load run completed 23,568 requests with
zero HTTP errors and 2.02 ms service p99. Requests arriving during publication wait
for correctness: this pause caused the open-loop generator to issue only 295 of
its target 300 req/s, leaving 432 requests unissued. That run therefore establishes
consistency under concurrent requests, not sustained 300 req/s during ingest.
The observed pause is not an upper bound for larger updates or catch-up replay.

Deterministic regression tests cover readers waiting through a blocked commit,
failed-commit withdrawal, and releasing the read lock before response writes.
`go test -race ./... -timeout 120s`, `go vet ./...`, and `git diff --check` pass.
Raw trial logs, scripts, browser screenshots, corpus output, and equality captures
are saved locally under `data/validation-2026-10-31/artifacts/` (ignored by Git).

Not checked: production network/CDN behavior, sustained production traffic, or
repair of an existing deployment's historical gaps. Broad-prefix search still
ranks a bounded candidate set. No deployment or historical data repair was made.

---

**Performance and code review — 7 September 2026**

Reviewed the HTTP/MCP request paths, rankings and rosters, SQLite history queries,
ingestion and its request lock, browser loading, and bot/agent connections. Focused
improvements are implemented in the working tree. No dependencies were added.

**Follow-up — 28 October 2026**

The main bugs in all six findings below are now fixed. Incremental ingest skips
older archive pairs; explicit baseline flags preserve production from zero through
restart and replay; team renames are persisted even without production; navigation
owns its view and cleanup; team search prioritizes exact names before limiting
results; and history presentation changes reuse fetched and prepared data.

The follow-up also caches large-team MCP activity summaries, exercises the actual
large-roster cache branch in tests, and prevents obsolete contribution-chart loads
from recreating charts after cleanup or overwriting a newer selection.

`go test -race ./...`, `go vet ./...`, and `git diff --check` pass. The new regression
checks fail against the corresponding original code. No production load test,
live-browser check, or real-corpus replay was run. Existing historical gaps require
an ordered replay to repair. Broad-prefix search still ranks a bounded candidate
set; superseded browser requests finish, but their obsolete results are ignored.

**Next batch — 28 October 2026**

- Active-only lifetime roster pages now reuse the existing snapshot cache for large
  teams. Small teams keep the direct scan. The first cached request builds the
  filtered order; subsequent pages slice it. With about 883k synthetic memberships
  and recent producers at the lifetime tail, the paging helper measured about
  0.55 ms uncached versus 17 ns with a warm cache, with no warm-path allocations.
  These are helper timings, not HTTP response times.
- Contribution charts share pending and resolved histories by contributor,
  granularity, and snapshot. Rate changes reuse responses; shape and legend changes
  reuse prepared arrays and the chart observer. Failures remain retryable.
- Roster and rivals controls ignore obsolete responses and errors. Standalone rivals
  pages reuse the response already fetched for the heading instead of fetching it
  twice, and paging links retain their team scope.

The full race suite, vet, and whitespace checks pass after this batch. Regression
checks cover cache reuse and fallback, history invalidation and retry, overlapping
controls, the single initial rivals request, and scoped paging links. Production
load, live-browser behavior, and real-corpus replay remain unchecked.

**Search and changes-feed batch — 29 October 2026**

- Live search invalidates results as soon as input changes. It cancels queued and
  in-flight searches when superseded, cleared, dismissed, or used to navigate; old
  responses and errors cannot reopen or erase the current dropdown.
- The shared donor search selects the exact name through the existing identity
  index before limiting prefix candidates. MCP now preserves low-ranked exact
  donors, and REST no longer repeats exact-name selection and deduplication.
- Changes queries now cover `(since, snapshot.at]`. A cycle committed before its
  snapshot is published cannot leak into an older response, and it becomes visible
  when that snapshot is published. Both bounds use the existing time index.

Regression tests cover each fix and fail with the corresponding behavior disabled.
The full race suite, vet, and whitespace checks pass; affected backend race tests
were repeated after refining the indexed exact-name selection. Production load,
live-browser behavior, and real-corpus replay remain unchecked. Broad-prefix search
still uses a bounded candidate set.

**Original findings, in priority order**

1. **P1 — An older archive pair can rewind live statistics.**
   [Service.Ingest](internal/service/service.go) checks whether a timestamp was already
   applied, but not whether it precedes the current state. Its two-hour lookback can
   discover a previously missing older pair. A local integration reproduction ingested
   22:30, then discovered 21:30: `Ingest` returned `(1, nil)`, the live state moved back
   to 21:30, and a member's total fell from 1,000 to 600. The metric window and database
   latest timestamp remained at 22:30. Reject older pairs on the incremental path;
   historical insertion needs an explicit ordered replay. This is unchanged.

2. **P1 — A known entity's first production from zero is discarded.**
   [State.Apply](internal/model/state.go) uses nonzero previous score/work units as
   evidence that an entity already existed. A zero-total entity therefore keeps being
   treated as a first sighting. The reproduced `0 → 100` transition produced a live
   total of 100, zero member/team 24-hour production, and a persisted member total of
   zero. Persistence updates only entities with deltas. Track first sighting from
   identity creation rather than score. This is unchanged.

3. **P2 — Team renames are not persisted.**
   [WriteCycle](internal/store/store.go) updates team score and work units but omits
   `name_id`. The reproduced `old/100 → new/200` transition left `new` in memory and
   `old` in SQLite. Renames therefore revert on restart; name-only changes also need
   persistence without inventing production deltas. This is unchanged.

4. **P2 — Overlapping navigation repeats requests and can lose cleanup ownership.**
   [render](web/app.js) lets older asynchronous handlers keep writing into the shared
   view. A late handler then invokes another render for the current URL and can
   overwrite its cleanup callback. This repeats the current page's request graph and
   can leave chart observers alive. Give each navigation ownership of its result and
   cleanup, and cancel superseded requests through the existing signal support in
   [api.js](web/api.js). This is a static finding; no browser race reproduction was run.

5. **P2 — An exact team search can return only a prefix match.**
   [TeamPrefix](internal/rank/rank.go) sorts and truncates hits before the API can move
   exact names first. Reproduction: team `abc` has 1 point, `abcd` has 100;
   `/v1/search?type=team&q=abc&limit=1` returns `abcd`, without an exact team hit.
   The `limit * 20` candidate cap also means broad-prefix results are not necessarily
   the strongest matches across the entire prefix range. Preserve exact-name lookup
   in the index before limiting candidates. This is unchanged.

6. **P3 — Chart presentation changes repeat unchanged data work.**
   [historyCard](web/views.js) calls its fetch path when switching rate or line/bar
   presentation even when granularity and time range are unchanged. Browser caching
   can avoid network transfer, but JSON parsing and series preparation still repeat.
   Reuse the loaded series for presentation changes and fetch again when the snapshot,
   granularity, or range changes. This is a static finding, left unchanged.

**Implemented performance improvements**

| Path | Change | Local result |
| --- | --- | --- |
| REST/MCP movers | Select ranks and deltas first; build full views only for the returned rows. REST and MCP share the selection. | REST teams 2.069 → 0.106 ms; REST donors 1.532 → 0.108 ms; MCP teams 3.097 → 0.120 ms; about 90% fewer allocated bytes for REST. |
| Single-membership donor history | Reuse the direct member query instead of `IN` plus aggregation. Apply the same shortcut to streak-day reads. | Hourly 1.634 → 1.030 ms; daily 553 → 343 µs; monthly 23.18 → 15.75 µs; streak days 410 → 251 µs. Weekly was roughly unchanged. |
| JSON/MCP/SVG gzip | Use gzip from the already-installed `klauspost/compress` dependency at BestSpeed. | 10/100/1,000-row JSON: 15.67/72.01/786.47 → 11.09/43.38/415.44 µs. Compressed bytes also fell from 531/3,844/38,794 to 521/3,440/35,004. |
| Static assets | Serve the existing bytes with `bytes.NewReader`, removing the whole-file string copy. | `/views.js`: 72.88 → 45.95 µs; 730,246 → 525,460 allocated bytes/request. |
| Ingest scratch buffers | Retain 12.5% capacity headroom, matching the metric-window approach. | 24 growth steps at 2.7M entities: 1.361 GB → 63.8 MB allocated; helper runtime 39.5 → 1.34 ms. |
| Rate calculations | Return immediately for zero production; avoid searching cycle history for entities older than the retained window. | A pair of rate calculations: active 61.61 → 47.10 ns; idle 58.54 → 3.93 ns. |
| Rank ties | Check adjacent scores before searching both tie boundaries. | Unique scores: about 140 → 5.6 ns. Wide ties were about 2% slower. |
| Detail-page loading | Render milestone cards immediately and fill the optional goal projection asynchronously. | History, roster and rivals requests no longer wait for the goals response. No end-to-end browser timing claimed. |

The scratch-buffer change avoids repeated large allocations while ingest holds the
API write lock. It retains roughly 12.5% extra scratch capacity; it does not remove
the write lock or establish a new end-to-end ingest latency.

**Other fixes made during the review**

- **SQLite connections:** initialization previously configured only the first of eight
  readers. The other seven reproduced a zero busy timeout, default temporary storage,
  and default cache settings. DSN pragmas now initialize every physical connection.
  Reader page-cache allowances total 64 MiB, with a separate 64 MiB writer allowance.
- **Agent reconnect loop:** an agent without rental management started a watcher that
  returned immediately and ended the session. The optional watcher now starts only
  when configured.
- **Bot freshness:** status/search responses now invalidate older cached statistics.
  Late responses from an older snapshot cannot replace the newer cache.
- **Roster sorting:** `today` now uses today's production, and `per_day` uses the
  displayed rolling 24-hour rate rather than the seven-day rate.
- **Concurrent reads:** `previous_at` is captured under the ingest guard. Donor/team
  HTML metadata also guards live state; static metadata stays independent of ingest.
  A race test reproduced the metadata race against the original source and passes
  with the fix.
- **Conditional HTTP requests:** weak and list ETags can take the 304 path, 304s retain
  `Vary: Accept-Encoding`, badges distinguish encoding validators, and `/v1/status`
  always returns current data rather than a 304 against an unchanged feed timestamp.
- **Validation cleanup:** removed two pre-existing copies of mutex-bearing snapshots
  in tests, allowing `go vet` to pass.

**Validation and measurement limits**

`go test ./...`, `go test -race ./...`, `go vet ./...`, and `git diff --check` passed.
The API/metrics race tests were repeated after the final tie optimization.
Regression checks cover history equivalence, all read-pool connections, scratch
capacity reuse, mover selection, roster sorting, conditional requests, ingest reads,
bot invalidation, nonblocking milestone rendering, and agents without rental management.

Benchmarks are local synthetic microbenchmarks on an Apple M3 Pro, Go 1.26.4. Most
reported timings are three-run medians; some ran alongside other checks and carry
scheduling noise. Movers uses 10,000 changed candidates. History uses 2,160 hourly,
730 daily, and 24 monthly buckets. The static handler benchmark includes an `httptest`
response buffer. Scratch growth uses one benchmark iteration per sample over 24
growth steps. These numbers are not production throughput, network latency, or p99
measurements, and their savings cannot be added together.

The existing roster-cache comparison fixture has only two members, below the cache
threshold, so it does not actually exercise the large-roster memoization branch.
That remains a coverage gap. No production load test or full real-corpus replay was run.

Runnable measurements:

```sh
go test ./internal/api -run '^$' -bench 'BenchmarkMovers|BenchmarkTieAt|BenchmarkGzipComparison' -benchmem -count=3
go test ./internal/store -run '^$' -bench BenchmarkSingleMemberHistory -benchmem -count=3
go test ./internal/model -run '^$' -bench BenchmarkScratchGrowth -benchmem -benchtime=1x -count=3
go test ./internal/metrics -run '^$' -bench BenchmarkWindowRates -benchmem -count=3
go test ./web -run '^$' -bench BenchmarkStaticAsset -benchmem -count=3
```
