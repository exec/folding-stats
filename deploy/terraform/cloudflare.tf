# The tunnel the site is served through.
#
# cloudflared on the n100 dials out to Cloudflare and holds four QUIC connections open;
# nothing dials in. That is why there is no port forward, no Origin CA certificate and
# no Cloudflare IP allowlist to keep current on this path — and why a flood aimed at
# foldingstats.org can no longer degrade the four other sites behind the shared DMZ.
#
# config_src = "local" is load-bearing. The ingress rules live in
# /etc/cloudflared/config.yml on the box. Setting this to "cloudflare", or adding a
# cloudflare_zero_trust_tunnel_cloudflared_config resource, would move that authority
# into the dashboard and the file on the box would stop being consulted — which is a
# silent way to lose the /relay/ route and with it every enrolled agent.
#
# The secret is deliberately not managed. Cloudflare will not read it back, so the
# provider sees "unset" on every plan and proposes writing one — an update against the
# tunnel currently carrying all of the site's traffic, to change the credential its
# four live connections were established with. That is a real risk taken in exchange for
# nothing: a tunnel secret is generated once and never rotated on a schedule. Ignoring
# it also keeps it out of terraform.tfstate, which stores such things in plaintext.
#
# If the tunnel ever genuinely needs recreating, do it with the API and re-run the
# import — the credentials file on the box has to be rewritten by hand either way.
resource "cloudflare_zero_trust_tunnel_cloudflared" "n100" {
  account_id = var.account_id
  name       = "n100-foldingstats"
  config_src = "local"

  lifecycle {
    ignore_changes = [tunnel_secret]
  }
}

locals {
  # Where a proxied CNAME points to send traffic down the tunnel.
  tunnel_target = "${cloudflare_zero_trust_tunnel_cloudflared.n100.id}.cfargotunnel.com"
}

# The apex. This record is the site.
#
# It was an A record to the WAN address until the migration; it is a CNAME to the
# tunnel now, and Cloudflare's CNAME flattening is what allows that at an apex. Proxied
# is not optional — an unproxied CNAME to cfargotunnel.com resolves to nothing a client
# can reach, so unticking it does not "bypass the CDN", it takes the site off the air.
resource "cloudflare_dns_record" "apex" {
  zone_id = var.zone_id
  name    = "foldingstats.org"
  type    = "CNAME"
  content = local.tunnel_target
  proxied = true
  ttl     = 1 # 1 means automatic, which is the only value a proxied record accepts.
  comment = "origin: N100 via cloudflared tunnel (was A ${var.origin_ip} -> DMZ CT 102)"
}

resource "cloudflare_dns_record" "www" {
  zone_id = var.zone_id
  name    = "www.foldingstats.org"
  type    = "CNAME"
  content = "foldingstats.org"
  proxied = true
  ttl     = 1
}

# A second name on the same tunnel, kept deliberately.
#
# The cache rule below is scoped to the apex hostname, so this one reaches the origin
# for every request. That makes it the way to observe what the box is actually doing —
# latency, headers, a fresh publish — without the edge answering on its behalf. It is
# also how the migration was verified before DNS was cut over.
resource "cloudflare_dns_record" "n100_direct" {
  zone_id = var.zone_id
  name    = "n100.foldingstats.org"
  type    = "CNAME"
  content = local.tunnel_target
  proxied = true
  ttl     = 1
  comment = "origin-direct: bypasses the /v1/* cache rule, for observing the box itself"
}

# The old name, kept pointing at the DMZ that still answers it with a 301.
#
# folding.exec.codes was the site's address before foldingstats.org existed. The nginx
# vhost on CT 102 returns a permanent redirect preserving the path; this record is what
# still gets anyone there. It outlives the move on purpose: a name that has been
# published anywhere is a name somebody still has in a bookmark or a chat log.
#
# Everything else in the exec.codes zone is unmanaged and must stay that way — it hosts
# around twenty unrelated services and the iCloud mail records for the domain.
resource "cloudflare_dns_record" "legacy_folding" {
  zone_id = var.legacy_zone_id
  name    = "folding.exec.codes"
  type    = "A"
  content = var.origin_ip
  proxied = true
  ttl     = 1
  comment = "retired name; DMZ 301s it to foldingstats.org"
}

# Cache policy for the public API.
#
# edge_ttl bypass_by_default means "honour the origin's Cache-Control if it sent one,
# otherwise do not cache". A fixed TTL here would be actively wrong: the origin marks
# /v1/status no-store precisely so pollers observe a publish, and caching that would
# serve every caller their own last answer while the data moved on.
#
# The origin sets Cache-Control deliberately per route — immutable for versioned assets,
# max-age-to-next-publish for data — and purges the nine hottest URLs by name after each
# ingest. So the rule's job is only to make /v1/* eligible; the origin decides the rest.
#
# This is an entrypoint ruleset: it represents the whole cache phase for the zone, so
# any rule added by hand in the dashboard will show up here as drift to be removed.
resource "cloudflare_ruleset" "cache" {
  zone_id = var.zone_id
  name    = "default"
  kind    = "zone"
  phase   = "http_request_cache_settings"

  # /badge/ is in here with /v1/ because it is the highest-fanout thing the service
  # serves and was the only one not covered. A badge is meant to be embedded in a README
  # or a forum signature, so one popular repository is a great many requests for an image
  # that changes once an hour. The origin already sends the right Cache-Control for it —
  # max-age to the next publish — so it only ever needed to be made eligible.
  rules = [{
    description = "Folding API and badges"
    enabled     = true
    expression  = "(http.request.full_uri wildcard r\"https://foldingstats.org/v1/*\") or (http.request.full_uri wildcard r\"https://foldingstats.org/badge/*\")"
    action      = "set_cache_settings"
    action_parameters = {
      cache = true
      edge_ttl = {
        mode = "bypass_by_default"
      }
      browser_ttl = {
        mode = "respect_origin"
      }
      # Return the origin's own JSON error body rather than Cloudflare's HTML page, so
      # a client parsing responses gets something it can parse when things go wrong.
      origin_error_page_passthru = true
    }
  }]
}
