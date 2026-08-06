// Countdown to the next upstream publish, and the live refresh it triggers.
//
// Upstream does not publish on a wall clock: the interval measures 3606–3613s and
// creeps later every cycle, so a countdown to "the top of the hour" would drift
// visibly wrong within a day. The server measures its own cadence and sends it, and
// this module counts down to that.
//
// Two clocks are involved and they do not agree. Comparing the server's
// next_expected_at against the browser's Date.now() is wrong by however far the
// browser's clock is off, which on unsynced machines is routinely minutes. Every
// figure here is therefore a *difference*: the server's own (next_expected_at −
// server_time) minus the elapsed time this page has observed since the response
// arrived. Absolute skew cancels and never enters the arithmetic.

import { api, onSnapshot } from '/api.js';

/** How long after the predicted publish we start checking, and how often. */
const CHECK_INTERVAL_MS = 20_000;
/** Backoff ceiling. Upstream outages last hours; a fixed 20s would poll all night. */
const MAX_CHECK_INTERVAL_MS = 120_000;
/** Spread so many clients don't arrive on the same second. */
const JITTER_MS = 4_000;
/**
 * How long past the prediction we call it "checking" rather than "overdue".
 *
 * Adaptive polling captures a publish within about a minute of it appearing, so a
 * short wait after zero is the normal case and worth saying so. Past that, upstream
 * is genuinely late and saying "checking" forever is a dead end: it never conveys how
 * late, and it never changes, so a display that is working looks like one that hung.
 */
const CHECKING_WINDOW_MS = 90_000;

let plan = null; // {atISO, remainingAtReceipt, receivedAt, measured}
let checkDelay = CHECK_INTERVAL_MS;
let timer = null;
let listeners = new Set();

/** Subscribe to countdown ticks. Receives {text, state}. */
export function onTick(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

onSnapshot((s) => {
  const serverTime = Date.parse(s.server_time);
  const nextExpected = Date.parse(s.next_expected_at);
  if (!Number.isFinite(serverTime) || !Number.isFinite(nextExpected)) {
    plan = null;
    return;
  }

  const isNew = !plan || plan.atISO !== s.at;
  plan = {
    atISO: s.at,
    // The whole wait, measured entirely on the server's clock.
    remainingAtReceipt: nextExpected - serverTime,
    // ...against which we measure only elapsed time on ours.
    receivedAt: Date.now(),
    measured: !s.warming_up?.interval_estimated,
  };

  if (isNew) checkDelay = CHECK_INTERVAL_MS;
});

function remainingMs() {
  if (!plan) return null;
  return plan.remainingAtReceipt - (Date.now() - plan.receivedAt);
}

function mmss(ms) {
  const total = Math.max(0, Math.round(ms / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  const pad = (v) => String(v).padStart(2, '0');
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`;
}

function emit() {
  const ms = remainingMs();
  let text, state;
  if (ms === null) {
    text = '';
    state = 'unknown';
  } else if (ms > 0) {
    // An unmeasured interval is the nominal hour, not an observation. Saying "~"
    // rather than a precise clock keeps the display from claiming a precision that
    // the first few cycles after a cold start do not have.
    // "next in 5:12" rather than "next update in 5:12": this sits beside "Updated
    // 30m ago" in a header where width is the scarcest thing on the page, and the word
    // it drops is already established by the thing next to it.
    text = `next in ${plan.measured ? '' : '~'}${mmss(ms)}`;
    state = 'waiting';
  } else if (-ms < CHECKING_WINDOW_MS) {
    text = 'checking…';
    state = 'checking';
  } else {
    // Count up instead. The figure moves every second, which is the difference
    // between a display that is waiting and one that has hung.
    text = `overdue by ${mmss(-ms)}`;
    state = 'overdue';
  }
  for (const fn of listeners) fn({ text, state, remainingMs: ms });
}

/**
 * Ask whether a new snapshot has landed.
 *
 * `/v1/status` is served from memory and is the cheapest route we have, and every
 * response republishes the snapshot block — so a check that finds new data updates
 * the countdown as a side effect of asking.
 */
async function check() {
  const before = plan?.atISO;
  try {
    const res = await api.status();
    if (res?.snapshot?.at && res.snapshot.at !== before) {
      // onSnapshot has already reset the countdown; tell the app to repaint.
      document.dispatchEvent(new CustomEvent('dataupdate'));
      return true;
    }
  } catch {
    // A failed check is not worth surfacing: the next one is seconds away, and the
    // stale dot already tells the story if upstream is genuinely down.
  }
  // Upstream is late. Ease off so an outage doesn't mean all-night polling.
  checkDelay = Math.min(checkDelay * 1.5, MAX_CHECK_INTERVAL_MS);
  return false;
}

let nextCheckAt = 0;

function tick() {
  const ms = remainingMs();
  if (ms !== null && ms <= 0) {
    // A hidden tab has no one watching the countdown, and polling it would be pure
    // waste. The visibilitychange handler checks immediately on return.
    if (!document.hidden && Date.now() >= nextCheckAt) {
      nextCheckAt = Date.now() + checkDelay + Math.random() * JITTER_MS;
      check();
    }
  }
  emit();
}

/** Begin ticking. Idempotent. */
export function start() {
  if (timer) return;
  timer = setInterval(tick, 1000);
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) return;
    // Back from a background tab, possibly hours later: check at once rather than
    // waiting out a backoff that accumulated while nobody was looking.
    nextCheckAt = 0;
    checkDelay = CHECK_INTERVAL_MS;
    tick();
  });
  tick();
}
