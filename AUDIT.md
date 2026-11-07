# Security audit — foldingstats.org

**7 November 2026.** White-box audit of the live service: the `foldingd` binary on
the n100, fronted by Cloudflare Tunnel.

- **Audited source:** `origin/main` @ `2d9f767`. The deployed binary (built
  4 November 2026, Go 1.26.4) is from a closely related tree — its `/openapi.json`
  differs by one `/v1/changes` description line — so every finding below was
  validated dynamically against the live service, not only against source.
- **Result: no exploitable vulnerability found.** One unambiguous code defect
  was found and is fixed alongside this report; everything else is operational
  follow-up, ordered by practical severity in [Findings](#findings).

## Scope and method

The target was the service, not the host. SSH was used as `dylan` only to reach
the origin directly (a port-forward to `127.0.0.1:8080`) and to observe service
identity; no elevated privileges were used. Two paths were tested — through
Cloudflare at `foldingstats.org`, and directly at the origin with
`Host: foldingstats.org` and `X-Forwarded-Proto: https` (bypassing the canonical
redirect).

Kept out of scope, as instructed: the relay (`foldingrelay.service` is disabled;
`/relay/*` through the tunnel answers 502 and was left that way), the Discord bot
and the Vast agent (not running on this host), and host-level audit.

Running units of this system on the box: `foldingd` and `cloudflared` only.
`foldingd` runs under a hardened systemd unit (`NoNewPrivileges`,
`ProtectSystem=strict`, `MemoryDenyWriteExecute`, and more). `cloudflared`'s own
metrics and pprof listener on `127.0.0.1:2000` is localhost-only and belongs to
the host, not this service.

Source review covered the full HTTP surface: the server and handlers, badge SVG
generation, SEO meta rendering, the MCP JSON-RPC endpoint, compression, the
store's SQL construction, feed parsing, the web shell, and the frontend's DOM
sinks. Dynamic probes covered injection, validation, cache behaviour, method
handling, CORS, host-header handling, and expensive-path costs.

## The fix included with this report

`/v1/search` accepted any `?type=` value: `?type=bogus` returned an empty 200
rather than a 400, contradicting both the strict-validation design documented on
`checkQuery` in `internal/api/server.go` and the `enum: [team, donor]` the
OpenAPI spec already promised. Every other parameter on every route is validated
or refused; this was the one exception. It is now refused up front, in the same
`badRequest` style the surrounding code uses, leaving the empty value meaning
"both kinds" exactly as before:

```diff
 	kind := r.URL.Query().Get("type")
+	if kind != "" && kind != "donor" && kind != "team" {
+		return nil, nil, badRequest("type must be donor or team")
+	}
```

`go test ./...` (16 packages) and `go vet` pass.

## What was verified

### Injection resistance, against real hostile names

Donor names are attacker-influenced upstream text, and the corpus contains real
hostile-looking names — including a donor actually named
`<script>alert('test')</script>`, another named
`<b>https://www.morbus-crohn-news.de</b>`, donors with leading tabs and newlines
(`\terrabyte`, `\noam`), and one whose entire name is a non-breaking space. All
rendering surfaces were tested against them:

- **HTML donor pages** — the name lands in `<title>`, `description`, `og:*`, and
  the crawlable summary, HTML-escaped in every position (the script-tag donor's
  page contains no raw `<script>` in its head).
- **Badges** — escaped in `aria-label`, `<title>`, and text segments; and
  `badgeText` in `internal/api/badge.go` additionally strips bidi overrides,
  zero-width characters, and other C0 controls, and truncates at 20 runes.
- **JSON** — names ride unescaped with `SetEscapeHTML(false)` under
  `application/json`, which is correct for the content type; the browser-side
  code renders every name through `textContent`, never `innerHTML`.
- **MCP prose** — tool output embeds names as plain text.
- **Headers** — all response headers are server-generated; a `%0d%0a` crafted
  into the query stays percent-encoded in the `Location` of the frontend's
  moved-path redirect.
- **SQL** — fully parameterized, with table and column names interpolated only
  from internal constants.
- **Paths** — 8,000-character names, `%2F`, null bytes, overlong UTF-8, and
  `../` sequences all answer clean JSON 404/400s; the mux's own path cleaning
  answers a same-origin 307 for `/v1/countries/../../etc`.

The frontend's one `innerHTML` path (blog posts) renders repo-authored markdown
through goldmark with raw HTML disabled; chart tooltips escape labels and
colours; and `fah.js` guards machine messages against prototype pollution, with
tests.

### Validation, cache, and DoS posture

Probed live, all as documented:

- Unknown and repeated parameters, negative or zero `per_page`, `page` at
  `MaxInt64`, bad `sort` values, history ranges beyond the per-granularity caps,
  out-of-window `since`, `within`/`limit` outside bounds, and malformed
  `from`/`to` timestamps all return 400 with a JSON problem body.
- MCP accepts malformed JSON, batch arrays, null bodies, and over-limit
  payloads gracefully (its 1 MB `MaxBytesReader` holds); tool limits are
  clamped.
- `Vary: Accept-Encoding` with encoding-specific ETags; immutable versioned
  assets where `?v=` matches the build stamp and `no-cache` where it does not;
  canonical-host redirect on the frontend; working 304s; and no cache-poisoning
  vector found — CORS is uniformly `*`, and nothing unkeyed varies a response.
- Expensive paths measured at the origin: the widest donors (10,426 teams)
  answer in 20–30 ms, a 90-day hourly project history in 32 ms, an 884k-member
  team's roster in 31 ms, and a 6-day `/v1/changes` fan-out in 136 ms. Twenty
  concurrent workers × 5 mixed requests completed cleanly in 0.4 s.
- Slowloris defenses are configured (10 s header, 30 s read, 60 s write, 120 s
  idle timeouts). The origin speaks HTTP/1.1 only — it refused h2c
  prior-knowledge — so the HTTP/2-server stdlib advisories are not reachable on
  this path today. A 700 KB `Range` header parsed in 117 ms (Go's 1 MB header
  cap bounds it).
- `/debug/*`, `/metrics`, `/admin`, `/.env` all answer 404 on the public port.
- Upstream feed parsing is bounded (name length ceilings, accumulation caps,
  strict numeric anchoring) and has fuzz tests.
- Secrets are root-only readable (`/etc/folding/cf.env` is 0600 root:root,
  read by systemd before dropping privileges).

## Findings

Ordered by practical severity. None of these blocks anything.

1. **Rebuild with a current Go and dependency set.** The deployed binary is
   Go 1.26.4; `govulncheck` against this tree finds stdlib fixes through
   Go 1.26.10 — `net/http` advisories (fixed 1.26.9), `crypto/x509` (1.26.10),
   and `crypto/tls` / `net/url` / `encoding/asn1` (1.26.5–1.26.9). On this
   deployment the HTTP/2-server ones are unreachable (origin is HTTP/1.1) and
   the Range-header one is negligible (measured; the 1 MB header cap bounds
   it); the client-side ones affect the hourly outbound TLS paths to
   `apps.foldingathome.org` and the Cloudflare purge API.
   `klauspost/compress` should bump to ≥ v1.20.2 — GO-2026-6865, a zstd
   decompression panic, is reachable through the archive read path on a corrupt
   local file. `gorilla/websocket` ≥ v1.5.3 (GO-2026-6278) matters for the
   relay/bot/agent binaries when next built; `foldingd` does not link it.

2. **Consider binding the origin to loopback.** `foldingd` listens on
   `0.0.0.0:8080`, so LAN clients bypass every Cloudflare protection. Behind
   home NAT this is modest, but `127.0.0.1:8080` would close it if the
   tunnel's service URL is localhost (verify against `/etc/cloudflared/config.yml`,
   unreadable as `dylan`). This also stops the frontend's `X-Forwarded-Proto`
   trust from applying to non-cloudflared sources.

3. **Restore the legacy hostname's redirect.** The Terraform in
   `deploy/terraform/cloudflare.tf` documents `folding.exec.codes` as 301ing to
   foldingstats.org via the DMZ; live, it serves an unrelated PHP file-upload
   app on every path (200, catch-all). Old links land on a file-upload page —
   a soft phishing risk on the site's former name — and `.codex/config.toml`
   still points an MCP client at `folding.exec.codes/mcp`. Restore the 301 or
   retire the record. (The upload endpoint was exercised gently; every path
   returns the form page and nothing indicates uploads persist or execute.)

4. **Decide about the CDN's Python-urllib ban.** Cloudflare answers
   `Python-urllib/*` with 403 error 1010 ("banned your access based on your
   browser's signature"). A reader following the README's free-API invitation
   with bare `urllib.request.urlopen()` gets refused until they set a
   User-Agent. If that is Browser Integrity Check rather than a deliberate rule,
   it is worth lifting for a site that invites programmatic use.

5. **Optional hardening.** No CSP, `X-Content-Type-Options`, or
   `frame-ancestors` is sent on HTML; rendering discipline (textContent
   everywhere, escaped charts, repo-authored markdown for posts) keeps risk low,
   but a modest CSP would add defense-in-depth against future frontend
   regressions. Separately, the MCP JSON parse error discloses the internal
   type name `api.mcpRequest` — negligible, standard Go. Note `/v1/*` answers
   on any Host that reaches the origin (the canonical redirect applies only to
   frontend routes) — harmless for a public read-only API, just an asymmetry.

6. **Keep deploys auditable.** The deployed binary's `/openapi.json` differs
   from `HEAD`, so source and binary have drifted slightly; building from tagged
   commits would keep a white-box audit exactly white-box.

## Not tested

- No sustained load testing — the repository's own `cmd/loadramp` has measured
  the ceiling (~28,000 req/s); this audit only did light concurrency.
- Relay, bot, and agent code was reviewed but not exercised (not running on
  this host; the relay was to stay disabled, and did).
- Upstream feed compromise was considered (parse bounds above) but not
  simulated.
- The Cloudflare edge and cloudflared's QUIC path are Cloudflare's.
- No host-level checks (out of scope by instruction).
