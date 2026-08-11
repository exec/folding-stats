# Adopting what already exists, rather than building it again.
#
# Every object described in cloudflare.tf is live and serving traffic right now. Without
# these blocks the first apply would not reconcile with reality — it would try to
# *create* an apex record that already exists, and the interesting failure is not the
# error, it is the case where it succeeds: Terraform replacing the one DNS record that
# is the entire route to the site.
#
# So the order is import first, plan until the plan is empty, and only then is this
# directory telling the truth. `terraform plan` with these present is safe and makes no
# changes; it reports what adoption would do.
#
# Once `terraform apply` has run once and the state file holds all six, this whole file
# can be deleted. It describes a one-time transition, not the desired state.

import {
  to = cloudflare_zero_trust_tunnel_cloudflared.n100
  id = "7a216bbe6214cfd8a6ec21fee13713df/b09badcb-55e4-47e2-9bf8-d953bfe96467"
}

import {
  to = cloudflare_dns_record.apex
  id = "5597e1a3ea8e2d485c27ed1eaa88317d/58939a0263243350b42024ac668f1716"
}

import {
  to = cloudflare_dns_record.www
  id = "5597e1a3ea8e2d485c27ed1eaa88317d/9be02e332e3137502dbef86cf4911cf2"
}

import {
  to = cloudflare_dns_record.n100_direct
  id = "5597e1a3ea8e2d485c27ed1eaa88317d/651d6261199d4eb51632775531aa2ba9"
}

import {
  to = cloudflare_dns_record.legacy_folding
  id = "7cdde6d78b6f2ece66b2c85132d6bafe/0d63b2fd0241600498700667ffd8b841"
}

import {
  to = cloudflare_ruleset.cache
  id = "zones/5597e1a3ea8e2d485c27ed1eaa88317d/d9cb6df5b4fd4f98a7bc064ba71c63ab"
}
