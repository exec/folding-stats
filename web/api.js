// API client.
//
// Every response carries a `snapshot` block describing how fresh the data is, so
// the client reads that once per request and republishes it to the header rather
// than polling a separate status endpoint.

import { sync as syncClock } from '/clock.js';

const listeners = new Set();
let latestSnapshot = null;
// The newest server_time we have seen, as epoch ms.
//
// Data responses are cacheable — deliberately, since a cycle's data does not change
// until the next publish. But the snapshot block rides along inside the cached body,
// so replaying one hands us a server_time frozen at the moment it was stored. Taken
// as "now" that drags the clock offset backwards, and every page ends up reporting
// whatever freshness its own cached copy happened to capture: one tab saying eight
// minutes, another forty-nine, all from the same snapshot.
//
// A snapshot block is only ever adopted if it is newer than what we already hold.
// The cached body is still perfectly good data — it is the same cycle — it just does
// not get to tell us what time it is.
let latestServerTime = 0;

/** Subscribe to snapshot metadata; called on every successful response. */
export function onSnapshot(fn) {
  listeners.add(fn);
  if (latestSnapshot) fn(latestSnapshot);
  return () => listeners.delete(fn);
}

export function snapshot() {
  return latestSnapshot;
}

export class ApiError extends Error {
  constructor(status, body) {
    super(body?.message || `Request failed (${status})`);
    this.status = status;
    this.kind = body?.error || 'error';
  }
}

// Responses are cacheable — deliberately, since data is immutable between cycles.
// That is exactly wrong during a refresh triggered by a *new* cycle: the browser
// would satisfy the re-fetch from the previous snapshot's cached copy, and the page
// would announce fresh data while showing the old numbers. A refresh therefore
// revalidates, and everything else caches as normal.
let cacheMode = 'default';

export function setCacheMode(mode) {
  cacheMode = mode;
}

async function request(path, { signal, cache } = {}) {
  const res = await fetch(path, {
    signal,
    cache: cache || cacheMode,
    headers: { Accept: 'application/json' },
  });
  let body = null;
  try {
    body = await res.json();
  } catch {
    // Fall through: a non-JSON body is still an error we can report by status.
  }
  if (!res.ok) throw new ApiError(res.status, body);

  const st = Date.parse(body?.snapshot?.server_time);
  if (Number.isFinite(st) && st > latestServerTime) {
    latestServerTime = st;
    // Sample the server's clock before anything renders against it.
    syncClock(body.snapshot.server_time);
    latestSnapshot = body.snapshot;
    for (const fn of listeners) fn(latestSnapshot);
  }
  return body;
}

function qs(params) {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== '') p.set(k, v);
  }
  const s = p.toString();
  return s ? `?${s}` : '';
}

// Donor names are unvalidated upstream text: they contain tabs, newlines, slashes
// and non-ASCII. encodeURIComponent handles all of it, and the server decodes the
// path segment back to the exact bytes.
const seg = (s) => encodeURIComponent(s);

export const api = {
  summary: (o) => request('/v1/summary', o),
  // Posts revalidate rather than trusting a stored copy. The server sends
  // no-cache, but that only governs responses received after it — a browser
  // holding an entry cached under an older policy would keep serving it until it
  // expired on its own, showing text that had already been edited away. Asking for
  // revalidation makes any such entry self-heal on the next visit, and costs a 304
  // with no body when nothing has changed.
  posts: (o) => request('/v1/posts', { ...o, cache: 'no-cache' }),
  post: (slug, o) => request(`/v1/posts/${seg(slug)}`, { ...o, cache: 'no-cache' }),
  // The freshness probe must never be answered from a cache — a cached "nothing has
  // changed" is indistinguishable from the truth and would stall the countdown
  // permanently. The server sends no-store; this is the belt to that pair of braces,
  // covering intermediaries that ignore it.
  status: (o) => request('/v1/status', { ...o, cache: 'no-store' }),
  projectHistory: (params, o) => request(`/v1/summary/history${qs(params)}`, o),

  teams: (params, o) => request(`/v1/teams${qs(params)}`, o),
  team: (id, o) => request(`/v1/teams/${encodeURIComponent(id)}`, o),
  teamMembers: (id, params, o) => request(`/v1/teams/${encodeURIComponent(id)}/members${qs(params)}`, o),
  teamHistory: (id, params, o) => request(`/v1/teams/${encodeURIComponent(id)}/history${qs(params)}`, o),
  teamRivals: (id, params, o) => request(`/v1/teams/${encodeURIComponent(id)}/rivals${qs(params)}`, o),

  donors: (params, o) => request(`/v1/donors${qs(params)}`, o),
  donor: (name, o) => request(`/v1/donors/${seg(name)}`, o),
  donorTeams: (name, params, o) => request(`/v1/donors/${seg(name)}/teams${qs(params)}`, o),
  donorHistory: (name, params, o) => request(`/v1/donors/${seg(name)}/history${qs(params)}`, o),
  donorRivals: (name, params, o) => request(`/v1/donors/${seg(name)}/rivals${qs(params)}`, o),

  search: (q, type, o) => request(`/v1/search${qs({ q, type, limit: o?.limit })}`, o),
};
