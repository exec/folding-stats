# Offsite backups.
#
# Until 11 August 2026 there were none. The n100 had a single history.db in
# /var/backups that a deploy had dropped there as a side effect, on the same LVM volume
# as the live database — which survives a mistake but not a disk. CT 126 on eqr6 holds
# its own independently ingested copy, but that is a replica, not a backup: it protects
# against the n100 dying, not against something wrong being written and faithfully
# mirrored.
#
# R2 rather than Drive or S3, for reasons that are mostly not about price:
#
#   - Retention is a property of the bucket, declared below, instead of a cron script
#     that parses dates out of filenames. A retention script with a bug either fills
#     the disk or deletes the backups, and both are discovered on the day they matter.
#   - Egress is free and unmetered. The moment this data is needed is the worst possible
#     moment to find the download is billed or throttled.
#   - It is S3-compatible, so the client is rclone and the config is four lines.
#
# It is also free at our size — about 7 GB against a recurring 10 GB free tier — where
# Google One's floor is $1.99/month whether you store 1 GB or 99.
#
# The concentration risk is real and was accepted deliberately: Cloudflare is already
# DNS, CDN and tunnel for this site, so one account suspension would take out serving
# and recovery together. The eqr6 copy is the independent third location that makes
# that survivable, and the data is a mirror of Folding@home's own published statistics,
# so there is no confidentiality here to lose.

resource "cloudflare_r2_bucket" "backups" {
  account_id = var.account_id
  name       = "foldingstats-backups"

  # Standard, not Infrequent Access — which is counterintuitive, because IA is cheaper
  # per GB ($0.010 vs $0.015). The recurring 10 GB free tier applies to Standard only,
  # and IA adds retrieval fees and a 30-day minimum storage duration. At our size
  # Standard is free and IA would cost money to hold and money to read back.
  storage_class = "Standard"

  lifecycle {
    # A bucket holding the only offsite copy of an archive that cannot be rebuilt.
    prevent_destroy = true
  }
}

resource "cloudflare_r2_bucket_lifecycle" "backups" {
  account_id  = var.account_id
  bucket_name = cloudflare_r2_bucket.backups.name

  # Order matters, and it is the API's order rather than a tidy one. The provider
  # compares this list positionally, so listing these the other way round — expiry
  # first, as they were written — made every plan propose rewriting both rules
  # forever. Perpetual drift is worse than untidiness: it teaches whoever reads the
  # next plan to skim past it, which is where a real change goes unnoticed.
  #
  # One cosmetic diff survives that and cannot be closed from here: the API echoes an
  # empty transition object back for whichever transition each rule does not use, and
  # the provider reports removing it. Applying does not converge — the same empty
  # objects come back on the next read.
  #
  # Do NOT silence it by declaring those empty transitions in config. An empty
  # delete_objects_transition on a bucket holding the only offsite backup is not a
  # tidy no-op to guess at, and the live rules are already exactly right: verified
  # against the API as abort-after-1-day on everything and delete-after-7-days on db/.
  # A one-line known diff is the cheaper of the two risks.
  rules = [
    # Multipart uploads that died halfway still occupy billable space and are invisible
    # in a normal listing. A 7 GB archive upload over a 41 Mbit uplink has plenty of
    # opportunity to be interrupted.
    {
      id      = "abort-stale-multipart"
      enabled = true
      conditions = {
        prefix = ""
      }
      abort_multipart_uploads_transition = {
        condition = {
          type    = "Age"
          max_age = 86400 # 1 day, in seconds
        }
      }
    },

    # Database snapshots roll. Each one is a complete ~166 MB copy of a database that is
    # itself derived from raw/, so keeping many is paying to store the same answer over
    # and over. A week is enough to notice corruption and step back behind it.
    #
    # There is no expiry rule for raw/ anywhere in this file, on purpose. That archive is
    # the one asset that cannot be reconstructed — history accrues in real time, so a gap
    # in it is permanent — and the uploader uses `rclone copy` rather than `sync` so that
    # thinning the local copy never propagates a deletion up here.
    {
      id      = "expire-db-snapshots"
      enabled = true
      conditions = {
        prefix = "db/"
      }
      delete_objects_transition = {
        condition = {
          type    = "Age"
          max_age = 604800 # 7 days, in seconds
        }
      }
    },
  ]
}

output "backup_bucket" {
  description = "Bucket the n100's backup timer writes to."
  value       = cloudflare_r2_bucket.backups.name
}

# The credentials are NOT managed here, deliberately.
#
# R2 does not issue S3 keys; it derives them from a Cloudflare API token — the access
# key is the token's id and the secret is the SHA-256 of its value, which is shown once
# at creation and never again. Managing the token in Terraform would therefore put a
# live credential in terraform.tfstate in plaintext, to save one API call that happens
# once in the life of the bucket.
#
# The token in use is account-owned, named "foldingstats-backups n100", and scoped with
# the bucket-level permission groups (Workers R2 Storage Bucket Item Read + Write) to
# this bucket alone. Recreate it the same way if it is ever lost: a token that can write
# every bucket in the account is a strictly worse thing to leave on a machine.
