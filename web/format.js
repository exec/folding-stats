// Formatting helpers.
//
// Points run from single digits to 5×10^13, so a table that prints them in full
// becomes a wall of commas that is impossible to compare down a column. Compact
// notation is the default in tables; the exact value goes in a title attribute so
// nothing is actually hidden.

const compact = new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 2 });
const full = new Intl.NumberFormat();

export const n = (v) => full.format(v ?? 0);
export const short = (v) => compact.format(v ?? 0);

/** A number cell: compact text, exact value on hover. */
export function numCell(v) {
  const el = document.createElement('td');
  el.className = 'num';
  el.textContent = short(v);
  el.title = n(v);
  return el;
}

export function plural(count, one, many = one + 's') {
  return `${n(count)} ${count === 1 ? one : many}`;
}

/** Rank movement. Positive means the entity climbed. */
export function delta(v) {
  const el = document.createElement('span');
  el.className = 'delta ' + (v > 0 ? 'up' : v < 0 ? 'down' : 'flat');
  // The arrow is a second channel so the direction is never colour alone.
  el.textContent = v > 0 ? `▲ ${n(v)}` : v < 0 ? `▼ ${n(-v)}` : '–';
  el.title = v === 0 ? 'No change' : `${v > 0 ? 'Up' : 'Down'} ${n(Math.abs(v))} places`;
  return el;
}

import { now as serverNow } from '/clock.js';

const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });

/**
 * How long ago something happened.
 *
 * Hours are compound ("1h 40m ago") rather than rounded ("2 hours ago"). Rounding is
 * fine in isolation and wrong beside a precise figure: an hour-and-forty-minute-old
 * snapshot shown as "2 hours ago" next to "overdue by 40:12" implies a 20-minute
 * publish interval, and the two numbers appear to contradict each other. They have
 * to reconcile, because a reader will check.
 */
export function ago(iso) {
  const then = new Date(iso).getTime();
  if (!Number.isFinite(then)) return '';
  // Against the server's clock, not this device's: see clock.js.
  const secs = Math.round((then - serverNow()) / 1000);
  const abs = Math.abs(secs);
  if (abs < 90) return rtf.format(secs, 'second');
  if (abs < 3600) return rtf.format(Math.round(secs / 60), 'minute');
  if (abs < 86400) {
    const h = Math.floor(abs / 3600);
    const m = Math.round((abs % 3600) / 60);
    // 1h 60m is not a thing.
    const [hh, mm] = m === 60 ? [h + 1, 0] : [h, m];
    const label = mm ? `${hh}h ${mm}m` : `${hh}h`;
    return secs < 0 ? `${label} ago` : `in ${label}`;
  }
  return rtf.format(Math.round(secs / 86400), 'day');
}

/**
 * A calendar date in UTC, for figures that are about UTC days rather than instants.
 *
 * Rendering a day boundary in the reader's own timezone moves it: 00:00 UTC on the 3rd
 * is the evening of the 2nd in Chicago, so a streak that began on the 3rd would be
 * reported as beginning the day before it did.
 */
export function utcDate(iso) {
  const d = new Date(iso);
  if (!Number.isFinite(d.getTime())) return '';
  return d.toLocaleDateString(undefined, { timeZone: 'UTC', month: 'short', day: 'numeric' });
}

export function dateTime(iso) {
  const d = new Date(iso);
  if (!Number.isFinite(d.getTime())) return '';
  // Named explicitly. A bare local time beside a UTC-labelled chart is the ambiguity
  // that makes the two look like they disagree when they do not.
  return d.toLocaleString(undefined, { timeZoneName: 'short' });
}

/** The browser's timezone abbreviation, e.g. "CDT". */
export function tzName() {
  const parts = new Intl.DateTimeFormat(undefined, { timeZoneName: 'short' })
    .formatToParts(new Date());
  return parts.find((p) => p.type === 'timeZoneName')?.value || 'local time';
}

/**
 * Production tier, 0–6, from the 7-day average.
 *
 * Ordinal, so it maps onto a single-hue ramp rather than distinct colours: the
 * bands are ordered, and a rainbow would imply they are categories.
 */
export function tier(pointsPerDay) {
  const v = pointsPerDay ?? 0;
  if (v <= 0) return 0;
  if (v < 150e3) return 1;
  if (v < 500e3) return 2;
  if (v < 1e6) return 3;
  if (v < 2.5e6) return 4;
  if (v < 10e6) return 5;
  return 6;
}

const TIER_LABEL = [
  'Idle',
  'Under 150k/day',
  '150k–500k/day',
  '500k–1M/day',
  '1M–2.5M/day',
  '2.5M–10M/day',
  'Over 10M/day',
];

/**
 * Write a name and colour it by the production tier it sits in.
 *
 * The colour is on the name rather than on a swatch beside it, which makes these
 * text colours: they are held to WCAG 4.5:1 against the surface, not the 2:1 a
 * decorative mark would need, and each theme has its own steps.
 *
 * Colour is never the only encoding here — the per-day figure the tier is derived
 * from is a column in the same row, so the ranking is fully readable without seeing
 * any of it.
 */
export function tierName(el, name, pointsPerDay) {
  nameText(el, name);
  const t = tier(pointsPerDay);
  el.classList.add('tier-name', `tier-${t}`);
  if (!el.title) el.title = TIER_LABEL[t];
  return el;
}

/**
 * Donor and team names arrive as raw upstream bytes and legitimately contain tabs
 * and newlines. Rendering them literally keeps the page honest about what the name
 * actually is; `.name-raw` preserves the whitespace so it is visible rather than
 * silently collapsed.
 */
export function nameText(el, name) {
  el.textContent = name;
  if (/[\t\n\r]/.test(name)) el.classList.add('name-raw');
  return el;
}

/**
 * A duration in plain words: "7 days", "3 hours", "42 minutes".
 *
 * Used where a label would otherwise assert a window we have not collected yet —
 * saying "the last 7 days" two hours after collection began is simply false.
 */
export function span(seconds) {
  if (!seconds || seconds < 60) return 'less than a minute';
  const units = [['day', 86400], ['hour', 3600], ['minute', 60]];
  for (const [name, size] of units) {
    if (seconds >= size) {
      const v = Math.round(seconds / size);
      return `${v} ${name}${v === 1 ? '' : 's'}`;
    }
  }
  return 'less than a minute';
}
