// Router, header behaviour and boot.

import { api, onSnapshot, setCacheMode } from '/api.js';
import { el, clear, setQuiet } from '/ui.js';
import { agoCompact, dateTime, n, short, nameText } from '/format.js';
import { onTick, start as startCountdown } from '/countdown.js';
import { skewMs } from '/clock.js';
import * as views from '/views.js';

const view = document.getElementById('view');
let cleanup = null;

/* ---------------------------------------------------------------- theme --- */

const THEME_KEY = 'folding.theme';

function applyTheme(mode) {
  document.documentElement.setAttribute('data-theme', mode);
  localStorage.setItem(THEME_KEY, mode);
  // Charts read their colours from CSS custom properties, so they rebuild on this.
  document.dispatchEvent(new CustomEvent('themechange'));
}

document.getElementById('theme-toggle').addEventListener('click', () => {
  const next = document.documentElement.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
  applyTheme(next);
});

applyTheme(localStorage.getItem(THEME_KEY) || 'dark');

/* ------------------------------------------------------------ freshness --- */

const freshEl = document.getElementById('freshness');
const freshText = document.getElementById('freshness-text');
const countdownEl = document.getElementById('countdown');

let currentSnap = null;

onSnapshot((s) => {
  currentSnap = s;
  freshText.textContent = `Updated ${agoCompact(s.at)}`;
  freshEl.querySelector('.dot').classList.toggle('stale', !!s.stale);

  // The interval is next_expected_at - at; the API no longer duplicates it as a field.
  const intervalSec = Math.round((Date.parse(s.next_expected_at) - Date.parse(s.at)) / 1000);
  const parts = [
    `Snapshot: ${dateTime(s.at)}`,
    `Next expected: ${dateTime(s.next_expected_at)}`,
    s.warming_up?.interval_estimated
      ? 'Upstream interval not measured yet — assuming one hour.'
      : `Measured upstream interval: ${Math.round(intervalSec / 60)} min (${intervalSec}s)`,
  ];
  if (s.stale) parts.push('The expected update has not arrived yet.');
  // A device clock minutes out is common and makes every relative time on the page
  // look wrong. Times here are measured against the server, so they stay correct —
  // but saying so beats leaving someone to wonder which of us is confused.
  const skew = Math.round(skewMs() / 60000);
  if (Math.abs(skew) >= 2) {
    parts.push(`This device's clock is ${Math.abs(skew)} min ${skew > 0 ? 'ahead of' : 'behind'} ` +
      `the server. Times shown here follow the server.`);
  }
  if (s.warming_up?.history_span_sec) {
    parts.push('Less than 7 days of history collected — the per-day average spans a shorter window.');
  }
  freshEl.title = parts.join('\n');
});

// "Updated 3m ago" goes wrong just by sitting there, so it is re-rendered on
// the countdown's own tick rather than on a second timer of its own.
onTick(({ text, state }) => {
  countdownEl.textContent = text;
  countdownEl.classList.toggle('checking', state === 'checking');
  countdownEl.classList.toggle('overdue', state === 'overdue');
  if (currentSnap) freshText.textContent = `Updated ${agoCompact(currentSnap.at)}`;
});
startCountdown();

// Anchor the clock on load. Every other request may be answered from cache, and a
// cached body's server_time is frozen — so without one guaranteed-fresh response the
// page could spend its whole life reckoning against whenever its cache was filled.
// /v1/status is no-store and served from memory, so this costs almost nothing.
api.status().catch(() => {});

/* ------------------------------------------------------- header metrics --- */

// The header wraps to two or three rows as the window narrows, so anything that
// offsets from it — sticky table headers — needs its real height rather than the
// design constant.
const headerEl = document.querySelector('.header');
const trackHeaderHeight = () => {
  const h = Math.round(headerEl.getBoundingClientRect().height);
  document.documentElement.style.setProperty('--header-now', `${h}px`);
};
new ResizeObserver(trackHeaderHeight).observe(headerEl);
trackHeaderHeight();

/* --------------------------------------------------------------- router --- */

// Leaderboard orderings, validated here rather than passed through: a hand-typed
// ?sort= would otherwise reach the API, come back 400, and render as an error page
// where the honest answer is the default board.
//
// The aliases are the first published names. Mapping rather than merely accepting
// them means an old bookmark lands on the column it used to mean, with that column's
// heading marked, instead of silently on lifetime.
const SORT_VALUES = [
  'lifetime', 'per_day', 'today', 'this_week', 'this_month',
  'last_24h', 'wus', 'members', 'teams',
];
const SORT_ALIAS = { daily: 'today', weekly: 'this_week', monthly: 'this_month' };
const sortParam = (q) => {
  const v = SORT_ALIAS[q.get('sort')] || q.get('sort');
  return SORT_VALUES.includes(v) ? v : 'lifetime';
};

const routes = [
  [/^\/$/, () => views.home(view)],
  [/^\/overview\/?$/, () => views.overview(view)],
  [/^\/blog\/([^/]+)\/?$/, (m) => views.postPage(view, { slug: decodeURIComponent(m[1]) })],
  [/^\/teams\/?$/, (m, q) =>
    views.teamsList(view, { page: +(q.get('page') || 1), sort: sortParam(q) }, navigate)],
  // Ordered before the detail routes: the donor pattern is greedy enough to swallow
  // a /rivals suffix as part of the name.
  [/^\/teams\/([^/]+)\/rivals\/?$/, (m, q) =>
    views.rivalsPage(view, { kind: 'team', id: decodeURIComponent(m[1]), page: +(q.get('page') || 0) || undefined }, navigate)],
  [/^\/donors\/(.+?)\/rivals\/?$/, (m, q) =>
    views.rivalsPage(view, { kind: 'donor', id: decodeURIComponent(m[1]), page: +(q.get('page') || 0) || undefined }, navigate)],
  [/^\/teams\/([^/]+)\/?$/, (m) => views.teamDetail(view, { id: decodeURIComponent(m[1]) }, navigate)],
  [/^\/donors\/?$/, (m, q) =>
    views.donorsList(view, { page: +(q.get('page') || 1), sort: sortParam(q) }, navigate)],
  [/^\/donors\/(.+?)\/?$/, (m) => views.donorDetail(view, { name: decodeURIComponent(m[1]) }, navigate)],
  [/^\/search\/?$/, (m, q) => views.searchPage(view, { q: q.get('q') || '' }, navigate)],
  [/^\/api\/?$/, () => views.apiDocs(view)],
  [/^\/agents\/?$/, () => views.agentsPage(view)],
  [/^\/privacy\/?$/, () => views.privacyPage(view)],
  [/^\/disclaimer\/?$/, () => views.disclaimerPage(view)],
];

let rendering = false;
// Kept-content renders currently in flight; see the dim below.
let swapping = 0;

/**
 * @param quiet        an in-place refresh from new data: keep content, keep scroll,
 *                     bypass the cache the previous snapshot filled.
 * @param keepContent  a navigation within the same view — a leaderboard tab or a page
 *                     of the same list. The skeleton is ~450px against a leaderboard
 *                     thousands of pixels tall, so showing it collapses the page and
 *                     springs it back: a flicker, and a scroll position thrown away.
 *                     The old rows stay put until the new ones are ready to replace
 *                     them in a single frame.
 */
async function render({ quiet = false, keepContent = false } = {}) {
  const path = location.pathname;
  const query = new URLSearchParams(location.search);
  const href = path + location.search;

  for (const a of document.querySelectorAll('.nav a')) {
    const r = a.dataset.route;
    // Home covers the landing page and the posts it links to; a reader inside an
    // article has not left the section the nav says they are in.
    const active = r === '/' ? path === '/' || path.startsWith('/blog') : path.startsWith(r);
    a.setAttribute('aria-current', active ? 'page' : 'false');
  }

  if (cleanup) {
    cleanup();
    cleanup = null;
  }

  for (const [re, handler] of routes) {
    const m = path.match(re);
    if (m) {
      // A quiet refresh is a data swap under a reader's cursor: no skeletons, and no
      // yanking them back to the top of a page they had scrolled down.
      if (!quiet && !keepContent) window.scrollTo(0, 0);
      setQuiet(quiet || keepContent);
      // A quiet render is triggered by a new snapshot, so its fetches must not be
      // answered from the previous one's cache. A tab switch is a different URL and
      // wants the cache, so it is deliberately not included here.
      if (quiet) setCacheMode('reload');
      // Nothing blanks during a kept-content swap, so without this the click has no
      // acknowledgement at all until the response lands. A short dim is feedback; a
      // skeleton is a flash.
      //
      // Counted rather than a flag: clicking a second tab before the first responds
      // leaves two renders in flight, and whichever finishes first would otherwise
      // clear the dim while the other is still loading — reintroducing exactly the
      // flicker this removes.
      if (keepContent) swapping++;
      view.classList.toggle('swapping', swapping > 0);
      rendering = true;
      try {
        cleanup = (await handler(m, query)) || null;
      } finally {
        setQuiet(false);
        setCacheMode('default');
        if (keepContent) swapping--;
        view.classList.toggle('swapping', swapping > 0);
        rendering = false;
      }
      // The reader navigated while this was in flight, so what just landed is the
      // wrong page. Redraw the one they actually asked for.
      if (href !== location.pathname + location.search) render();
      return;
    }
  }

  clear(view).append(
    el('div.page-head', el('h1.page-title', 'Not found')),
    el('div.card', el('div.empty', 'No page at ', el('code', path), '.'))
  );
}

export function navigate(to, { keepContent = false } = {}) {
  if (to !== location.pathname + location.search) history.pushState(null, '', to);
  render({ keepContent });
}

// New data landed. Repaint the current page in place — no reload, no scroll jump.
document.addEventListener('dataupdate', async () => {
  // A navigation already in flight will paint fresher data than this would.
  if (rendering) return;
  await render({ quiet: true });
  // Say so, briefly. Numbers changing with no acknowledgement looks like a glitch.
  view.classList.remove('refreshed');
  void view.offsetWidth; // restart the animation rather than ignore a repeat
  view.classList.add('refreshed');
});

// Intercept in-app links so navigation never round-trips to the server.
document.addEventListener('click', (e) => {
  const a = e.target.closest('a');
  if (!a) return;
  const href = a.getAttribute('href');
  if (!href || !href.startsWith('/') || a.target === '_blank' || e.metaKey || e.ctrlKey) return;
  e.preventDefault();
  navigate(href);
});

window.addEventListener('popstate', () => render());

/* --------------------------------------------------------------- search --- */

const input = document.getElementById('q');
const results = document.getElementById('search-results');
let searchSeq = 0;
let activeIdx = -1;

const debounce = (fn, ms) => {
  let t;
  return (...a) => {
    clearTimeout(t);
    t = setTimeout(() => fn(...a), ms);
  };
};

function closeResults() {
  clear(results);
  activeIdx = -1;
}

// Results appear as you type, so prefix matches are useful rather than noisy —
// the objection to them is a full page of near-misses replacing what you were
// looking at, not the matching itself. An exact hit is pinned first and marked, so
// "these look similar" never gets mistaken for "this is you".
const runSearch = debounce(async (q) => {
  if (!q) return closeResults();
  const seq = ++searchSeq;
  try {
    const res = await api.search(q, undefined, { limit: 8 });
    if (seq !== searchSeq) return;
    const { teams, donors, exact_donor: exactDonor, exact_team: exactTeam } = res.data;
    clear(results);
    activeIdx = -1;

    donors.forEach((d, i) => {
      results.append(resultRow('Donor', d.name, d.points_total,
        `/donors/${encodeURIComponent(d.name)}`, i === 0 && exactDonor));
    });
    teams.forEach((t, i) => {
      results.append(resultRow('Team', t.name, t.points_total,
        `/teams/${t.team_id}`, i === 0 && exactTeam));
    });
    if (!donors.length && !teams.length) {
      results.append(el('div.search-empty',
        el('div', `Nothing matches “${q}”.`),
        el('div', { style: 'margin-top:4px;font-size:12px' },
          'Try fewer characters, or a numeric team ID.')));
    }
  } catch {
    closeResults();
  }
}, 130);

function resultRow(kind, name, points, href, exact) {
  const nameEl = el('span');
  nameText(nameEl, name);
  return el('div.search-result', { role: 'option', onclick: () => { input.value = ''; closeResults(); navigate(href); } },
    el('span.kind', kind),
    nameEl,
    // Only the exact match is marked; everything else is self-evidently a
    // near-miss, and badging them all would be the noise we are avoiding.
    exact ? el('span.exact', { title: 'Exact match' }, 'exact') : null,
    el('span.val', { title: n(points) }, short(points)));
}

input.addEventListener('input', () => runSearch(input.value.trim()));
input.addEventListener('focus', () => { if (input.value.trim()) runSearch(input.value.trim()); });

input.addEventListener('keydown', (e) => {
  const rows = [...results.querySelectorAll('.search-result')];
  if (e.key === 'Escape') {
    closeResults();
    input.blur();
    return;
  }
  if (e.key === 'Enter') {
    e.preventDefault();
    if (activeIdx >= 0 && rows[activeIdx]) rows[activeIdx].click();
    else if (input.value.trim()) {
      const q = input.value.trim();
      closeResults();
      navigate(`/search?q=${encodeURIComponent(q)}`);
    }
    return;
  }
  if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
  e.preventDefault();
  if (!rows.length) return;
  rows[activeIdx]?.classList.remove('active');
  activeIdx = e.key === 'ArrowDown'
    ? (activeIdx + 1) % rows.length
    : (activeIdx - 1 + rows.length) % rows.length;
  rows[activeIdx].classList.add('active');
  rows[activeIdx].scrollIntoView({ block: 'nearest' });
});

document.addEventListener('click', (e) => {
  if (!e.target.closest('#search')) closeResults();
});

// "/" focuses search from anywhere, the way every tool people already use does.
document.addEventListener('keydown', (e) => {
  if (e.key === '/' && !/^(INPUT|TEXTAREA)$/.test(document.activeElement.tagName)) {
    e.preventDefault();
    input.focus();
  }
});

render();
