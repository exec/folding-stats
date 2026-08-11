# Cloudflare, as code

The DNS records, cache rule and tunnel that put foldingstats.org on the internet. All of
it was built by hand during the move to the n100 on 10 August 2026; this directory is
the record of what was built, so it survives losing a dashboard or a memory.

It covers **Cloudflare only**. The n100's own setup — users, systemd units, cloudflared's
install and its `config.yml` — is not here, and neither are the Proxmox containers on
eqr6. `main.tf` explains why in full; the short version is that Terraform provisions
infrastructure and does not install a systemd unit, and the containers are hand-raised
pets that mostly belong to other projects.

## First run

Everything described here already exists and is serving traffic, so the first job is
adoption, not creation. `imports.tf` holds an `import` block for all six objects.

```sh
export CLOUDFLARE_API_TOKEN=...   # scoped, see below
terraform init
terraform plan                    # read this properly
```

**The plan must say `0 to destroy`.** If it proposes destroying or replacing
`cloudflare_dns_record.apex`, stop: that record is the only route to the site, and
Terraform recreating it is an outage. A replacement there means the config has drifted
from reality — fix the config to match what is live, never the other way around.

As of writing, a clean plan reads:

```
Plan: 6 to import, 0 to add, 2 to change, 0 to destroy.
```

The two changes are DNS record **comments** — one missing, one left stale by the
migration. Nothing about routing moves.

```sh
terraform apply
```

After that has run once, `imports.tf` has done its job and can be deleted. It describes
a one-time transition, not desired state.

## The token

Use a scoped API token, not the global key. The global key can do anything to any zone
on the account, including the ~20 unrelated services on `exec.codes`. This directory
needs exactly:

| Scope | Permission |
|---|---|
| Zone → foldingstats.org, exec.codes | `DNS:Edit` |
| Zone → foldingstats.org | `Cache Rules:Edit` |
| Account | `Cloudflare Tunnel:Edit` |

## State

`terraform.tfstate` is gitignored and currently local, which means it lives on one
laptop and is not shared. That is fine for one operator and wrong for two — if anyone
else ever runs this, move it to a remote backend first, or two people will race and one
will silently revert the other.

Nothing in this directory holds a credential. The tunnel secret is deliberately
unmanaged (`ignore_changes`), because Cloudflare will not read it back, so managing it
would mean every plan proposing to rewrite the credential on the tunnel that is
currently carrying all of the site's traffic — and storing it in plaintext in state for
the privilege.

## Things that will bite

**The cache ruleset is an entrypoint.** It represents the entire cache phase for the
zone, not one rule within it. A rule added by hand in the dashboard shows up here as
drift, and applying will remove it.

**`config_src = "local"` on the tunnel is load-bearing.** The ingress rules live in
`/etc/cloudflared/config.yml` on the n100. Flipping this to `cloudflare`, or adding a
`cloudflare_zero_trust_tunnel_cloudflared_config` resource, moves that authority to the
dashboard and the file on the box stops being consulted — which silently drops the
`/relay/` route and with it every enrolled agent.

**Proxied is not a CDN toggle on the apex.** It is a CNAME to `cfargotunnel.com`;
unproxied, that resolves to nothing a client can reach. Unticking it does not bypass
Cloudflare, it takes the site off the air.

## Failing back

The old path still exists: CT 126 on eqr6 is running, ingesting, and holds a current
copy of the archive. To fail back, point `cloudflare_dns_record.apex` at
`var.origin_ip` as an `A` record — the DMZ vhost for `foldingstats.org` on CT 102 is
still configured and still proxies to it. Also restore
`/etc/folding/cf.env.retired-2026-08-10` on CT 126 so the live origin is the one purging
the CDN.
