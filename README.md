# folding-stats

Folding@home donor and team statistics, with a free public API.

**Live at [folding.exec.codes](https://folding.exec.codes)** — no key, no account, no rate limit, no challenge page.

Folding@home publishes cumulative totals as two large text files. Everything anyone
actually wants — rates, rankings, history, per-team breakdowns — has to be derived by
comparing one snapshot against the next. This does that work once, in public, and gives
it away.

Single Go binary. No build step, no `node_modules`, no CDN, no database server.

---

## Contents

- [What it does](#what-it-does)
- [Quick start](#quick-start)
- [API](#api)
- [How it works](#how-it-works)
- [What it costs](#what-it-costs)
- [Deploying](#deploying)
- [Development](#development)
- [License](#license)

---

## What it does

- Tracks **every** donor and team, not a sampled top-N: ~2.1 M donors across ~130 k teams.
- Updates within about a minute of upstream publishing, roughly hourly.
- Serves the whole site and API from an in-memory snapshot — median response 0.10 ms.
- **Donors are their own entity.** If you fold for three teams you are one donor whose
  totals are the sum, ranked against every other donor, with the per-team breakdown in
  the same response. Most team-oriented stats treat you as three separate people.
- Charts are client-rendered from JSON. Nothing generates a PNG per team per cycle.

### Field names say what they mean

The figure conventionally labelled "24hr Avg" on stats sites is a **seven-day moving
average**. Here it is `points_per_day_7d_avg`, because a field name should explain
itself to somebody reading the API cold. The formula was verified arithmetically against
three published values before any of this was written: rounding the seven-day total to
the nearest point reproduces all three, truncating reproduces none.

Anything bucketed by calendar day or month is explicitly UTC and says so.

---

## Quick start

```sh
go build ./cmd/foldingd
./foldingd -dir ./data
```

That is the whole thing. It fetches the upstream feeds into `./data/raw`, builds
`./data/history.db`, and serves on `:8080`.

The first run takes about 20 seconds to build identity for 2.7 M members; subsequent
cycles apply in ~1.5 s.

### Flags

| flag | default | meaning |
|---|---|---|
| `-dir` | `data` | archive + database directory |
| `-addr` | `:8080` | HTTP listen address |
| `-poll` | `10m` | upstream poll interval (see [Adaptive polling](#adaptive-polling)) |
| `-no-fetch` | off | follow an archive somebody else is filling; never contacts upstream |
| `-user-agent` | — | **set this in production**, with a contact URL |
| `-compact-after` | `2160h` | age at which raw deltas roll up to daily |
| `-keep-daily` | `17520h` | age at which daily rollups collapse to monthly |
| `-v` | off | verbose logging |

### Running a read-only replica

`-no-fetch` serves an archive maintained elsewhere and scans it for new snapshots on the
same schedule the fetcher would have used. A second instance therefore costs upstream
nothing, which is the entire reason to run one that way rather than pointing another
collector at their origin.

---

## API

Every response carries a `snapshot` block describing freshness, then the data.

```jsonc
{
  "snapshot": {
    "at": "2026-08-03T06:31:11Z",          // upstream publish time this reflects
    "next_expected_at": "2026-08-03T07:31:20Z",
    "stale": false,
    "server_time": "2026-08-03T06:42:07Z", // compare against this, not your own clock
    "interval_sec": 3609,                  // measured, not assumed
    "interval_measured": true,
    "avg_window_complete": true,
    "history_span_sec": 604800
  },
  "data": { }
}
```

### Routes

| method | path | notes |
|---|---|---|
| `GET` | `/v1/status` | snapshot and corpus size. `no-store` — never cached |
| `GET` | `/v1/summary` | project-wide totals |
| `GET` | `/v1/summary/history` | project production over time |
| `GET` | `/v1/teams` | team leaderboard, paginated |
| `GET` | `/v1/teams/{id}` | one team |
| `GET` | `/v1/teams/{id}/members` | roster, `?active_only=true` |
| `GET` | `/v1/teams/{id}/history` | `?granularity=hourly\|daily\|monthly` |
| `GET` | `/v1/donors` | donor leaderboard, paginated |
| `GET` | `/v1/donors/{name}` | one donor, `?sort=production` |
| `GET` | `/v1/donors/{name}/teams` | full team list, paginated |
| `GET` | `/v1/donors/{name}/history` | `?team_id=` to scope to one team |
| `GET` | `/v1/search` | `?q=` name prefix, exact name, or team ID |
| `GET` | `/v1/posts`, `/v1/posts/{slug}` | site announcements |

### Notes for clients

- **Cache against `next_expected_at`** rather than polling. It is derived from the
  measured interval, not from an assumed hour.
- **Conditional requests are ~6× cheaper** and return no body. Send `If-None-Match`;
  data only changes hourly, so most polls are a 304.
- **Compare timestamps against `server_time`**, not your own clock. Unsynced client
  clocks are routinely minutes out, and every relative figure derived from one is wrong
  by exactly that much.
- **Names are raw upstream text.** They contain tabs, newlines and non-ASCII.
  URL-encode them in paths.
- **A UTC day has ~23.94 publishes, not 24.** See [Cadence](#cadence-is-measured-not-assumed).
- Responses over 1 KB are gzipped and `Vary: Accept-Encoding`. The ETag differs by
  encoding.

---

## How it works

### Everything is derived by differencing

The upstream feeds carry only cumulative `score` and `wu` per entity. Every rate, rank
and delta on the site comes from comparing successive snapshots. A donor's *first*
sighting is never production — that row carries a lifetime total accumulated before we
existed.

### Storage grain is `(name, team)`

That is the feed's native grain. Donor aggregation is a read-time view, never a schema
commitment — which is what makes it cheap to present a donor as one entity while still
answering per-team questions from the same rows.

Donor names are **not unique**. `Anonymous` appears on nearly six thousand teams. Where
a name looks shared, the site says so rather than presenting the aggregate as a person.

### Sparse deltas

Only ~0.04% of donors produce in a given hour. Each cycle is stored as a short list of
`(entity, delta)` pairs and the rolling windows are maintained incrementally — add the
entering cycle, subtract the ones that aged out.

Seven days of history costs **~8 MB** this way. Keeping 168 full snapshots of 2.7 M
`int64` scores would be ~3.6 GB.

### Memory

The expensive part is allocated once and does not grow with history:

| | |
|---|---|
| member rate windows (5 × `int64` × 2.7 M) | 108 MB |
| team rate windows | 5 MB |
| delta ring, per cycle | 0.05 MB |
| **steady state** | **~1.0–1.4 GB** |

`GOMEMLIMIT` is the setting that matters — it makes Go collect harder rather than grow.

### Cadence is measured, not assumed

Upstream does not publish on a wall clock. Measured over consecutive publishes the
interval runs **3606–3613 s** — always longer than an hour, creeping later each cycle,
so the publish time sweeps a full hour about every two weeks.

Three consequences:

1. A UTC day has ~23.94 publishes. Usually 24 hourly buckets, occasionally 23.
2. Nothing can be scheduled against `at + 1h`. `next_expected_at` comes from the median
   of the last 24 observed intervals, clamped to 50–75 min. The **median**, not the
   mean: one missed publish is a single two-hour interval that would drag a mean past
   the truth and hold it there for a day.
3. Because the interval is never *under* an hour, two publishes cannot collide in one
   bucket.

Both feeds come from one generation run — they carry an identical `feed_timestamp`, and
the user file's `Last-Modified` trails the team file's by ~63 s only because it is 66 MB
against 3.8 MB and takes that much longer to write.

### Adaptive polling

A fixed tick spreads the gap between upstream publishing and us capturing it uniformly
across the interval — five minutes on average at a 10-minute poll. So the poll rate
follows the prediction: slow while nothing is due, every 60 s once the window opens.
Conditional GETs make the extra polls nearly free.

Measured capture lag: **17 seconds** from publish to serving.

### Timezones

Two kinds of timestamp, treated differently on purpose:

- An **instant** — a publish time, an hourly point — renders in the reader's own
  timezone. Both renderings name the same moment.
- A **calendar bucket** — a day, a month — is a named period, not a moment. The server
  aggregates one set of rollups on UTC boundaries for everyone; there is no per-viewer
  "July". Rendering its start instant through a timezone does not translate the name,
  it changes it: `2026-07-01T00:00:00Z` formats as "Jun 30" west of Greenwich.

### Caching: three different answers

| what | policy | why |
|---|---|---|
| `index.html` | `no-cache` | carries the build stamp that invalidates everything else |
| `/app.js?v=<hash>` | `immutable`, 1 year | every internal module specifier is stamped with one content hash, so a deploy changes every URL in the graph at once and a browser can never mix versions |
| `/v1/status` | `no-store` | it answers "has anything changed yet"; a cached answer is not cheap, it is wrong |
| other `/v1/*` | `max-age` to next publish, floor 30 s | data is genuinely immutable between cycles |

---

## What it costs

### Throughput

Measured on a 4-core / 4 GB container, load generator pinned to separate cores, 60 s per
rate, real donor and team lookups:

| offered | achieved | errors | svc p50 | svc p99 | watcher |
|---|---|---|---|---|---|
| 20,000/s | 19,995 | 0 | 0.10 ms | 0.61 ms | 0.20 ms |
| 24,000/s | 23,993 | 0 | 0.10 ms | 0.68 ms | 0.13 ms |
| 28,000/s | 27,992 | 0 | 0.11 ms | 0.92 ms | 0.14 ms |
| 32,000/s | 31,992 | 0 | 0.11 ms | 4.63 ms | 0.16 ms |
| 48,000/s | 47,987 | 0 | 14.82 ms | 37.03 ms | 15.76 ms |

**Zero errors at every rate.** The failure mode is latency, not refusal. ~28,000 req/s
is the comfortable ceiling (p99 under a millisecond); the knee is around 32,000. The run
ended because the *generator* could not produce 52,000 req/s, not because the service
failed.

`cmd/loadramp` is the tool. It is an **open loop** — requests are scheduled at a target
rate and sent whether or not earlier ones came back — and it measures latency from when
each request was *due*, not when it went out. Measuring from the send time hides
saturation: under overload the generator falls behind, the slowest requests are never
issued, and latency looks fine right up until the service falls over.

### Bandwidth

The average response over a realistic endpoint mix is 3,839 bytes uncompressed. Only 35%
of responses clear the 1 KB compression threshold, but they carry most of the bytes, so
the mix average falls to 1,017 bytes.

| | uncompressed | as served |
|---|---|---|
| 24,000 req/s | 0.80 Gbit/s | 0.26 Gbit/s |
| 32,000 req/s | 1.06 Gbit/s | 0.34 Gbit/s |

Compression also made the service *faster* — p99 at 24,000 req/s went 6.89 ms → 2.51 ms.
The CPU spent compressing is repaid several times over by the syscall and copy work
avoided in writing smaller bodies to sockets.

Small responses are deliberately **not** compressed: over half of real traffic is
sub-kilobyte, and gzipping those spends CPU on every request to save a fraction of one
packet.

### Being a good citizen of someone else's server

The feeds come from `apps.foldingathome.org`, and `cf-cache-status` reports `DYNAMIC` —
the CDN is not absorbing these requests, so every one reaches their origin.

| | per cycle | per day |
|---|---|---|
| uncompressed | 70.1 MB | 1.68 GB |
| gzip | 34.7 MB | 833 MB |

**Poll frequency is not the cost.** Polls are conditional GETs answered `304` with no
body; a payload crosses the wire only when the content changes. Download volume equals
publish volume — one copy of each published file — whether you check every ten minutes
or every three hours.

`429` and `503` are honoured properly: they return a `BackoffError` carrying
`Retry-After`, which overrides the poll schedule entirely, including the once-a-minute
cadence near a predicted publish. An absent or unparseable header errs long, because the
point is to stop asking.

**Set `-user-agent` with a contact URL.** Feed operators reasonably expect to identify
whoever is pulling from them.

---

## Deploying

One static binary, no runtime dependencies:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" ./cmd/foldingd
```

The sqlite driver is `modernc.org/sqlite` (pure Go), so `CGO_ENABLED=0` produces a fully
static binary with no glibc coupling.

A systemd unit wants, at minimum:

```ini
[Service]
ExecStart=/usr/local/bin/foldingd -dir /var/lib/folding -addr 0.0.0.0:8080 \
          -user-agent "yourthing/1.0 (+https://example.com; F@H stats mirror)"
Restart=always
Environment=GOMEMLIMIT=2500MiB
MemoryHigh=2800M
MemoryMax=3400M
```

`GOMEMLIMIT` is load-bearing; the systemd limits are the backstop. An OOM kill mid-ingest
abandons a cycle, and **cycles are not replayable** — upstream overwrites each file, so a
gap in the archive is permanent.

Behind a reverse proxy, pass the origin's headers through untouched. It sets
`Cache-Control` deliberately per route and `Vary: Accept-Encoding` with encoding-specific
ETags; overriding either breaks correctness in ways that only show up hours later.

### Writing posts

Markdown in `content/posts/`, compiled into the binary:

```markdown
---
title: Something happened
date: 2026-08-03
summary: One line for the listing.
draft: false
---
```

The date prefix on the filename orders files on disk and is stripped from the URL.
Frontmatter errors fail the build rather than silently dropping the post.

---

## Development

```sh
go test ./...          # unit, corpus and fuzz tests
go vet ./...
```

| command | purpose |
|---|---|
| `cmd/foldingd` | the server |
| `cmd/archiver` | archive upstream feeds only, no API |
| `cmd/gendata` | synthetic corpora for testing at scale |
| `cmd/loadtest` | fixed-concurrency latency profile |
| `cmd/loadramp` | open-loop ramp; finds where it breaks |

`internal/gen` builds a few months of plausible history from a wordlist, which is how
the storage and query paths were exercised at full scale before any real data existed.

[`DESIGN-BACKEND.md`](DESIGN-BACKEND.md) is the long-form design record, including a
table of every bug found during deep testing and what it took to surface each one. Most
of them needed real data, a second process, or concurrency — the kind that fixtures do
not catch.

---

## Not affiliated

This project is not run by Folding@home, Stanford, Washington University, or any other
statistics site. Data comes from the official Folding@home feeds. Nothing here is
scraped from anyone's pages.

## License

MIT. See [LICENSE](LICENSE).

If your team would rather run its own instance, or point it at a different subset of the
data, or fork it somewhere I have not thought of — please do.
