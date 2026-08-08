#!/usr/bin/env bash
#
# Provisioning for a rented folding box on Vast.
#
# Installs the Folding@home client and our agent, points the first at the reader's
# donor account and the second at the relay, and gets out of the way. By the time this
# exits the machine is folding and is on its owner's dashboard.
#
# Two things about this file are not style choices.
#
# It never echoes FOLDING_ENROL. Vast publishes instance logs to a bucket that needs no
# credentials, so anything printed here is public — which is the whole reason the
# enrolment token is good once and for thirty minutes rather than being a standing
# credential. `set -x` would undo that in one character, so it is never used.
#
# And it fails loudly and early. A box that boots, folds nothing, and enrols nowhere
# still bills by the hour, so every step that can fail is checked and says which one
# went wrong. Silence here costs real money.

set -euo pipefail

log() { printf '[folding] %s\n' "$*"; }
die() { printf '[folding] FAILED: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------- inputs ---

# The token authorises adding one machine to one fleet. Minted in the browser, signed
# by the owner's key, single use.
[ -n "${FOLDING_ENROL:-}" ] || die "FOLDING_ENROL is not set. Copy the whole environment block from the Rent card on /fold."

# Who the points belong to. Without this the client folds anonymously and the work is
# credited to nobody, which is the one failure that looks exactly like success.
[ -n "${FOLDING_USER:-}" ] || die "FOLDING_USER is not set — the box would fold anonymously and the points would go nowhere."

FOLDING_TEAM="${FOLDING_TEAM:-0}"
FOLDING_PASSKEY="${FOLDING_PASSKEY:-}"

# Vast names every instance. Using it means a rented box arrives on the dashboard
# already identifiable, instead of as a container hostname nobody recognises.
MACHINE_NAME="${FOLDING_NAME:-${VAST_CONTAINERLABEL:-vast-$(hostname)}}"

log "provisioning ${MACHINE_NAME} for ${FOLDING_USER} (team ${FOLDING_TEAM})"

# Environment variables are not visible to SSH or Jupyter sessions on Vast unless they
# are exported, which is a documented trap. Everything but the secrets goes through, so
# somebody who shells in to debug sees the same configuration this script used.
{
  echo "FOLDING_USER=${FOLDING_USER}"
  echo "FOLDING_TEAM=${FOLDING_TEAM}"
  echo "FOLDING_NAME=${MACHINE_NAME}"
} >> /etc/environment

# ------------------------------------------------------- folding client ---

log "installing the folding client"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq curl ca-certificates tar >/dev/null

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)  GOARCH=amd64 ;;
  aarch64) GOARCH=arm64 ;;
  *)       die "unsupported architecture ${ARCH}" ;;
esac

# The client is fetched rather than baked into an image so the template does not go
# stale every time upstream ships a release.
FAH_DEB="/tmp/fah-client.deb"
curl -fsSL -o "$FAH_DEB" \
  "https://download.foldingathome.org/releases/public/fah-client/debian-stable-64bit/release/latest.deb" \
  || die "could not download the folding client"
apt-get install -y -qq "$FAH_DEB" >/dev/null || die "could not install the folding client"

# Configure before first start. The client writes its own config on first run, so
# setting it afterwards means a window where work is fetched under the wrong account.
install -d -m 0755 /etc/fah-client
{
  echo "user: ${FOLDING_USER}"
  echo "team: ${FOLDING_TEAM}"
  [ -n "$FOLDING_PASSKEY" ] && echo "passkey: ${FOLDING_PASSKEY}"
  echo "on-idle: false"
} > /etc/fah-client/config.xml

systemctl enable --now fah-client 2>/dev/null || {
  # Vast containers frequently have no init, so fall back to running it directly.
  log "no systemd; starting the client directly"
  nohup fah-client --config /etc/fah-client/config.xml >/var/log/fah-client.log 2>&1 &
}

# ---------------------------------------------------------------- agent ---

log "installing the folding agent"
AGENT_VERSION="${FOLDING_AGENT_VERSION:-latest}"
if [ "$AGENT_VERSION" = "latest" ]; then
  BASE="https://github.com/exec/folding-stats/releases/latest/download"
  # The latest release redirects, and the asset name carries the version — so ask the
  # API what the tag is rather than guessing at a filename that changes every release.
  TAG="$(curl -fsSL https://api.github.com/repos/exec/folding-stats/releases/latest \
        | sed -n 's/.*"tag_name": *"agent\/\([^"]*\)".*/\1/p' | head -1)"
  [ -n "$TAG" ] || die "could not determine the latest agent release"
else
  TAG="$AGENT_VERSION"
  BASE="https://github.com/exec/folding-stats/releases/download/agent%2F${TAG}"
fi

ASSET="foldingagent-${TAG}-linux-${GOARCH}.tar.gz"
cd /tmp
curl -fsSL -O "${BASE}/${ASSET}" || die "could not download ${ASSET}"
curl -fsSL -O "${BASE}/SHA256SUMS" || die "could not download SHA256SUMS"

# Checked rather than trusted. This binary is about to hold a credential and talk to
# the reader's fleet; a truncated download should stop here rather than half-run.
grep " ${ASSET}\$" SHA256SUMS | sha256sum -c - >/dev/null 2>&1 \
  || die "${ASSET} does not match its published checksum"

tar xzf "$ASSET"
install -m 0755 foldingagent /usr/local/bin/foldingagent
install -d -m 0700 /var/lib/foldingagent

# ---------------------------------------------------------------- start ---

# The token is passed through the environment of this one exec and never written to
# disk, a unit file, or the log. The agent makes its own key on first run and keeps
# that instead, so the token is spent the moment it is used.
log "enrolling ${MACHINE_NAME}"
FOLDING_ENROL="$FOLDING_ENROL" \
FOLDING_NAME="$MACHINE_NAME" \
  nohup /usr/local/bin/foldingagent >/var/log/foldingagent.log 2>&1 &

sleep 3
if ! pgrep -x foldingagent >/dev/null; then
  die "the agent exited immediately — see /var/log/foldingagent.log"
fi

log "done: ${MACHINE_NAME} is folding for ${FOLDING_USER} and should appear on /fold within a minute"
