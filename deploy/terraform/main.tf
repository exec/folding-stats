# The parts of foldingstats.org that live in Cloudflare rather than in this repo.
#
# Everything here was built by hand, in a dashboard and over curl, during the move to
# the n100 on 10 August 2026. That is the reason this file exists: the DNS records are
# the only route to the site, and the cache rule is a page of settings that nothing in
# the repo encoded. Losing the account meant reconstructing both from memory.
#
# What is deliberately NOT here, so nobody goes looking:
#
#   - The n100's own configuration — the folding/foldingrelay users, the systemd units,
#     the cloudflared install and its /etc/cloudflared/config.yml. Terraform provisions
#     infrastructure; it does not install a systemd unit, and pretending otherwise with
#     remote-exec would be a worse shell script than the one we already have. That is a
#     config-management job (Ansible, or the documented sequence) and it is tracked in
#     the deploy notes instead.
#
#   - The Proxmox containers on eqr6. Most of them (auth, oxal, callfrank, dns, vault)
#     have nothing to do with this project, and the one that does — CT 126 — is now a
#     cold fallback rather than the origin. Importing twenty hand-raised pets to
#     describe one spare would be a lot of HCL guarding something we are trying to stop
#     depending on.
#
#   - The tunnel's ingress rules. See the note on the tunnel resource: this tunnel is
#     config_src = "local", and managing its config here would take that away from the
#     file on the box that is actually serving traffic.

terraform {
  required_version = ">= 1.5"
  required_providers {
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.0"
    }
  }
}

# Credentials come from the environment, never from a file in the repo.
#
#   export CLOUDFLARE_API_TOKEN=...
#
# A scoped token, not the global key. The global key can do anything to any zone on the
# account including the twenty unrelated services on exec.codes; a token can be limited
# to Zone:DNS:Edit, Zone:Cache Rules:Edit and Account:Cloudflare Tunnel:Edit on just
# the two zones below, which is the whole blast radius of this directory.
provider "cloudflare" {}

variable "account_id" {
  description = "Cloudflare account that owns the zones and the tunnel."
  type        = string
  default     = "7a216bbe6214cfd8a6ec21fee13713df"
}

variable "zone_id" {
  description = "foldingstats.org — the live site."
  type        = string
  default     = "5597e1a3ea8e2d485c27ed1eaa88317d"
}

variable "legacy_zone_id" {
  description = "exec.codes — hosts ~20 other services; only the one redirect record below is managed here."
  type        = string
  default     = "7cdde6d78b6f2ece66b2c85132d6bafe"
}

variable "origin_ip" {
  description = "WAN address of the DMZ proxy. Only the retired folding.exec.codes record still points at it."
  type        = string
  default     = "98.34.90.69"
}

# No tunnel_secret variable on purpose — see the note on the tunnel resource. Nothing in
# this directory holds a credential, which is why terraform.tfstate here is boring.
