# Offsite backups

Nightly, from the n100 to Cloudflare R2. Until 11 August 2026 there were none: the only
copy of `history.db` outside the live directory was one a deploy had dropped in
`/var/backups` as a side effect, on the same volume as the original — which survives a
mistake but not a disk.

## What runs

| | |
|---|---|
| `folding-backup.sh` | → `/usr/local/bin/`, root:root 0755 |
| `folding-backup.service` | → `/etc/systemd/system/`, oneshot, runs as `folding` |
| `folding-backup.timer` | → `/etc/systemd/system/`, daily at 04:17 UTC ±15m, `Persistent=true` |
| `/etc/folding/rclone.conf` | the R2 credentials, root:folding 0640 — **not in this repo** |

The bucket and its retention rules are Terraform, in `../terraform/backups.tf`.

## What it copies, and how far back

**`db/`** — a consistent snapshot of `history.db`, taken with `VACUUM INTO` so the
service never stops, plus `state.json`. Expired after 7 days by an R2 lifecycle rule
rather than by anything in the script: a retention script with a bug either fills the
bucket or deletes the backups, and both are discovered on the day they matter.

**`raw/`** — the upstream archive, never expired. It is the one asset that cannot be
rebuilt, because history accrues in real time and a gap in it is permanent. Uploaded
with `rclone copy`, never `sync`: sync mirrors deletions, so the day the archive thinner
drops old snapshots locally, sync would drop them from the only offsite copy too.

`/var/lib/foldingrelay/machines.json` is deliberately absent. It holds relay enrolment
keys, which are bearer credentials; an enrolment is cheap to redo and a leaked key is
not.

## Restoring

```sh
# The newest database snapshot.
rclone --config /etc/folding/rclone.conf lsf r2:foldingstats-backups/db/ | sort | tail -4
rclone --config /etc/folding/rclone.conf copyto \
  r2:foldingstats-backups/db/history-<stamp>.db /var/lib/folding/history.db

# Ownership matters: foldingd runs as `folding` and cannot open a root-owned database.
systemctl stop foldingd
chown folding:folding /var/lib/folding/history.db
systemctl start foldingd
```

Check it before trusting it — `sqlite3 <file> 'PRAGMA integrity_check;'` and compare the
cycle count against what the running service reports at `/v1/status`.

The archive restores the same way (`rclone copy r2:foldingstats-backups/raw /var/lib/folding/raw`),
but it is the slow half: it is tens of gigabytes against a 41 Mbit/s uplink, so plan in
hours. The database alone is what gets the site back.

## Watch the size

The archive grows at a measured **748 MB/day**. `foldingd -keep-raw` thins snapshots
older than 90 days to one per day, which would cap it near 67 GB — but see
the note below: that pass has never actually run.

R2 bills $0.015/GB/month past a recurring 10 GB free tier, and egress is free. At the
capped size that is well under a pound a month. Unthinned it is about 273 GB a year, and
the bill grows with it.

**The daily maintenance pass has never run once.** `maintain()` in `cmd/foldingd/main.go`
uses a bare `time.NewTicker(24 * time.Hour)` with no run at startup, so the timer resets
every time the process restarts — and deploys restart it several times a day. Nothing in
the journal has ever logged `compacted` or `archive pruned`. Until that is fixed, both
`-compact-after` and `-keep-raw` are settings that describe an intention rather than a
behaviour.
