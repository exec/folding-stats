#!/bin/bash
# Offsite backup of the folding stats data to Cloudflare R2.
#
# Two things with two different retentions, because they are two different kinds of
# asset:
#
#   db/   a consistent snapshot of history.db, uploaded under a dated name. Expires
#         after 7 days by an R2 lifecycle rule, not by anything in this script — a
#         retention script with a bug either fills the bucket or deletes the backups,
#         and both are found out on the day they matter. history.db is derived from
#         raw/, so keeping many copies is paying to store the same answer repeatedly.
#
#   raw/  the upstream archive. Never expired, and uploaded with `rclone copy` rather
#         than `sync`: history accrues in real time so a gap in it is permanent, and
#         sync would mirror deletions — the day the archive thinner drops snapshots
#         older than 90 days locally, sync would drop them from the only offsite copy
#         too, turning a backup into a replica of a decision we might regret.
#
# /var/lib/foldingrelay is deliberately not backed up. machines.json holds relay
# enrolment keys, which are bearer credentials; an enrolment is cheap to redo and a
# leaked key is not. Excluding it by simply never naming it here is more durable than
# an --exclude flag someone might reorganise away.

set -euo pipefail

DATA=/var/lib/folding
# Overridable so the whole path — snapshot, verify, transfer, layout — can be exercised
# against a local directory before R2 credentials exist, and afterwards without
# spending a real upload to find out a quoting mistake broke it.
BUCKET="${FOLDING_BACKUP_DEST:-r2:foldingstats-backups}"
TS=$(date -u +%Y%m%dT%H%M%SZ)

STAGE=$(mktemp -d /var/tmp/folding-backup.XXXXXX)
trap 'rm -rf "$STAGE"' EXIT

# VACUUM INTO copies a live WAL-mode database consistently without stopping the writer.
# A plain cp of history.db while foldingd is running can tear across the WAL and
# produce a file that opens fine and is wrong.
sqlite3 "$DATA/history.db" "VACUUM INTO '$STAGE/history.db'"

# Verify before uploading, never after. A corrupt backup is worse than no backup: it is
# the same outage, plus the confidence that stopped you looking for another copy.
check=$(sqlite3 "$STAGE/history.db" 'PRAGMA integrity_check;')
if [ "$check" != "ok" ]; then
	echo "integrity check failed (${check}); refusing to upload" >&2
	exit 1
fi
cycles=$(sqlite3 "$STAGE/history.db" 'SELECT count(*) FROM cycles;')
echo "snapshot ok: ${cycles} cycles, $(stat -c %s "$STAGE/history.db") bytes"

cp -a "$DATA/state.json" "$STAGE/state.json"

# --bwlimit because this shares one 41 Mbit/s uplink with the site itself, and the
# tunnel carries every response over it. The first raw/ upload is ~6 GB; unthrottled it
# would saturate the link for half an hour and make the site look broken from outside.
RC=(rclone --bwlimit 3M --stats-one-line --stats 30s)

"${RC[@]}" copyto "$STAGE/history.db" "$BUCKET/db/history-$TS.db"
"${RC[@]}" copyto "$STAGE/state.json" "$BUCKET/db/state-$TS.json"
"${RC[@]}" copy "$DATA/raw" "$BUCKET/raw"

echo "backup complete: $TS"
