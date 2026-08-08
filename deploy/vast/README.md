# The Vast template

One template, shared by URL, used by everybody. It carries no secrets, so it is safe
to make public: every per-reader value arrives in the environment variables they paste
at launch, and the one credential among them is good once and for thirty minutes.

Creating it needs a Vast account, so it is a manual step done once. Everything else —
the provisioning script, the enrolment, the dashboard — is in this repository.

## Settings to enter

| Field | Value |
|---|---|
| **Image** | `ubuntu:22.04` |
| **Launch mode** | **Entrypoint** |
| **On-start script** | the contents of [`onstart.sh`](onstart.sh) |
| **Disk** | 16 GB is ample; the client and agent together are under 100 MB |
| **Environment variables** | leave empty — the renter supplies them |

**Launch mode is not a preference.** Vast documents that custom environment variables
are not visible inside SSH, tmux or Jupyter sessions, and are naturally visible only to
an entrypoint script. Choose the wrong mode and the script sees no `FOLDING_ENROL`, the
box boots, folds nothing, enrols nowhere, and bills by the hour without a single error
in the log.

## After creating it

Copy the template's share URL into `VAST_TEMPLATE` in `web/views.js`. The Rent card on
`/fold` links there and is hidden until it is set, so the page never offers a button
that goes nowhere.

## What the renter pastes

Generated for them by the Rent card, so this is only for reference:

```
FOLDING_ENROL={"owner":"…","exp":…,"nonce":"…","sig":"…"}
FOLDING_USER=YourDonorName
FOLDING_TEAM=32
FOLDING_PASSKEY=optional
```

`FOLDING_USER` is the one that silently matters. Without it the client folds
anonymously, the box runs perfectly, and the points are credited to nobody — the only
failure here that looks exactly like success, which is why the script refuses to start
without it.

## Why the token is short-lived

Vast publishes instance logs to a bucket that needs no credentials. Anything printed
during provisioning is public, so the enrolment token is single-use and expires in
thirty minutes; finding one later is worth nothing. `onstart.sh` never echoes it and
never writes it to disk, and the agent replaces it with a key of its own on first run.

That is also why this is not a standing credential in Vast's account-level environment
variables, convenient though that would be: it would turn every future rental into one
click, and turn one leaked log line into permanent access to the fleet.
