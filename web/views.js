// Views.
//
// Each export renders one route into the main element and returns an optional
// cleanup function, which the router calls before rendering the next route so
// charts release their observers.

import { api, snapshot } from '/api.js';
import { el, clear, card, cardWith, statTile, pager, segmented, notice, loading, skeleton, errorView, link } from '/ui.js';
import { n, short, ago, dateTime, utcDate, delta, tierName, nameText, plural, span, tzName } from '/format.js';
import { productionChart, seriesChart, stack, legend, palette, densify, perDayPoints, MAX_STACK_SERIES } from '/charts.js';
import { LocalClient } from '/fah.js';

const PER_PAGE = 100;

/**
 * Where the Discord bot installs from.
 *
 * The bare authorize URL rather than one carrying scopes: the application declares
 * both install contexts, so Discord's own dialog offers the choice between adding it
 * to a server and adding it to an account. Pinning scopes here would pick one for the
 * reader and quietly drop the other.
 */
const DISCORD_INVITE = 'https://discord.com/oauth2/authorize?client_id=1534970793283293255';

/** Newest snapshot time in ms — the point past which no bucket can exist yet. */
/** How much history the "active" counts actually cover. */
function activeWindow() {
  const s = snapshot();
  const w = s?.warming_up;
  return !w?.history_span_sec ? '7 days' : span(w.history_span_sec);
}

const complete = () => {
  const s = snapshot();
  return !s?.warming_up?.history_span_sec;
};

function snapshotMs() {
  const s = snapshot();
  return s ? Date.parse(s.at) : Date.now();
}

/**
 * The window a chart may draw in: nothing after the newest snapshot, nothing before
 * the service started watching.
 *
 * One helper rather than the pair repeated at every render site. Both bounds are
 * needed together or a per-day rate is wrong — the upper one keeps the in-progress
 * bucket from being divided by time that has not happened, the lower one keeps it
 * from being divided by time nobody was recording.
 */
function chartSpan() {
  const w = snapshot()?.warming_up;
  const until = snapshotMs();
  return { until, since: w?.history_span_sec ? until - w.history_span_sec * 1000 : null };
}

/** The granularities every history chart offers, in coarsening order. */
const GRANULARITIES = [
  { value: 'hourly', label: 'Hourly', title: 'One point per upstream publish' },
  { value: 'daily', label: 'Daily', title: 'UTC days' },
  { value: 'weekly', label: 'Weekly', title: 'UTC weeks starting Sunday' },
  { value: 'monthly', label: 'Monthly', title: 'UTC calendar months' },
];

/**
 * What a bar stands for: what the bucket produced, or the rate it produced at.
 *
 * Totals make the granularity control a unit change — a monthly bar is roughly 700x
 * an hourly one, so the four views cannot be compared with each other or with the
 * "Per day" figure in the stats above. Dividing each bucket by its own length holds
 * the unit still and leaves granularity doing the only thing it is useful for here,
 * which is smoothing.
 */
const RATES = [
  { value: 'total', label: 'Total', title: 'Points produced in each bucket' },
  // PPD is what Folding@home calls this and what donors already compare each other
  // by, so the control is labelled in the reader's vocabulary rather than ours.
  { value: 'per-day', label: 'PPD', title: 'Each bucket as points per day' },
];

/**
 * How several series are drawn against each other.
 *
 * Two different questions, not two skins. Stacked accumulates the series and answers
 * "what did they add up to, and who made it up". Overlaid draws each on its own and
 * answers "which of them is ahead". The underlying values differ between the two —
 * stacking sums them and comparing must not — so this switches the data as well as
 * the marks.
 */
const SHAPES = [
  { value: 'stacked', label: 'Stacked', title: 'Contributions piled up to the total' },
  { value: 'lines', label: 'Lines', title: 'Each series on its own, for comparison' },
];

/**
 * The columns a leaderboard can be ordered by, in table order.
 *
 * Each one is a heading the reader can click, and the key is the same string the API
 * takes — so what the column says, what the URL says and what the server sorts by are
 * one value rather than three that have to be kept in step.
 *
 * `today`, `this_week` and `this_month` are calendar buckets in UTC, not rolling
 * windows: `today` reads low just after 00:00 UTC because it answers "produced
 * today". `per_day` is the seven-day average, and `last_24h` the rolling day — three
 * different questions that are easy to mistake for one.
 */
const COLUMNS = [
  { key: 'members', label: 'Members', kind: 'team', title: 'Active of total members' },
  { key: 'teams', label: 'Teams', kind: 'donor', title: 'Teams this donor folds for' },
  { key: 'per_day', label: 'Per day', field: 'points_per_day_7d_avg',
    title: 'Points over the last 7 days divided by 7' },
  { key: 'today', label: 'Today', field: 'points_today_utc', title: 'Points since 00:00 UTC' },
  { key: 'this_week', label: 'This week', field: 'points_this_week_utc',
    title: 'Points since Sunday 00:00 UTC' },
  { key: 'this_month', label: 'This month', field: 'points_this_month_utc',
    title: 'Points since the 1st, 00:00 UTC' },
  { key: 'last_24h', label: 'Last 24h', field: 'points_last_24h', title: 'Rolling 24 hours' },
  { key: 'wus', label: 'WUs', field: 'wus_total', title: 'Work units completed' },
  { key: 'lifetime', label: 'Points', field: 'points_total', title: 'Cumulative points, all time' },
];

const SORT_BLURB = {
  lifetime: 'lifetime points',
  per_day: 'the 7-day average',
  today: 'points since 00:00 UTC today',
  this_week: 'points since Sunday 00:00 UTC',
  this_month: 'points since the 1st of the month, UTC',
  last_24h: 'points in the rolling last 24 hours',
  wus: 'work units',
  members: 'member count',
  teams: 'team count',
};

/** A list URL that carries only the parameters that differ from the defaults. */
function listHref(base, { page = 1, sort = 'lifetime' } = {}) {
  const q = new URLSearchParams();
  if (page > 1) q.set('page', page);
  if (sort !== 'lifetime') q.set('sort', sort);
  const s = q.toString();
  return s ? `${base}?${s}` : base;
}

/**
 * A sortable column heading.
 *
 * Sorting is descending only. Every column here is "how much", and nobody opens a
 * leaderboard to find who is doing the least — an ascending pass would mostly return
 * the millions of donors tied on zero.
 */
function sortHeader(col, sort, onPick) {
  const active = col.key === sort;
  return el('th', { class: active ? 'sortable active' : 'sortable', 'aria-sort': active ? 'descending' : 'none' },
    el('button', {
      title: col.title,
      onclick: () => onPick(col.key),
    }, col.label, active ? el('span.sort-caret', '▾') : null));
}

/**
 * How long until an overtake, in the coarsest unit that still says something.
 *
 * The input is a seven-day average projected forward, so precision past the leading
 * couple of digits is invented. "in 3 days" is a claim worth making; "in 3.17 days"
 * dresses the same guess up as a measurement.
 */
function overtakeIn(days) {
  if (days === null || days === undefined) return null;
  if (days <= 0) return 'level now';
  if (days < 1) {
    const h = Math.max(1, Math.round(days * 24));
    return `in ${h} ${h === 1 ? 'hour' : 'hours'}`;
  }
  if (days < 14) {
    const d = Math.round(days);
    return `in ${d} ${d === 1 ? 'day' : 'days'}`;
  }
  if (days < 90) return `in ${Math.round(days / 7)} weeks`;
  if (days < 730) return `in ${Math.round(days / 30.4)} months`;
  return `in ${Math.round(days / 365)} years`;
}

/**
 * The rivals table: who this entity is about to pass, and who is about to pass it.
 *
 * The subject's own row travels in the list rather than being spliced in by the
 * client, so the neighbourhood renders as one continuous ranking with the reader
 * inside it — which is the whole point of the view. Rows above are targets, rows
 * below are chasers, and the two are distinguished by position rather than by being
 * split into separate tables.
 */
function rivalsTable(data, kind) {
  const rows = data.rivals || [];
  const selfRank = data.rank;
  const body = el('tbody');

  for (const r of rows) {
    const when = overtakeIn(r.overtake_days);
    // Above the reader is someone to catch; below is someone catching up. Same
    // projection either way — only the reader's stake in it differs.
    const chasing = r.rank < selfRank;
    const href = kind === 'team' ? `/teams/${r.team_id}` : `/donors/${encodeURIComponent(r.name)}`;
    const nameEl = r.self ? el('span') : el('a', { href });
    tierName(nameEl, r.name, r.points_per_day_7d_avg);

    body.append(el('tr', { class: r.self ? 'rival-self' : null },
      el('td.rank', n(r.rank)),
      el('td.left.name-cell', nameEl, r.self ? el('span.rival-you', 'you') : null),
      el('td.num', { title: n(r.points_total) }, short(r.points_total)),
      el('td.num', { title: n(r.points_per_day_7d_avg) }, short(r.points_per_day_7d_avg)),
      el('td.num', r.self ? '—' : el('span', { title: n(r.points_gap) }, short(r.points_gap))),
      r.self
        ? el('td.left.muted', '—')
        : el('td.left.overtake',
            when
              ? el('span', { class: chasing ? 'gain' : 'loss' },
                  `${chasing ? '▲' : '▼'} ${when}`)
              : el('span.muted', { title: 'Neither is closing on the other at current rates' }, 'not closing'))
    ));
  }

  return el('div.table-wrap', el('table.data',
    el('thead', el('tr',
      el('th.left', 'Rank'),
      el('th.left', kind === 'team' ? 'Team' : 'Donor'),
      el('th', 'Points'),
      el('th', 'Per day'),
      el('th', 'Gap'),
      el('th.left', 'Overtake'))),
    body));
}

/** How many neighbours a rivals view shows at once. */
const RIVALS_PER_PAGE = 21;

/**
 * A rivals card that pages through the ranking.
 *
 * It opens on whichever page the subject is on rather than at rank 1 — the server
 * picks that when no page is given — and from there it is an ordinary pager, so
 * paging up walks toward the leaders and paging down toward the chasers. Every row
 * keeps its projection against the subject however far away it lands, which is what
 * makes walking the board worth doing: "could I ever catch them" has an answer.
 *
 * `load` takes params and returns the API response, so the same card serves teams,
 * donors, the detail pages and the standalone view.
 */
function rivalsCard(load, kind, { href, onPage } = {}) {
  const body = el('div');
  const node = href
    ? cardWith('Rivals', el('a.section-link', { href }, 'Open ↗'), body)
    : card('Rivals', body);

  async function go(page) {
    // The old rows stay while the next page loads, for the same reason the
    // leaderboard tabs do: a skeleton here collapses the card and springs it back.
    body.classList.add('swapping');
    try {
      const res = await load({ page, per_page: RIVALS_PER_PAGE });
      const d = res.data;
      clear(body).append(
        rivalsTable(d, kind),
        pager(res.page.page, res.page.total_pages, res.page.total_items, (p) => {
          if (onPage) onPage(p);
          go(p);
        }),
        el('div.chart-note',
          'Projected from each side’s current per-day average, held constant. ',
          'Nobody folds at a constant rate, so treat these as “about when”, not a date. ',
          `Anything further out than ${Math.round(d.horizon_days / 365)} years is reported as not closing.`));
    } catch (err) {
      console.warn('rivals: could not load', err);
      clear(body).append(el('div.card-body',
        el('div.muted', `Couldn\u2019t load rivals \u2014 ${err?.message || err}`)));
    } finally {
      body.classList.remove('swapping');
    }
  }

  return { node, go };
}

/**
 * The rivals card, or a note saying why it is not there.
 *
 * A rivals table is an extra and must never take down the figures the reader actually
 * came for — but swallowing the failure outright turned out worse than the crash it
 * was guarding against. A missing-parameters bug left the card silently absent from
 * every detail page, with nothing on the page or in the console to say that anything
 * was meant to be there; the standalone view, which had no such catch, was the only
 * thing that ever reported it.
 */
async function rivalsSection(load, kind, href) {
  const c = rivalsCard(load, kind, { href });
  await c.go();          // no page: the server opens on the subject's own
  return el('section.section', c.node);
}

/** Empty chart state. Says what window was searched, so "nothing" is informative. */
function emptyChart(granularity) {
  const window_ = {
    hourly: 'the last 30 days',
    daily: 'the last 5 years',
    weekly: 'the last 5 years',
    monthly: 'any month on record',
  }[granularity] || 'this window';
  return el('div.chart-empty',
    el('div', 'No production recorded'),
    el('div', { style: 'font-size:12px;margin-top:4px' }, `Nothing in ${window_}.`));
}

/* ------------------------------------------------------------ components --- */

/* ------------------------------------------------------------------ home --- */

const postDate = (iso) =>
  new Date(iso).toLocaleDateString(undefined, {
    timeZone: 'UTC', year: 'numeric', month: 'long', day: 'numeric',
  });

/**
 * The landing page: what this is, then the numbers.
 *
 * A first-time visitor arrives without knowing why this site exists or why its
 * figures differ from the one they already use, and a wall of leaderboards answers
 * neither question. The latest post carries that explanation; the rail keeps the
 * headline numbers one glance away for everyone who already knows and just wants
 * them. The full overview is its own page, one click along in the nav.
 */
export async function home(view) {
  loading(view);
  try {
    const [postsRes, sum] = await Promise.all([api.posts(), api.summary()]);
    const posts = postsRes.data.posts || [];
    clear(view);

    const main = el('div.home-main');
    if (!posts.length) {
      main.append(card('', el('div.empty', 'No posts yet.')));
    } else {
      // Newest in full — it is the reason someone is on this page — and the rest as
      // a list, which is all a two-line archive needs to be.
      const latest = await api.post(posts[0].slug);
      main.append(postArticle(latest.data, { heading: 'h1' }));
      if (posts.length > 1) {
        main.append(card('Earlier posts',
          el('div.post-list', ...posts.slice(1).map(postRow))));
      }
    }

    view.append(el('div.home', main, statsRail(sum.data)));
  } catch (err) {
    errorView(view, err);
  }
}

function postRow(p) {
  return el('a.post-row', { href: `/blog/${encodeURIComponent(p.slug)}` },
    el('span.post-row-title', { text: p.title }),
    el('time.post-row-date', { datetime: p.date, text: postDate(p.date) }));
}

/** One article. `heading` is h1 on a page of its own, h2 when stacked on the home page. */
function postArticle(p, { heading = 'h1' } = {}) {
  return el('article.post',
    el(`${heading}.post-title`, { text: p.title }),
    el('time.post-date', { datetime: p.date, text: postDate(p.date) }),
    // The body is Markdown rendered server-side, with raw HTML dropped by the
    // renderer — so this is our own generated markup, not anything a reader supplied.
    el('div.prose', { html: p.html }));
}

/** The stats rail: the headline figures, without the full leaderboards. */
function statsRail(d) {
  const fig = (label, value, sub) =>
    el('div.rail-stat',
      el('div.rail-label', { text: label }),
      el('div.rail-value.num', { title: typeof value === 'string' ? value : undefined, text: value }),
      sub ? el('div.rail-sub', { text: sub }) : null);

  return el('aside.home-side',
    cardWith('Right now', el('a.section-link', { href: '/overview' }, 'Full overview →'),
      el('div.card-body',
        fig('Total points', short(d.points_total), n(d.points_total)),
        fig('Last 24 hours', short(d.points_last_24h), 'points across the project'),
        fig('Active donors', short(d.donors_active), `of ${n(d.donors_total)} in the last ${activeWindow()}`),
        fig('Teams', short(d.teams_active), `producing, of ${n(d.teams_total)}`),
        fig('Work units', short(d.wus_total), n(d.wus_total)))),
    card('The API',
      el('div.card-body',
        el('p.rail-note', 'Every figure here is available as JSON. No key, no account, and no rate limit for now.'),
        el('a.section-link', { href: '/api' }, 'Read the docs →'))));
}

/** A single post on its own page. */
export async function postPage(view, { slug }) {
  loading(view);
  try {
    const res = await api.post(slug);
    clear(view);
    view.append(
      el('div.breadcrumb', link('/', 'Home'), el('span', '/'), el('span', { text: res.data.title })),
      el('div.post-page', postArticle(res.data, { heading: 'h1' })));
  } catch (err) {
    errorView(view, err);
  }
}

/**
 * The subtitle of a Rank tile: how far the entity moved over the last day.
 *
 * An absent rank_change_24h is not "no movement". It means there is no ranking a day
 * back to compare against — either the entity is newer than that, or the service has
 * not yet been watching for a full 24 hours. Those are different facts from a rank
 * that held steady, and rendering both as "–" would claim a measurement never made,
 * so the tile falls back to describing what the rank means instead.
 */
function rankMovement(change, fallback) {
  if (change === undefined || change === null) return fallback;
  return el('span.stat-move', delta(change), ' in 24h');
}

/** The two bases the Percentile tile can be read on. */
const BASES = [
  { value: 'lifetime', label: 'Lifetime', title: 'Share of every team or donor tracked, by lifetime points' },
  { value: 'this_month', label: 'This month', title: 'Share of those that produced this month' },
];

/**
 * A share of the field, formatted across the five orders of magnitude it spans.
 *
 * The leader of two million donors is 0.00005% and somebody mid-table is 43%. One
 * fixed precision cannot serve both: it prints either "0.00%" for the best in the
 * world, or "43.0000%" for everybody else.
 */
function pct(p) {
  if (p >= 10) return `${p.toFixed(0)}%`;
  if (p >= 1) return `${p.toFixed(1)}%`;
  if (p >= 0.01) return `${p.toFixed(2)}%`;
  // Below a hundredth of a percent the digits stop meaning anything. "Top 0.000047%"
  // of two million donors is a laborious way of writing "first", and the Rank tile
  // right beside this one already says it.
  return '<0.01%';
}

/**
 * Where the entity sits in the field, which is the question a rank only half answers.
 *
 * "#48,213" means nothing without knowing whether the field is fifty thousand or two
 * million, and most people cannot hold that denominator in their head while reading a
 * page. The share carries it.
 */
function standingTile(standing, basis) {
  const s = standing?.[basis];
  const hint = 'The share of the field at or above this entity — smaller is better';
  // Absent on this month is not last place: it is not being in the field at all, and
  // a percentage would be a measurement of something that did not happen.
  if (!s) return statTile('Percentile', '—', 'nothing produced this month', hint);
  return statTile('Percentile', `Top ${pct(s.top_percent)}`,
    basis === 'lifetime'
      ? `of ${n(s.of)} tracked, by lifetime points`
      : `of ${n(s.of)} that produced this month`,
    hint);
}

/**
 * How long a streak has to run to reach each step of the production heat ramp.
 *
 * The ramp is reused rather than given its own colours: it is already validated in
 * both themes for contrast, monotone lightness and separable neighbours, and a second
 * six-step scale would be the same work done twice and less well.
 */
const STREAK_TIERS = [365, 100, 30, 7, 3, 1];

function streakTier(days) {
  const i = STREAK_TIERS.findIndex((t) => days >= t);
  return i < 0 ? 0 : STREAK_TIERS.length - i;
}

/**
 * The flame, drawn rather than typed.
 *
 * An emoji would have been shorter, but it carries its own colours and so could not
 * climb the ramp with the streak — and the point of the icon is that it changes.
 */
function flame() {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('class', 'flame');
  svg.setAttribute('aria-hidden', 'true');
  const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
  path.setAttribute('d', 'M12 2 C16 8, 18 10, 18 14 A6 6 0 0 1 6 14 C6 10, 8 8, 12 2 Z');
  path.setAttribute('fill', 'currentColor');
  svg.append(path);
  return svg;
}

/**
 * Consecutive days with production.
 *
 * Every other tile is a magnitude. This is the only one about persistence, which is
 * what distributed computing actually runs on — a modest machine left on every day
 * beats a fast one switched on twice a month, and nothing else here says so.
 *
 * A run reaching the first day on record is labelled as such rather than quoted. This
 * service started watching on a particular day, so somebody who has folded daily for
 * fifteen years would otherwise be told they were on a three-day streak.
 */
function streakTile(s) {
  const hint = 'Consecutive days with any production. For a donor, folding for two ' +
    'teams on one day is one day.';
  if (!s || !s.active_days) return statTile('Streak', '—', 'no production on record', hint);
  if (!s.current) {
    return statTile('Streak', '—', `best run was ${plural(s.longest, 'day')}`, hint);
  }
  const value = el(`span.streak.tier-${streakTier(s.current)}`,
    flame(), el('span', { text: plural(s.current, 'day') }));
  return statTile('Streak', value,
    s.at_collection_floor
      ? 'every day since collection began'
      : `since ${utcDate(s.since)} · best ${plural(s.longest, 'day')}`,
    hint);
}

/**
 * The stats row for an entity's own page, with the basis switch its Percentile tile
 * needs.
 *
 * The switch sits above the grid rather than inside the tile: a tile is 180px at its
 * narrowest and a two-way control inside one would crowd out the figure it qualifies.
 * The tile names its own basis in the subtitle, so the pairing survives the distance.
 */
function detailStats(d, extra = []) {
  let basis = 'lifetime';
  const tiles = () => [
    ...extra,
    ...(d.standing ? [standingTile(d.standing, basis)] : []),
    ...(d.streak ? [streakTile(d.streak)] : []),
  ];
  // Nothing to switch between: no control, and one render.
  if (!d.standing) return el('section.section', productionStats(d, tiles()));

  const bar = el('div.stats-bar');
  const host = el('div');
  function draw() {
    clear(bar).append(segmented(BASES, basis, (v) => { basis = v; draw(); }));
    clear(host).append(productionStats(d, tiles()));
  }
  draw();
  return el('section.section', bar, host);
}

/** The production figures every entity shares, as a row of stat tiles. */
function productionStats(d, extra = []) {
  return el(
    'div.stats',
    ...extra,
    statTile('Points', short(d.points_total), n(d.points_total)),
    // Both windows read low until enough history exists to fill them, so the
    // subtitle names what was actually measured rather than the nominal window.
    statTile('Per day', short(d.points_per_day_7d_avg),
      complete() ? '7-day average' : `over ${activeWindow()} so far`,
      'Total points over the last 7 days divided by 7'),
    statTile('Last 24 hours', short(d.points_last_24h),
      complete() ? 'rolling window' : `only ${activeWindow()} collected`),
    statTile('Today', short(d.points_today_utc), 'since 00:00 UTC'),
    // The ratio rides on the tile holding the number it is derived from, rather than
    // taking a tile of its own: it is a property of the work units, not a sixth
    // headline figure, and the stats row is already full.
    statTile('Work units', short(d.wus_total), perWUText(d),
      'Work units completed, and the points each one earned. Compare the recent figure ' +
      'between entities — the lifetime one is far lower for everybody, because points ' +
      'per work unit have inflated hugely since the project began.')
  );
}

/**
 * Points per work unit, as the Work units tile's subtitle.
 *
 * This is the only figure on the page that says anything about *what* is folding. A
 * work unit's value varies by orders of magnitude between project classes, so a GPU on
 * modern assignments sits one to two decimal orders above a CPU on small ones — which
 * makes the quotient the closest thing to a hardware reading that the upstream feed
 * permits. It is stated as a ratio and never as a hardware claim, because it is a
 * career average: a decade of CPU folding still reads as CPU the week after a new card
 * arrives.
 */
/**
 * Points per work unit — the recent window first, because the lifetime figure is not
 * the one that means anything.
 *
 * Measured across the top teams, the recent ratio runs 3× to 27× the lifetime one, for
 * every entity rather than for a few that changed hardware. Points per work unit have
 * inflated enormously over twenty years, so a career average largely measures how long
 * somebody has been folding. The recent window is what is comparable between entities,
 * since they are all facing the same work units now.
 */
function perWUText(d) {
  const wus = d.wus_total ?? 0;
  if (!wus) return n(wus);
  const recent = d.recent?.points_per_wu;
  if (!recent) return `${n(wus)} · ${n(d.points_per_wu)} points each, lifetime`;
  return `${n(wus)} · ${short(recent)}/WU now, ${short(d.points_per_wu)} lifetime`;
}

/**
 * What the x-axis of a chart at this granularity actually means.
 *
 * Hourly points are instants and render in the reader's own timezone; days and months
 * are UTC periods the server aggregates once for everyone. Saying which is which is
 * the difference between a reader trusting the chart and thinking it disagrees with
 * the clock in the header.
 */
function chartNote(granularity, rate = 'total') {
  let s;
  if (granularity === 'monthly') s = `Months are UTC. Gaps are months with no production.`;
  else if (granularity === 'weekly')
    s = `Weeks start Sunday 00:00 UTC. Gaps are weeks with no production.`;
  else if (granularity === 'daily') s = `Days are UTC. Gaps are days with no production.`;
  else s = `Times are ${tzName()}. Gaps are hours with no production.`;
  if (rate !== 'per-day') return s;
  // The in-progress bucket is divided by the part of it that has elapsed, so it is a
  // rate so far and not a total, and it moves around early in the period. Saying so
  // costs a clause and stops the newest bar reading as a crash or a spike.
  return granularity === 'hourly'
    ? `${s} Each point is its hour's output as PPD.`
    : `${s} Each bar is PPD; the one in progress is measured over the part elapsed.`;
}

/**
 * A history chart with granularity controls.
 *
 * `fetcher(params)` returns an API history response. Returns {node, destroy}.
 */
function historyCard(title, fetcher) {
  const plotEl = el('div.chart');
  const legendEl = el('div.legend');
  const chart = productionChart(plotEl);
  let granularity = 'hourly';
  let rate = 'total';

  const controls = el('div.chart-toolbar');
  const noteEl = el('div.chart-note', chartNote(granularity, rate));
  const body = el('div.card-body', { style: 'padding:0' }, legendEl, plotEl, noteEl);
  const node = cardWith(title, controls, body);

  function renderControls() {
    clear(controls).append(
      segmented(GRANULARITIES, granularity, (v) => { granularity = v; renderControls(); load(); }),
      segmented(RATES, rate, (v) => { rate = v; renderControls(); load(); })
    );
  }

  async function load() {
    noteEl.textContent = chartNote(granularity, rate);
    try {
      const res = await fetcher({ granularity, metric: 'points' });
      const pts = res.data.points || [];
      if (!pts.length) {
        chart.render(null);
        clear(legendEl).append(emptyChart(granularity));
        return;
      }
      clear(legendEl);
      const dense = densify(pts, granularity, { ...chartSpan(), perDay: rate === 'per-day' });
      const xs = dense.map((p) => Math.floor(Date.parse(p.at) / 1000));
      const ys = dense.map((p) => p.points);
      chart.render([xs, ys], { granularity, perDay: rate === 'per-day' });
    } catch (err) {
      chart.render(null);
      clear(legendEl).append(el('div.error', { style: 'padding:40px 0' }, err.message));
    }
  }

  renderControls();
  load();
  return { node, destroy: () => chart.destroy() };
}

/**
 * Who turned up in the last day.
 *
 * Every other figure on the overview is about how much is being produced. This is the
 * only one about the project still gaining people, which is a different question and
 * the one that says whether Folding@home is alive.
 *
 * Absent is not zero: before a full day has been observed there is no baseline to be
 * new since, and printing "0 new donors" then would report a measurement never taken.
 */
function arrivalsTile(d) {
  if (d.new_donors_24h === undefined || d.new_donors_24h === null) {
    return statTile('New donors', '—', `nothing to compare against yet`,
      'Donors seen for the first time in the last 24 hours');
  }
  return statTile('New donors', short(d.new_donors_24h),
    `in 24h · ${n(d.new_teams_24h)} new teams`,
    'Donors and teams seen for the first time in the last 24 hours');
}

/* ------------------------------------------------------------- overview --- */

export async function overview(view) {
  loading(view);
  const cleanups = [];
  try {
    const [sum, teams, donors] = await Promise.all([
      api.summary(),
      api.teams({ per_page: 10 }),
      api.donors({ per_page: 10 }),
    ]);
    const d = sum.data;

    clear(view);
    view.append(
      el(
        'div.page-head',
        el('div.eyebrow', 'Folding@home'),
        el('h1.page-title', 'Every donor, every team, one snapshot'),
        el('p.page-sub',
          `Tracking ${n(d.donors_total)} donors across ${n(d.teams_total)} teams. ` +
          `Updated hourly, with a free API and nothing standing in front of it.`)
      )
    );

    // The headline is one number, so it is a hero figure and not a chart.
    view.append(
      el(
        'section.section',
        el(
          'div.stats',
          statTile('Total points', el('span.hero-value', { text: short(d.points_total) }), n(d.points_total)),
          // The window is 7 days once 7 days exist. Before that it is however much
          // has been collected, and naming the nominal window would overstate what
          // this count covers.
          statTile('Active donors', short(d.donors_active),
            `of ${n(d.donors_total)} — produced in the last ${activeWindow()}`),
          statTile('Active teams', short(d.teams_active), `of ${n(d.teams_total)}`),
          statTile('Last 24 hours', short(d.points_last_24h), 'points across the project'),
          statTile('Work units', short(d.wus_total), n(d.wus_total)),
          arrivalsTile(d)
        )
      )
    );

    const hist = historyCard('Project production', (p) => api.projectHistory(p));
    cleanups.push(hist.destroy);
    view.append(el('section.section', hist.node));

    view.append(
      el(
        'div.grid-2',
        el(
          'section.section',
          el('div.section-head', el('div.section-title', 'Top teams'),
            el('a.section-link', { href: '/teams' }, 'All teams →')),
          card(null, teamTable(teams.data, { compact: true }))
        ),
        el(
          'section.section',
          el('div.section-head', el('div.section-title', 'Top donors'),
            el('a.section-link', { href: '/donors' }, 'All donors →')),
          card(null, donorTable(donors.data, { compact: true }))
        )
      )
    );
  } catch (err) {
    errorView(view, err);
  }
  return () => cleanups.forEach((f) => f());
}

/* ---------------------------------------------------------------- teams --- */

function teamTable(teams, { compact = false, sort = 'lifetime', offset = 0, onSort } = {}) {
  // The compact table on the overview has no room for nine columns and nothing to
  // sort — it is a preview of a board that lives elsewhere.
  const cols = compact
    ? COLUMNS.filter((c) => ['per_day', 'lifetime'].includes(c.key))
    : COLUMNS.filter((c) => c.kind !== 'donor');
  const ranked = sort !== 'lifetime';

  const head = el('tr',
    el('th.left', 'Rank'),
    el('th.left', 'Team'),
    ...cols.map((c) => (onSort ? sortHeader(c, sort, onSort) : el('th', { title: c.title }, c.label))));

  const body = el('tbody');
  teams.forEach((t, i) => {
    const idle = (t.points_per_day_7d_avg ?? 0) === 0;
    const nameLink = el('a', { href: `/teams/${t.team_id}` });
    tierName(nameLink, t.name, t.points_per_day_7d_avg);
    body.append(el('tr', { class: idle ? 'dim' : null },
      // Under a non-default ordering the position is the position in *that* board.
      // The lifetime rank is a different number, and showing it here would look like
      // the sort had simply not worked.
      ranked
        ? el('td.rank', { title: `Lifetime rank #${n(t.rank)}` }, n(offset + i + 1))
        : el('td.rank', n(t.rank)),
      el('td.left.name-cell', nameLink),
      ...cols.map((c) => {
        const active = c.key === sort ? '.metric' : '';
        if (c.key === 'members') {
          return el(`td.num${active}`, { title: `${n(t.members_active)} active of ${n(t.members_total)}` },
            `${short(t.members_active)} / ${short(t.members_total)}`);
        }
        return el(`td.num${active}`, { title: n(t[c.field]) }, short(t[c.field]));
      })));
  });
  return el('div.table-wrap', el('table.data', el('thead', head), body));
}

export async function teamsList(view, { page = 1, sort = 'lifetime' }, nav) {
  loading(view);
  try {
    const res = await api.teams({ page, per_page: PER_PAGE, sort });
    clear(view);
    view.append(
      el('div.page-head',
        el('h1.page-title', 'Teams'),
        el('p.page-sub', `${n(res.page.total_items)} teams, ranked by ${SORT_BLURB[sort]}.`))
    );
    view.append(
      card(null,
        teamTable(res.data, {
          sort,
          offset: (res.page.page - 1) * res.page.per_page,
          // A new ordering starts at page 1: holding position would land the reader
          // on page 40 of a board they have not seen the top of.
          onSort: (key) => nav(listHref('/teams', { sort: key }), { keepContent: true }),
        }),
        pager(res.page.page, res.page.total_pages, res.page.total_items,
          (p) => nav(listHref('/teams', { page: p, sort }), { keepContent: true })))
    );
  } catch (err) {
    errorView(view, err);
  }
}

export async function teamDetail(view, { id }, nav) {
  loading(view);
  const cleanups = [];
  try {
    const res = await api.team(id);
    const t = res.data;

    clear(view);
    const title = el('h1.page-title');
    nameText(title, t.name);
    view.append(
      el('div.page-head',
        el('div.breadcrumb', el('a', { href: '/teams' }, 'Teams'), el('span', '/'),
          el('span', `#${n(t.rank)}`)),
        title,
        el('p.page-sub',
          `Team ${n(t.team_id)} · ${plural(t.members_total, 'member')} · ` +
          `${n(t.members_active)} active in the last ${activeWindow()}`))
    );

    view.append(detailStats(t, [
      statTile('Rank', `#${n(t.rank)}`,
        rankMovement(t.rank_change_24h, 'by lifetime points'),
        'Rank by lifetime points, and places moved over the last 24 hours'),
    ]));

    const hist = historyCard('Production', (p) => api.teamHistory(t.team_id, p));
    cleanups.push(hist.destroy);
    view.append(el('section.section', hist.node));

    // Who is producing it, after the team's own total. Someone landing on a team page
    // came for "how is my team doing" before "who is doing it", so the breakdown
    // follows the headline chart rather than replacing it.
    const byMember = teamMembersChartCard(t);
    cleanups.push(byMember.destroy);
    view.append(el('section.section', byMember.node));

    view.append(el('section.section', await teamMembersCard(t.team_id, nav)));

    // Last, and deliberately. This is the only block on the page about somebody else,
    // and above the fold it competed with the team's own figures — which are what the
    // reader came for. Being last also keeps its fetch off the critical path: the
    // stats, the chart and the roster all render before it is even requested.
    view.append(await rivalsSection(
      (p) => api.teamRivals(t.team_id, p), 'team', `/teams/${t.team_id}/rivals`));
  } catch (err) {
    errorView(view, err);
  }
  return () => cleanups.forEach((f) => f());
}

async function teamMembersCard(teamID, nav) {
  let page = 1;
  let activeOnly = false;
  let sort = 'lifetime';
  const body = el('div');
  const controls = el('div.chart-toolbar');
  const node = cardWith('Members', controls, body);

  function renderControls() {
    clear(controls).append(
      segmented(
        [
          { value: 'all', label: 'All' },
          { value: 'active', label: 'Active', title: 'Produced in the last 7 days once 7 days are collected' },
        ],
        activeOnly ? 'active' : 'all',
        (v) => {
          activeOnly = v === 'active';
          page = 1;
          load();
        }
      )
    );
  }

  async function load() {
    clear(body).append(el('div', { style: 'padding:24px' }, el('div.skeleton', { style: 'height:180px' })));
    renderControls();
    try {
      const res = await api.teamMembers(teamID, {
        page, per_page: PER_PAGE, active_only: activeOnly ? 'true' : undefined,
        sort: sort === 'lifetime' ? undefined : sort,
      });
      clear(body);
      if (!res.data.length) {
        body.append(el('div.empty', activeOnly ? `No members produced in the last ${activeWindow()}.` : 'No members.'));
        return;
      }
      body.append(memberTable(res.data, sort, (key) => {
        // A new ordering is a new first page: keeping the page number would land the
        // reader somewhere arbitrary in a list they have just reordered.
        sort = key;
        page = 1;
        load();
      }));
      body.append(pager(res.page.page, res.page.total_pages, res.page.total_items, (p) => {
        page = p;
        load();
      }));
    } catch (err) {
      clear(body).append(el('div.error', err.message));
    }
  }

  await load();
  return node;
}

/**
 * The columns a team's roster can be ordered by, left to right as they appear.
 *
 * A subset of COLUMNS rather than a second list: the key is the same string the API
 * takes, so the heading, the URL and the server's ordering stay one value. Lifetime
 * points sits last and is the default, because it is the ranking a member's position
 * in the team is actually derived from.
 */
const ROSTER_COLUMNS = COLUMNS.filter((c) =>
  ['per_day', 'last_24h', 'this_week', 'this_month', 'lifetime'].includes(c.key));

function memberTable(members, sort, onSort) {
  const body = el('tbody');
  for (const m of members) {
    const idle = (m.points_per_day_7d_avg ?? 0) === 0;
    const nameLink = el('a', { href: `/donors/${encodeURIComponent(m.name)}` });
    tierName(nameLink, m.name, m.points_per_day_7d_avg);
    body.append(
      el(
        'tr',
        { class: idle ? 'dim' : null },
        el('td.rank', n(m.rank_in_team)),
        el('td.left.name-cell', nameLink),
        ...ROSTER_COLUMNS.map((c) =>
          el('td.num', { title: n(m[c.field]) }, short(m[c.field])))
      )
    );
  }
  return el(
    'div.table-wrap',
    el(
      'table.data',
      el('thead', el('tr',
        el('th.left', 'In team'), el('th.left', 'Donor'),
        ...ROSTER_COLUMNS.map((c) => sortHeader(c, sort, onSort)))),
      body
    )
  );
}


/* --------------------------------------------------------------- donors --- */

function donorTable(donors, { compact = false, sort = 'lifetime', offset = 0, onSort } = {}) {
  const cols = compact
    ? COLUMNS.filter((c) => ['per_day', 'lifetime'].includes(c.key))
    : COLUMNS.filter((c) => c.kind !== 'team');
  const ranked = sort !== 'lifetime';

  const body = el('tbody');
  donors.forEach((d, i) => {
    const idle = (d.points_per_day_7d_avg ?? 0) === 0;
    const nameLink = el('a', { href: `/donors/${encodeURIComponent(d.name)}` });
    tierName(nameLink, d.name, d.points_per_day_7d_avg);
    const cell = el('td.left.name-cell', nameLink);
    // Shared placeholder names carry no badge in the table: the team count beside
    // them already tells the story, and a warning on every top row is noise. The
    // full explanation lives on the donor's own page, where it appears once.
    if (d.likely_not_a_person) {
      cell.title = `Appears on ${n(d.team_count)} teams — almost certainly a shared default name.`;
    }
    body.append(el('tr', { class: idle ? 'dim' : null },
      ranked
        ? el('td.rank', { title: `Lifetime rank #${n(d.rank)}` }, n(offset + i + 1))
        : el('td.rank', n(d.rank)),
      cell,
      ...cols.map((c) => {
        const active = c.key === sort ? '.metric' : '';
        if (c.key === 'teams') return el(`td.num${active}`, n(d.team_count));
        return el(`td.num${active}`, { title: n(d[c.field]) }, short(d[c.field]));
      })));
  });

  return el('div.table-wrap', el('table.data',
    el('thead', el('tr',
      el('th.left', 'Rank'), el('th.left', 'Donor'),
      ...cols.map((c) => (onSort ? sortHeader(c, sort, onSort) : el('th', { title: c.title }, c.label))))),
    body));
}

export async function donorsList(view, { page = 1, sort = 'lifetime' }, nav) {
  loading(view);
  try {
    const res = await api.donors({ page, per_page: PER_PAGE, sort });
    clear(view);
    view.append(
      el('div.page-head',
        el('h1.page-title', 'Donors'),
        el('p.page-sub', `${n(res.page.total_items)} donors, ranked by ${SORT_BLURB[sort]} across every team they fold for.`))
    );
    view.append(
      card(null,
        donorTable(res.data, {
          sort,
          offset: (res.page.page - 1) * res.page.per_page,
          // A new ordering starts at page 1: holding position would land the reader
          // on page 40 of a board they have not seen the top of.
          onSort: (key) => nav(listHref('/donors', { sort: key }), { keepContent: true }),
        }),
        pager(res.page.page, res.page.total_pages, res.page.total_items,
          (p) => nav(listHref('/donors', { page: p, sort }), { keepContent: true })))
    );
  } catch (err) {
    errorView(view, err);
  }
}

export async function donorDetail(view, { name }, nav) {
  loading(view);
  const cleanups = [];
  try {
    const res = await api.donor(name);
    const d = res.data;

    clear(view);
    const title = el('h1.page-title');
    nameText(title, d.name);
    const head = el('div.page-head',
      el('div.breadcrumb', el('a', { href: '/donors' }, 'Donors'), el('span', '/'),
        el('span', `#${n(d.rank)}`)),
      title,
      el('p.page-sub',
        d.team_count === 1
          ? 'Folding for one team.'
          : `Folding for ${n(d.team_count)} teams — totals below are the sum across all of them.`));
    view.append(head);

    if (d.likely_not_a_person) {
      view.append(el('div.section', notice(
        `“${d.name}” appears on ${n(d.team_count)} teams. It is almost certainly a default or ` +
        `placeholder name shared by many different people, so these totals are an aggregate ` +
        `rather than one person's record.`)));
    }

    view.append(detailStats(d, [
      statTile('Rank', `#${n(d.rank)}`,
        rankMovement(d.rank_change_24h, 'across all donors'),
        'Rank across all donors, and places moved over the last 24 hours'),
    ]));

    const teams = d.teams || [];
    if (teams.length > 1) {
      const bd = breakdownCard(d, teams);
      cleanups.push(bd.destroy);
      view.append(el('section.section', bd.node));
    } else {
      const hist = historyCard('Production', (p) => api.donorHistory(d.name, p));
      cleanups.push(hist.destroy);
      view.append(el('section.section', hist.node));
    }

    view.append(el('section.section', teamsCard(d, teams)));

    // Last, as on a team page: the donor's own figures lead, and the comparison
    // against everyone else follows them.
    view.append(await rivalsSection(
      (p) => api.donorRivals(d.name, p), 'donor', `/donors/${encodeURIComponent(d.name)}/rivals`));
  } catch (err) {
    errorView(view, err);
  }
  return () => cleanups.forEach((f) => f());
}

/**
 * The multi-team donor view.
 *
 * "All teams" stacks per-team contribution: total output and where it went, from
 * one figure. Selecting a single team switches to that team's own line — the
 * confidence a single series carries does not survive stacking, so the two views
 * are deliberately separate rather than layered.
 */
/**
 * Production split into contributions, over time.
 *
 * Two pages ask the same question of different things. A donor's output divides among
 * the teams they fold for; a team's divides among its members. The decomposition, the
 * "Other" tail, the per-contributor tabs and both view shapes are identical either
 * way, so they are one card parameterised by what a contributor is rather than two
 * that drift.
 *
 * A contributor is { key, label, value }: `key` is whatever fetchFor needs to ask for
 * its history, `label` is what the legend and tab call it, and `value` is its recent
 * production — used only to rank and to annotate the tabs.
 */
function contributionCard({ title, noun, items, total, fetchFor, refresh, emptyDetail }) {
  const plotEl = el('div.chart');
  const legendEl = el('div.legend');
  const tabs = el('div.tabs', { role: 'tablist' });
  const controls = el('div.chart-toolbar');

  let granularity = 'hourly';
  let rate = 'total';
  let shape = 'stacked';
  let selected = 'all';
  let chart = null;

  const noteEl = el('div.chart-note', chartNote(granularity, rate));
  const body = el('div.card-body', { style: 'padding:0' }, tabs, legendEl, plotEl, noteEl);
  const node = cardWith(title, controls, body);

  // Ranked by recent production, not lifetime total. The biggest contributors by
  // lifetime are frequently dormant, so ranking this card the way the table below it
  // ranks selects precisely the ones with nothing to plot — the chart then looks
  // broken while there is demonstrably output to show.
  let shown = [];
  let rest = [];
  let producers = [];

  function setSeries(list) {
    producers = list.filter((c) => (c.value ?? 0) > 0);
    const ranked = producers.length ? producers : list;
    shown = ranked.slice(0, MAX_STACK_SERIES);
    rest = ranked.slice(MAX_STACK_SERIES);
  }
  setSeries(items || []);

  function renderTabs() {
    clear(tabs);
    // With a single producing contributor, "all" and its own tab are the same series;
    // showing both is a choice between two identical views.
    if (shown.length < 2) return;
    const mk = (key, label, val) =>
      el('button.tab', {
        role: 'tab',
        'aria-selected': selected === key ? 'true' : 'false',
        onclick: () => { selected = key; load(); },
      }, label, val ? el('span.tab-val', short(val)) : null);

    tabs.append(mk('all',
      producers.length > 1 ? `Top ${Math.min(shown.length, MAX_STACK_SERIES)} ${noun}s` : `All ${noun}s`,
      total));
    for (const c of shown) tabs.append(mk(c.key, c.label, c.value));
  }

  function renderControls(bands) {
    clear(controls).append(
      segmented(GRANULARITIES, granularity, (v) => { granularity = v; load(); }),
      segmented(RATES, rate, (v) => { rate = v; load(); })
    );
    // Only where there is something to compare. One band stacked and one band
    // overlaid are the same picture, and a control that changes nothing is worse
    // than no control at all.
    if (bands > 1) {
      controls.append(segmented(SHAPES, shape, (v) => { shape = v; load(); }));
    }
  }

  // Labels the reader has switched off, and the last multi-series result to redraw
  // from. Kept as labels rather than indices because the set outlives a reorder.
  const hidden = new Set();
  let drawn = null;

  function draw() {
    if (!drawn) return;
    const keep = drawn.labels.map((l) => !hidden.has(l));
    // Never hide everything: an empty chart is not a view of anything, and the way
    // back is the legend that just disappeared with it.
    if (!keep.some(Boolean)) {
      hidden.clear();
      keep.fill(true);
    }
    const labels = drawn.labels.filter((_, i) => keep[i]);
    const rows = drawn.rows.filter((_, i) => keep[i]);
    const colors = drawn.colors.filter((_, i) => keep[i]);

    legend(legendEl,
      drawn.labels.map((label, i) => ({ label, color: drawn.colors[i], hidden: !keep[i] })),
      (label) => {
        hidden.has(label) ? hidden.delete(label) : hidden.add(label);
        draw();
      });

    if (chart) chart.destroy();
    chart = seriesChart(plotEl);
    // Re-stacked from the visible rows, not merely hidden. The stored values are
    // cumulative, so dropping a middle band from a finished stack would leave every
    // band above it still carrying its contribution.
    const stacked = shape === 'stacked';
    chart.render([drawn.xs, ...(stacked ? stack(rows) : rows)],
      { granularity: drawn.granularity, labels, colors, stacked, perDay: drawn.perDay });
  }

  const one = (pts, label, perDay, rescaled) => {
    chart = productionChart(plotEl, label);
    if (!pts.length) {
      legendEl.append(emptyChart(granularity));
      return;
    }
    const dense = densify(pts, granularity, rescaled ? { ...chartSpan() } : { ...chartSpan(), perDay });
    chart.render(
      [dense.map((p) => Math.floor(Date.parse(p.at) / 1000)), dense.map((p) => p.points)],
      { granularity, perDay }
    );
  };

  async function load() {
    renderTabs();
    renderControls(selected === 'all' ? shown.length : 1);
    noteEl.textContent = chartNote(granularity, rate);
    if (chart) chart.destroy();
    clear(legendEl);
    drawn = null;

    const perDay = rate === 'per-day';
    const win = chartSpan();
    try {
      if (selected === 'all' && !producers.length) {
        legendEl.append(el('div.chart-empty',
          el('div', `No ${noun} has produced recently`),
          el('div', { style: 'font-size:12px;margin-top:4px' }, emptyDetail)));
        return;
      }
      // A stack of one is a line. Stacking a single series draws it as a solid block,
      // which reads as far more emphatic than the data warrants.
      if (selected === 'all' && shown.length === 1) {
        const only = shown[0];
        const res = await fetchFor(only.key, { granularity });
        one(res.data.points || [], only.label, perDay, false);
        return;
      }

      if (selected === 'all') {
        // Rescale on arrival rather than at render: the bands are summed, filtered and
        // stacked below, and a stack is only meaningful if every band is in the same
        // unit. Doing it once here is the only way that stays true down every branch.
        const fetched = await Promise.all(shown.map(async (c) => {
          const pts = (await fetchFor(c.key, { granularity })).data.points || [];
          return { c, points: perDay ? perDayPoints(pts, granularity, win.until, win.since) : pts };
        }));

        // A contributor that produced nothing *in this window* gets no band, no legend
        // entry and no tooltip row. Selecting series by the recent figure is wrong as
        // soon as the chart shows a different window — the key would name colours that
        // appear nowhere on the plot.
        const active = fetched.filter((f) => f.points.some((p) => p.points > 0));
        if (!active.length) {
          legendEl.append(emptyChart(granularity));
          return;
        }
        if (active.length === 1) {
          one(active[0].points, active[0].c.label, perDay, true);
          return;
        }

        const labels = active.map((f) => f.c.label);
        // Align every contributor onto the union of timestamps: one idle in a bucket
        // contributes zero there rather than shortening the series.
        const merged = [...new Set(active.flatMap((f) => f.points.map((p) => p.at)))]
          .sort()
          .map((at) => ({ at, points: 0, wus: 0 }));
        if (!merged.length) {
          legendEl.append(emptyChart(granularity));
          return;
        }
        const times = densify(merged, granularity, { ...chartSpan() }).map((p) => p.at);
        const idx = new Map(times.map((t, i) => [t, i]));
        const rows = active.map((f) => {
          const arr = new Array(times.length).fill(0);
          for (const p of f.points) if (idx.has(p.at)) arr[idx.get(p.at)] = p.points;
          return arr;
        });
        if (rest.length) {
          // Everything past the slot count, as one band — but only if it actually
          // produced in this window.
          const others = await Promise.all(
            rest.slice(0, 8).map((c) => fetchFor(c.key, { granularity })));
          const arr = new Array(times.length).fill(0);
          for (const r of others) {
            const pts = r.data.points || [];
            for (const p of perDay ? perDayPoints(pts, granularity, win.until, win.since) : pts) {
              if (idx.has(p.at)) arr[idx.get(p.at)] += p.points;
            }
          }
          if (arr.some((v) => v > 0)) {
            rows.push(arr);
            labels.push(`Other (${rest.length})`);
          }
        }

        const colours = palette();
        // Held so a legend click redraws from what is already fetched. Hiding a band
        // is a question about the same data, not a different one.
        drawn = {
          xs: times.map((t) => Math.floor(new Date(t).getTime() / 1000)),
          rows,
          labels,
          // Pinned by position in the full set, so a colour stays with its team or
          // member however many of its neighbours are hidden.
          colors: labels.map((_, i) => colours[i % colours.length]),
          perDay,
          granularity,
        };
        draw();
      } else {
        const res = await fetchFor(selected, { granularity });
        one(res.data.points || [], undefined, perDay, false);
      }
    } catch (err) {
      legendEl.append(el('div.error', { style: 'padding:40px 0' }, err.message));
    }
  }

  (async () => {
    if (refresh) {
      try {
        const fresh = await refresh(producers);
        if (fresh && fresh.length) setSeries(fresh);
      } catch {
        // Fall back to whatever we started with; a worse ordering, not a broken one.
      }
    }
    load();
  })();
  return { node, destroy: () => chart && chart.destroy() };
}

/** A donor's production, split across the teams they fold for. */
function breakdownCard(donor, teams) {
  const item = (t) => ({
    key: String(t.team_id),
    label: t.team_name || `Team ${t.team_id}`,
    value: t.points_last_7d ?? 0,
  });
  return contributionCard({
    title: 'Production by team',
    noun: 'team',
    items: (teams || []).map(item),
    total: donor.points_last_7d,
    emptyDetail: `${n(donor.team_count)} teams on record, none with output in the last 7 days.`,
    fetchFor: (key, p) => api.donorHistory(donor.name, { ...p, team_id: key }),
    // The embedded breakdown is capped and ordered by lifetime points, so a donor's
    // actual producers can fall outside it entirely. Ask for them explicitly.
    refresh: async (found) => {
      if (!donor.teams_truncated && found.length) return null;
      return (await api.donorTeams(donor.name, { sort: 'production', per_page: 20 })).data.map(item);
    },
  });
}

/**
 * A team's production, split across its members.
 *
 * The share the top few hold is itself the finding, and it varies enormously: six
 * people are 84% of one team on this site and 36% of another. "This team is six
 * people" and "this team is four hundred people and nobody carries it" are different
 * facts about a team that nothing else on the page tells you.
 *
 * A member's history is a donor's history scoped to one team, which is the same
 * request the donor card makes with the other half fixed.
 */
function teamMembersChartCard(team) {
  return contributionCard({
    title: 'Production by member',
    noun: 'member',
    items: [],
    total: team.points_last_7d,
    emptyDetail: `${n(team.members_total)} members on record, none with output in the last 7 days.`,
    fetchFor: (name, p) => api.donorHistory(name, { ...p, team_id: team.team_id }),
    // Always fetched: the team payload carries no roster, and the members worth
    // plotting are the recent producers rather than the lifetime leaders.
    refresh: async () => {
      const res = await api.teamMembers(team.team_id, {
        sort: 'per_day', per_page: MAX_STACK_SERIES + 8,
      });
      return res.data.map((m) => ({ key: m.name, label: m.name, value: m.points_last_7d ?? 0 }));
    },
  });
}

function teamsCard(donor, teams) {
  const body = el('tbody');
  for (const t of teams) {
    const link = el('a', { href: `/teams/${t.team_id}` });
    tierName(link, t.team_name || `Team ${t.team_id}`, t.points_per_day_7d_avg);
    body.append(
      el(
        'tr',
        el('td.left.name-cell', link),
        el('td.num', `#${n(t.rank_in_team)}`),
        el('td.num', { title: n(t.points_per_day_7d_avg) }, short(t.points_per_day_7d_avg)),
        el('td.num', { title: n(t.points_total) }, short(t.points_total))
      )
    );
  }
  const table = el(
    'div.table-wrap',
    el('table.data',
      el('thead', el('tr',
        el('th.left', 'Team'), el('th', 'Rank in team'), el('th', 'Per day'), el('th', 'Points'))),
      body)
  );
  const node = card(`Teams (${n(donor.team_count)})`, table);
  if (donor.teams_truncated) {
    node.append(el('div', { style: 'padding:12px 24px' },
      notice(`Showing the ${teams.length} highest-scoring teams of ${n(donor.team_count)}.`)));
  }
  return node;
}

/* --------------------------------------------------------------- search --- */

/**
 * The rivals view on a page of its own.
 *
 * The card on the detail page is where people find this; the page is where they send
 * it from. "Four days off passing them" is a thing worth linking to, and a link into
 * the middle of somebody else's team page is not that.
 */
export async function rivalsPage(view, { kind, id, page }, nav) {
  loading(view);
  const load = (p) => (kind === 'team' ? api.teamRivals(id, p) : api.donorRivals(id, p));
  try {
    // One fetch up front so the heading can name the subject before the card
    // renders; the card then owns paging from there.
    const res = await load({ page, per_page: RIVALS_PER_PAGE });
    const d = res.data;
    clear(view);

    const back = kind === 'team' ? `/teams/${id}` : `/donors/${encodeURIComponent(id)}`;
    const title = el('h1.page-title');
    nameText(title, d.name);
    view.append(el('div.page-head',
      el('div.breadcrumb',
        el('a', { href: kind === 'team' ? '/teams' : '/donors' }, kind === 'team' ? 'Teams' : 'Donors'),
        el('span', '/'), el('a', { href: back }, `#${n(d.rank)}`),
        el('span', '/'), el('span', 'Rivals')),
      title,
      el('p.page-sub', `Ranked #${n(d.rank)}. Who is within reach, and who is within reach of them.`)));

    // The page rides in the URL here so a link carries the view the sender was
    // looking at — the whole reason this exists as a page and not only a card.
    const base = `${back}/rivals`;
    const c = rivalsCard(load, kind, {
      onPage: (p) => history.replaceState(null, '', p > 1 ? `${base}?page=${p}` : base),
    });
    view.append(el('section.section', c.node));
    await c.go(page);
  } catch (err) {
    errorView(view, err);
  }
}

export async function searchPage(view, { q }, nav) {
  loading(view);
  try {
    const res = await api.search(q, undefined, { limit: 50 });
    const { teams, donors } = res.data;
    clear(view);
    view.append(
      el('div.page-head',
        el('h1.page-title', `Results for “${q}”`),
        el('p.page-sub', 'Exact matches first, then names starting with what you typed. ',
          'Short names like ', el('code', 'DH'), ' work.'))
    );
    if (!teams.length && !donors.length) {
      view.append(card(null, el('div.empty',
        el('div', { style: 'margin-bottom:6px' }, `Nothing matches “${q}”.`),
        el('div.muted', 'Try fewer characters, or a numeric team ID.'))));
      return;
    }
    if (donors.length) view.append(el('section.section',
      el('div.section-head', el('div.section-title', 'Donors')), card(null, donorTable(donors))));
    if (teams.length) view.append(el('section.section',
      el('div.section-head', el('div.section-title', 'Teams')), card(null, teamTable(teams))));
  } catch (err) {
    errorView(view, err);
  }
}

/* ------------------------------------------------------------------ api --- */

export async function apiDocs(view) {
  clear(view);
  const snap = await api.status().catch(() => null);
  const base = location.origin;

  // Placeholders resolve to entities that exist in every corpus, so each row is a
  // link you can actually follow: team 0 is "Default (No team specified)", the
  // largest team, and Anonymous is the most widely shared donor name. The column
  // shows the template, because that is the documentation; the href is a worked
  // example, and the title says where it goes for anyone who wants to know before
  // clicking.
  //
  // This previously read `.replace(/\{[^}]+\}/g, (m) => m)` — a substitution that
  // returns the match unchanged — so every templated row linked to a literal
  // `/v1/donors/{name}` and 404'd.
  const EXAMPLES = { '{id}': '0', '{name}': 'Anonymous' };
  const exampleOf = (path) => {
    const p = path.replace(/\{[^}]+\}/g, (m) => EXAMPLES[m] ?? m);
    if (p === '/v1/search') return p + '?q=Anonymous';
    // The changes feed needs a cursor to be a working example at all. The snapshot
    // time is the right one, but this page can be the first thing a visitor loads and
    // there is no snapshot until something has been fetched — so fall back to an hour
    // ago by the reader's own clock, which is inside the window unless their machine
    // is a week out.
    if (p === '/v1/changes') {
      return p + `?since=${snapshot()?.at ?? new Date(Date.now() - 3600e3).toISOString()}`;
    }
    return p;
  };

  const endpoint = (method, path, desc) => {
    const ex = exampleOf(path);
    return el('tr',
      el('td.left', el('code', { style: 'color:var(--series-3)' }, method)),
      el('td.left', el('a', { href: base + ex, title: `Example: ${ex}` }, el('code', path))),
      el('td.left.muted', desc));
  };

  view.append(
    el('div.page-head',
      el('h1.page-title', 'API'),
      el('p.page-sub',
        'Free, public, and unauthenticated. No key, no sign-up, and nothing your browser has to solve first.'))
  );

  view.append(el('section.section', el('div.stats',
    statTile('Auth', 'None', 'no key required'),
    statTile('Rate limit', 'None yet', 'and not before it is needed'),
    statTile('Format', 'JSON', 'one envelope for every route'),
    statTile('Refresh', 'Hourly', 'matching upstream')
  )));

  view.append(el('section.section', card('Endpoints',
    el('div.table-wrap', el('table.data',
      el('thead', el('tr', el('th.left', 'Method'), el('th.left', 'Path'), el('th.left', ''))),
      el('tbody',
        endpoint('GET', '/v1/summary', 'Project-wide totals'),
        endpoint('GET', '/v1/status', 'Snapshot and corpus size'),
        endpoint('GET', '/v1/summary/history', 'Project-wide production over time'),
        endpoint('GET', '/v1/teams', 'Team leaderboard, paginated. ?sort= any numeric column'),
        endpoint('GET', '/v1/teams/{id}', 'One team'),
        endpoint('GET', '/v1/teams/{id}/members', 'Team roster, ?active_only=true, ?sort= any numeric column'),
        endpoint('GET', '/v1/teams/{id}/history', '?granularity=hourly|daily|weekly|monthly'),
        endpoint('GET', '/v1/teams/{id}/rivals', 'Ranking around this team with projected overtakes; opens on its own page'),
        endpoint('GET', '/v1/donors', 'Donor leaderboard, paginated. ?sort= any numeric column'),
        endpoint('GET', '/v1/donors/{name}', 'Per-team breakdown, ?sort=production'),
        endpoint('GET', '/v1/donors/{name}/teams', 'Full team list, paginated'),
        endpoint('GET', '/v1/donors/{name}/history', '?team_id= to scope to one team, same granularities'),
        endpoint('GET', '/v1/donors/{name}/rivals', 'Ranking around this donor with projected overtakes; opens on its own page'),
        endpoint('GET', '/v1/search', '?q= name prefix, exact name, or team ID'),
        endpoint('GET', '/v1/changes', 'Only what moved. ?since= a snapshot time, ?kind=teams|donors|members')
      ))))));

  view.append(el('section.section', card('MCP — for AI agents',
    el('div.card-body',
      el('p',
        'There is a Model Context Protocol server at ', el('code', '/mcp'),
        // Counted, not stated. This sentence said "seven" for as long as there were
        // eleven — the same drift the note above MCP_TOOLS describes, in the one
        // place that fix did not reach.
        ` — ${MCP_TOOLS.length} tools shaped like questions rather than like the routes ` +
        'above, so an agent can ask "is my team catching them?" in one call instead of five. ',
        link('/agents', 'How to connect →')),
      el('p',
        'If you are writing a program rather than an agent, the REST API on this page ' +
        'is the better interface: cheaper, paginated, and structured JSON rather than ' +
        'text meant to be read.')))));

  // A pointer, not a copy. The command list lives on /bots, and restating it here is
  // how the tool count on this page came to say seven while the server served eleven.
  view.append(el('section.section', card('Chat bots',
    el('div.card-body',
      el('p',
        'If you would rather ask than call: there is a Discord bot with slash commands ' +
        'for everything on this site, and it reads these same endpoints. ',
        link('/bots', 'Commands and install →'))))));

  view.append(el('section.section', card('Rate limits',
    el('div.card-body',
      el('p',
        'There are none, and adding one is a last resort rather than a plan. ' +
        'Applications that call this heavily are often the most useful ones to the ' +
        'community, and a limit set defensively would break them before they were ' +
        'ever a problem.'),
      el('p',
        'The bar for changing that is deliberately concrete: API calls consistently ' +
        'taking over half a second, with no optimisation left that would bring them ' +
        'down. Today every route answers in well under a millisecond, and load has ' +
        'been met by making the slow path fast rather than by turning callers away. ' +
        'That is the order those two things will always be tried in.'),
      el('p',
        'If it ever does become necessary it will be as lenient as it can be, and ' +
        'aimed at whatever is actually causing the problem rather than at everyone — ' +
        'most likely per-address, engaging only for traffic heavy enough to degrade ' +
        'the service for other people, and generous even then.'),
      el('p',
        'One ask, which makes all of the above easier to keep to: if you are building ' +
        'a site on this, pass your users’ requests through rather than mirroring ' +
        'the whole dataset on a schedule. Copying every team and every donor every ' +
        'hour costs far more than serving the pages people actually open, and it is ' +
        'the one pattern likely to make a limit necessary. Every response carries a ',
        el('code', 'snapshot'), ' block with ', el('code', 'next_expected_at'),
        ', so you can cache precisely instead of polling.')))));

  view.append(el('section.section', card('Sorting leaderboards',
    el('div.card-body',
      el('p',
        el('code', '?sort='), ' on ', el('code', '/v1/teams'), ' and ', el('code', '/v1/donors'),
        ' orders by any numeric column, descending, defaulting to ', el('code', 'lifetime'),
        '. Every key names the field it sorts by, so the column you see and the key you ' +
        'send are the same thing. Under a non-default ordering ', el('code', 'rank'),
        ' stays the lifetime rank — a row\u2019s position for that ordering is its index ' +
        'in the page.')),
    el('div.table-wrap', el('table.data',
      el('thead', el('tr', el('th.left', 'sort'), el('th.left', 'Column'), el('th.left', 'Field'))),
      el('tbody',
        ...[
          ['lifetime', 'Points', 'points_total — the default'],
          ['per_day', 'Per day', 'points_per_day_7d_avg — the 7-day average'],
          ['today', 'Today', 'points_today_utc — calendar day, resets 00:00 UTC'],
          ['this_week', 'This week', 'points_this_week_utc — resets Sunday 00:00 UTC'],
          ['this_month', 'This month', 'points_this_month_utc — resets on the 1st, UTC'],
          ['last_24h', 'Last 24h', 'points_last_24h — rolling, not a calendar bucket'],
          ['wus', 'WUs', 'wus_total'],
          ['members', 'Members', 'members_total — teams only'],
          ['teams', 'Teams', 'team_count — donors only'],
        ].map(([k, col, field]) =>
          el('tr',
            el('td.left', el('code', k)),
            el('td.left', col),
            el('td.left.muted', field)))))),
    el('div.card-body', { style: 'border-top:1px solid var(--line)' },
      el('p', { style: 'margin:0' }, el('span.muted',
        'daily, weekly and monthly are still accepted as aliases for today, this_week ' +
        'and this_month. They were the first published names, from before per_day ' +
        'existed \u2014 at which point “daily” became ambiguous with it.'))))));

  view.append(el('section.section', card('Notes',
    el('div.card-body',
      el('p',
        el('strong', 'Every response carries a '), el('code', 'snapshot'),
        ' block with the upstream publish time and when the next one is due. ' +
        'Cache against it rather than polling.'),
      el('p',
        el('strong', 'Donors are aggregated across teams. '),
        'One name folding for three teams is one donor whose totals are the sum, with the ' +
        'per-team breakdown included in the same response.'),
      el('p',
        el('strong', 'Field names say what they mean. '),
        el('code', 'points_per_day_7d_avg'),
        ' is the last 7 days divided by 7 — the figure other sites label “24hr avg”, which it is not.'),
      el('p',
        el('strong', 'Mirroring? Use '), el('code', '/v1/changes'), el('strong', ', not the collections. '),
        'About 1,100 members produce in any given hour, out of 2.7 million — so ',
        el('code', '/v1/changes?since=<the snapshot.at you already hold>'),
        ' is roughly a twentieth of one percent of what crawling everything costs, and it ' +
        'is the same rows through the same builders. ', el('code', 'kind'),
        ' selects teams, donors or members; the bound is exclusive, so passing back the ',
        el('code', 'snapshot.at'),
        ' of your last response is the whole cursor and there is no state to keep on either ' +
        'side. It reaches seven days back — past that a full crawl is genuinely cheaper, and ' +
        'the endpoint says so rather than serving you a diff that costs more than the thing ' +
        'it replaces.'),
      el('p',
        el('strong', 'A streak can only be as long as the record. '),
        el('code', 'streak.current'),
        ' counts consecutive UTC days with production, and survives a today that has ' +
        'not finished — a day still in progress cannot have been missed. When ',
        el('code', 'at_collection_floor'),
        ' is set the run reaches the first day this service recorded anything, so the ' +
        'figure is a lower bound and not a fact about the entity: somebody who has folded ' +
        'daily since the nineties reports the age of this site. For a donor a day counts ' +
        'once however many teams they folded for.'),
      el('p',
        el('strong', 'Standing is a share of the field, and only on detail responses. '),
        el('code', 'standing.lifetime.top_percent'), ' and ',
        el('code', 'standing.this_month.top_percent'),
        ' count downward — smaller is better — and each carries the ', el('code', 'of'),
        ' it was taken against, because the two denominators differ: lifetime is every ' +
        'entity tracked, while this month is only those that produced this month. ',
        el('code', 'this_month'), ' is absent for an entity that produced nothing, which ' +
        'is not last place. Collections omit ', el('code', 'standing'),
        ' entirely: within a page of the top fifty it is the same number fifty times.'),
      el('p',
        el('strong', 'Compare points per work unit recently, never lifetime. '),
        el('code', 'recent.points_per_wu'),
        ' on detail responses is the ratio over the last 30 days, and it is the one worth ' +
        'reading: GPU projects pay orders of magnitude more per work unit than CPU ones, and ' +
        'every entity is facing the same work units now, so the figure is comparable between ' +
        'them. ', el('code', 'points_per_wu'),
        ' is the lifetime ratio, and it runs 3× to 27× lower for essentially everybody — not ' +
        'because their hardware changed, but because points per work unit have inflated ' +
        'enormously over the project’s twenty years. It tracks how long an entity has ' +
        'folded far more than what it folds with. ', el('code', 'recent.days'),
        ' says how much of the 30-day window actually exists, since it cannot reach back past ' +
        'the start of collection, and ', el('code', 'recent'),
        ' is absent entirely for an entity that produced nothing in it.'),
      el('p',
        el('strong', 'Calendar buckets are UTC, and weeks start Sunday. '),
        el('code', 'points_today_utc'), ', ', el('code', 'points_this_week_utc'), ' and ',
        el('code', 'points_this_month_utc'),
        ' reset on their UTC boundary, as do the ', el('code', 'weekly'), ' and ',
        el('code', 'monthly'), ' history buckets and the leaderboard ',
        el('code', 'sort'), ' orderings. They are calendar periods, not rolling windows: ',
        el('code', 'points_today_utc'), ' reads low just after midnight, while ',
        el('code', 'points_last_24h'), ' is the rolling figure.'),
      el('p',
        el('strong', 'Overtakes are projections, not measurements. '),
        el('code', 'overtake_days'), ' on ', el('code', '/rivals'),
        ' is when two entities would swap places if both held their current per-day ' +
        'average forever. Nobody does. It is null when the one behind is not gaining, ' +
        'or would not arrive inside ', el('code', 'horizon_days'),
        ' — the common case, and not an error.'),
      el('p',
        el('strong', 'Rank movement is absent, not zero, when unknown. '),
        el('code', 'rank_change_24h'),
        ' is places gained over the last 24 hours, negative for places lost. It is omitted ' +
        'entirely when there is nothing to compare against — the entity is newer than a day, ' +
        'or the service has not yet watched for one. Zero means the rank genuinely held; ',
        el('code', 'warming_up.rank_change_24h_unavailable'),
        ' says when nobody can be compared yet.'),
      el('p',
        el('strong', 'Upstream does not publish on the hour. '),
        'The measured interval is about ', el('code', '3610s'), ' and drifts later every ' +
        'cycle, so a day usually has 24 hourly buckets and occasionally 23. Use ',
        el('code', 'next_expected_at'),
        ' rather than assuming a fixed schedule — it comes from the measured cadence, ' +
        'and subtracting ', el('code', 'at'), ' gives the interval.'),
      el('p',
        el('strong', 'Clocks disagree. '), 'Every response includes ', el('code', 'server_time'),
        '. Compare timestamps against that rather than your own clock, and a countdown ' +
        'stays right on a machine whose clock is minutes off.'),
      el('p',
        el('strong', 'But it is when the response was built, not now. '),
        'Every route except ', el('code', '/v1/status'), ' is cacheable, and ',
        el('code', 'server_time'), ' rides inside the cached body — so a stored copy ' +
        'replays the reading it was built with. ', el('code', 'Age'),
        ' says how long it has been held, so now is ', el('code', 'server_time'),
        ' plus ', el('code', 'Age'), '. Or read ', el('code', '/v1/status'),
        ', which is never cached.'),
      el('p',
        el('strong', 'Names are raw upstream text. '),
        'They may contain tabs, newlines and non-ASCII; URL-encode them in paths.')),
      el('div.card-body', { style: 'border-top:1px solid var(--line)' },
        el('p', { style: 'margin:0' },
          'This site is open source under the MIT license — collector, storage engine, ' +
          'API and frontend. ',
          el('a', {
            href: 'https://github.com/exec/folding-stats',
            target: '_blank', rel: 'noopener noreferrer',
          }, 'github.com/exec/folding-stats'),
          '. Run your own instance if you would rather.')))));

  if (snap) {
    view.append(el('section.section', card('Live response',
      el('div.card-body',
        el('pre', {
          style: 'margin:0;overflow-x:auto;font-family:var(--mono);font-size:12px;color:var(--ink-secondary)',
        }, JSON.stringify(snap, null, 2))))));
  }
}

/* -------------------------------------------------------------- policies --- */

// The policy pages describe what this site actually does, verified against the
// running system rather than adapted from a template: nginx's log_format and its
// logrotate retention, fail2ban's dbpurgeage, the absence of any request logging at
// the origin, and the absence of cookies, third-party scripts and analytics. A policy
// that overstates collection is as wrong as one that understates it, and this one is
// unusually easy to keep honest because there is so little to describe.
const POLICY_UPDATED = '5 August 2026';

/** A titled block of prose paragraphs. */
function policySection(title, ...paras) {
  return el('section.section', card(title,
    el('div.card-body',
      ...paras.map((p) => el('p',
        ...(Array.isArray(p) ? p : [p]))))));
}

function policyHead(view, title, sub) {
  clear(view);
  view.append(el('div.page-head',
    el('h1.page-title', title),
    el('p.page-sub', sub),
    el('p.page-sub', { style: 'font-size:12px' }, `Last updated ${POLICY_UPDATED}.`)));
}

export async function privacyPage(view) {
  policyHead(view, 'Privacy',
    'What this site collects, why, and for how long. It is a short list.');

  view.append(policySection('The short version',
    'There are no accounts, no cookies, no tracking scripts, no analytics and no ' +
    'third-party requests of any kind. Your browser talks to this site and to ' +
    'nothing else. The only personal data handled is what any web server ' +
    'unavoidably sees: the address your request came from.'));

  view.append(policySection('What is logged',
    ['The reverse proxy in front of this site writes one line per request, ' +
     'containing your IP address, the time, the hostname and path requested, the ' +
     'response status and size, the referring page if your browser sent one, your ' +
     'browser’s user-agent string, and an approximate location.'],
    ['That location is looked up from your IP address in a local database — country, ' +
     'region, city and the city’s coordinates. It is not read from your device, ' +
     'it is not GPS, and it is about as precise as the city you appear to be in, ' +
     'which for many people is not the one they are actually in.'],
    ['The application itself logs no requests at all. It records only its own ' +
     'operation: when it fetched from Folding@home, how long a cycle took, how many ' +
     'rows changed. Nothing in those lines relates to a visitor.']));

  view.append(policySection('How long it is kept',
    ['Request logs are rotated daily and deleted after 14 days. There is no archive ' +
     'and no backup of them anywhere else.'],
    ['Addresses that trip an abuse rule are recorded separately by the blocking ' +
     'service and discarded after one day.']));

  view.append(policySection('What it is used for',
    ['Understanding traffic: how much there is, where it comes from, which endpoints ' +
     'get used, whether something is broken. Deciding whether a rate limit ever ' +
     'becomes necessary — see the ',
     link('/api', 'API page'),
     ' for the position on that, which is that there is none and adding one is a last ' +
     'resort. And blocking abuse when something hammers the service hard enough to ' +
     'degrade it for other people.'],
    ['That is the complete list. The logs are not sold, shared, published, or used to ' +
     'build a profile of anyone, and no advertising or marketing exists here to use ' +
     'them for.']));

  view.append(policySection('Cloudflare',
    ['This site sits behind Cloudflare, which serves it, caches it and absorbs ' +
     'attacks. Every request therefore passes through their network before reaching ' +
     'this server, and they process it under their own terms and retention.'],
    ['That is not an arrangement this site can see inside of or speak for. ',
     el('a', {
       href: 'https://www.cloudflare.com/privacypolicy/',
       target: '_blank', rel: 'noopener noreferrer',
     }, 'Cloudflare’s privacy policy'),
     ' covers what they do with it.']));

  view.append(policySection('What is stored in your browser',
    ['One thing: whether you chose the light or dark theme. It stays on your device, ' +
     'is never sent anywhere, and clearing your site data removes it.'],
    ['No cookies are set by this site, so there is no cookie banner to dismiss.']));

  view.append(policySection('Donor and team names',
    ['The names and statistics shown here come from the public statistics feeds that ' +
     'Folding@home publishes. This site mirrors that data and derives rates and ' +
     'rankings from it; it does not create it, and it has no way to identify the ' +
     'person behind a donor name.'],
    ['If you want a name changed or removed, that has to happen at Folding@home — ' +
     'their feed is the source, and anything removed there stops appearing here at ' +
     'the next update. If that is not working for you, get in touch anyway and it can ' +
     'be looked at.']));

  view.append(policySection('Getting in touch',
    ['Questions, corrections, or a request about your own data: ',
     el('a', { href: 'mailto:privacy@exec.codes' }, 'privacy@exec.codes'),
     '. A request about a donor name is easier to act on if it says which name.'],
    ['Anything that does not need to be private is welcome as an issue at ',
     el('a', {
       href: 'https://github.com/exec/folding-stats/issues',
       target: '_blank', rel: 'noopener noreferrer',
     }, 'github.com/exec/folding-stats'),
     ' instead, where the answer is visible to the next person who wonders the same ' +
     'thing.'],
    ['Security problems have their own address — ',
     el('a', { href: 'mailto:security@exec.codes' }, 'security@exec.codes'),
     ', also published at ',
     el('a', { href: '/.well-known/security.txt' }, '/.well-known/security.txt'),
     '. Please use it rather than a public issue for anything exploitable.']));
}

export async function disclaimerPage(view) {
  policyHead(view, 'Disclaimer',
    'What this site is, what it is not, and what it does not promise.');

  view.append(policySection('Not affiliated with anyone',
    ['This site is not run by, endorsed by, or connected to the Folding@home project ' +
     '— today based at the University of Pennsylvania, after Stanford and Washington ' +
     'University in St. Louis before it — nor to ExtremeOverclocking, nor to any team ' +
     'listed on it.'],
    ['It is an independent mirror of the statistics Folding@home publishes. The ' +
     'underlying data is theirs and they give it away; the derived figures, the ' +
     'rankings and the mistakes in them are this site’s own.']));

  view.append(policySection('The data is provided as is',
    ['Everything here is offered without warranty of any kind — no guarantee that it ' +
     'is accurate, current, complete, or fit for any purpose you have in mind.'],
    ['Folding@home publishes cumulative totals. Every rate, ranking, delta and ' +
     'projection on this site is derived by comparing one published snapshot against ' +
     'the next, which means an upstream error, a missed publish or an outage here ' +
     'propagates into the derived figures. Where a figure is not yet reliable the ' +
     'site says so rather than presenting it as settled.'],
    ['History begins on 3 August 2026, when collection started. Lifetime totals come ' +
     'from upstream and go back to the beginning; anything requiring a comparison ' +
     'between two moments only exists from that date onward.']));

  view.append(policySection('Projections are guesses',
    ['Where the site says when one team or donor might overtake another, it is ' +
     'assuming both hold their current rate forever. Nobody does. It is a guess ' +
     'presented as a date, it is rounded to admit that, and it should not be treated ' +
     'as a forecast of anything.']));

  view.append(policySection('No guarantee of availability',
    ['This is a free service run by one person on hardware that is not a data centre. ' +
     'It may be slow, unavailable, or discontinued, with or without notice. The API ' +
     'may change; breaking changes will be avoided where reasonably possible, but ' +
     'nothing here is a commitment to a stable interface forever.'],
    ['If you build something on it, build it to tolerate the site being down.']));

  view.append(policySection('Limitation of liability',
    ['To the fullest extent permitted by law, the operator of this site accepts no ' +
     'liability for any loss or damage arising from use of it or reliance on anything ' +
     'it shows — including decisions made on the basis of a figure that turns out to ' +
     'be wrong, and including any interruption, error, or discontinuation of the ' +
     'service or its API.'],
    ['Nothing here is advice of any kind. It is a scoreboard for a distributed ' +
     'computing project.']));

  view.append(policySection('If something here is wrong',
    ['Report it. A figure that looks wrong usually is, and the ones worth fixing are ' +
     'found by the people who know what their own numbers should say.'],
    ['General corrections: ',
     el('a', {
       href: 'https://github.com/exec/folding-stats/issues',
       target: '_blank', rel: 'noopener noreferrer',
     }, 'an issue on GitHub'),
     ', or ', el('a', { href: 'mailto:dylan@exec.codes' }, 'dylan@exec.codes'),
     '. Anything about personal data goes to ',
     el('a', { href: 'mailto:privacy@exec.codes' }, 'privacy@exec.codes'),
     ', and anything exploitable to ',
     el('a', { href: 'mailto:security@exec.codes' }, 'security@exec.codes'),
     '.']));

  view.append(policySection('The software',
    ['The site is open source under the MIT license, which carries its own warranty ' +
     'disclaimer covering the code itself. That is a separate thing from this page, ' +
     'which is about the service and the data. ',
     el('a', {
       href: 'https://github.com/exec/folding-stats',
       target: '_blank', rel: 'noopener noreferrer',
     }, 'github.com/exec/folding-stats'),
     '.']));
}

/* ---------------------------------------------------------------- agents --- */

/**
 * The tools, as the server declares them.
 *
 * Written by hand rather than fetched from tools/list, because the descriptions there
 * are written to help a model choose between tools and run to several sentences each —
 * a wall of text in a table somebody is skimming. These are one line apiece.
 *
 * The cost of writing them twice is that they drift, which they immediately did: four
 * tools were added and this list was not, so the page confidently documented seven of
 * eleven. A test now compares this list against the server's, which is the only reason
 * keeping a second copy is defensible.
 */
const MCP_TOOLS = [
  ['search', 'query, limit?',
   'Find donors and teams by name, or a team by number. The entry point: everything ' +
   'else needs an exact name or id, and donor names are not unique.'],
  ['get_donor', 'name',
   'One donor: lifetime total, rank, rate, 24-hour rank movement, and every team ' +
   'they fold for.'],
  ['get_team', 'team_id, members?, sort?',
   'One team: totals, rank, active member count, how concentrated its output is, and ' +
   'its top contributors by any column.'],
  ['leaderboard', 'kind, sort?, limit?',
   'Top teams or donors by any column — lifetime, per_day, today, this_week, ' +
   'this_month, last_24h, wus.'],
  ['production_history', 'scope, team_id?, donor?, granularity?',
   'Production over time for the project, one team, or one donor, bucketed hourly, ' +
   'daily, weekly or monthly.'],
  ['compare', 'kind, a, b',
   'Two teams or donors head to head: the gap, who is gaining, and roughly when one ' +
   'would pass the other.'],
  ['rivals', 'kind, who, span?',
   'Who is directly ahead and directly behind in the rankings, with the gap to each ' +
   'and when the order would swap. For "who am I about to pass".'],
  ['team_activity', 'team_id, limit?',
   'What changed on a roster: members who produced all week and have stopped, members ' +
   'far above their own average, and who joined in the last day.'],
  ['movers', 'kind, direction?, within?, limit?',
   'The biggest 24-hour rank movements near the top of a ranking, where a place gained ' +
   'still takes real production.'],
  ['what_would_it_take', 'kind, who, target_rank? | target_points? | overtake?, by?',
   'The daily rate a goal would need, accounting for the target moving too — the ' +
   'inverse of compare.'],
  ['project_status', '—',
   'Project totals and freshness: donors, teams, active counts, arrivals in the last ' +
   'day, and when the next update is due.'],
];

/**
 * How to point each client at this server.
 *
 * Worth writing out per client rather than showing one JSON blob and waving at it,
 * because there is no agreement on the shape. The same remote server is `url` in
 * Cursor, `serverUrl` in Windsurf and `httpUrl` in Gemini CLI — where a plain `url`
 * means SSE and quietly does not work. VS Code keys the whole block on `servers`
 * rather than `mcpServers`, and both it and Cline need an explicit transport, which
 * Cline defaults to legacy SSE without.
 *
 * Six near-identical snippets is more page than one generic one, and it is the
 * difference between a reader connecting and a reader debugging.
 */
function mcpClients(origin) {
  const url = `${origin}/mcp`;
  const block = (o) => JSON.stringify(o, null, 2);
  return [
    {
      id: 'claude-code',
      label: 'Claude Code',
      steps: [{ text: 'One command:', code: `claude mcp add --transport http folding ${url}` }],
      note: ['Then ', el('code', '/mcp'), ' inside Claude Code to confirm it connected.'],
    },
    {
      id: 'claude',
      label: 'Claude Desktop',
      steps: [{ text: 'No config file. Settings → Connectors → Add custom connector, then paste:', code: url }],
      note: ['The same flow works on claude.ai. There is no key to enter — leave the auth fields empty.'],
    },
    {
      id: 'cursor',
      label: 'Cursor',
      steps: [{
        text: 'In ~/.cursor/mcp.json, or .cursor/mcp.json for one project:',
        code: block({ mcpServers: { folding: { url } } }),
      }],
      note: ['Cursor infers the transport from the ', el('code', 'url'), ' key; there is no type field.'],
    },
    {
      id: 'vscode',
      label: 'VS Code',
      steps: [{
        text: 'In .vscode/mcp.json, or the user config via “MCP: Open User Configuration”:',
        code: block({ servers: { folding: { type: 'http', url } } }),
      }],
      note: ['Two things differ here: the block is keyed ', el('code', 'servers'), ', not ',
        el('code', 'mcpServers'), ', and ', el('code', 'type'), ' is required.'],
    },
    {
      id: 'windsurf',
      label: 'Windsurf',
      steps: [{
        text: 'In ~/.codeium/windsurf/mcp_config.json:',
        code: block({ mcpServers: { folding: { serverUrl: url } } }),
      }],
      note: ['Windsurf spells it ', el('code', 'serverUrl'), '. A plain ', el('code', 'url'),
        ' is read as a local command and fails.'],
    },
    {
      id: 'gemini',
      label: 'Gemini CLI',
      steps: [{
        text: 'In ~/.gemini/settings.json:',
        code: block({ mcpServers: { folding: { httpUrl: url } } }),
      }],
      note: [el('code', 'httpUrl'), ' selects Streamable HTTP. Gemini CLI picks the transport from the ',
        'key name, and ', el('code', 'url'), ' would mean SSE, which this server does not serve.'],
    },
    {
      id: 'other',
      label: 'Anything else',
      steps: [
        { text: 'Cline — in cline_mcp_settings.json. Without the type it falls back to legacy SSE:',
          code: block({ mcpServers: { folding: { type: 'streamableHttp', url } } }) },
        { text: 'Goose — goose configure → Add Extension → Remote Extension (Streamable HTTP), then the URL.',
          code: null },
        { text: 'OpenAI Responses API — as a tool on the request:',
          code: block({ type: 'mcp', server_label: 'folding', server_url: url, require_approval: 'never' }) },
        { text: 'Or speak to it directly. Nothing here needs a client at all:',
          code: `curl -X POST ${url} \\\n  -H 'content-type: application/json' \\\n` +
            `  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'` },
      ],
      note: ['It is Streamable HTTP over POST, protocol ', el('code', '2025-06-18'),
        ', no session and no auth. A GET asking for an event stream is answered 405, which is what ',
        'the spec says to do when there is nothing to stream — every tool here is a pure read. Clients ',
        'that only speak the older SSE transport will not connect.'],
    },
  ];
}

/* ------------------------------------------------------------------ fold --- */

/**
 * The reader's own machine, live.
 *
 * Laid out by what somebody opens it to find out, which is not the order the client
 * happens to report things in. Almost every visit is one of two questions — "is it
 * still folding?" and "how far along is this one?" — so those are the first two things
 * on the page and the second is the largest thing on it. The machine's specification
 * is read once, ever, so it is a single quiet line at the bottom rather than a card
 * above the work.
 *
 * The account figures are deliberately in their own block, well away from the live
 * ones. Putting "1.35M/day right now" beside "61.38M/day average" in one stat row
 * invites a comparison that is simply false: the first is one machine at this instant,
 * often mid-ramp on a fresh unit, and the second is every machine you own averaged
 * over a week. They are different scopes on different clocks and they must not look
 * like peers.
 *
 * It also updates in place. The client sends about one patch every four seconds, and
 * rebuilding the document each time destroyed any text selection, dropped focus from
 * whatever button was under the cursor, and made the progress bar's transition
 * unreachable — a new element has no previous width to animate from. The layout is
 * rebuilt only when its shape changes; everything else is a text assignment.
 */
export function foldPage(view) {
  clear(view);
  const fah = new LocalClient();
  let donor = null;
  let donorFor = null;
  let shape = null;
  let sync = null;

  // Rebuild only when the page's *structure* would differ. Numbers changing is not a
  // structural change, and treating it as one is what made the page flicker.
  const shapeOf = () => {
    if (fah.status !== 'connected') return fah.status;
    return ['live', fah.units.map((u) => u.id).join(','), donor ? 'donor' : ''].join('|');
  };

  const paint = () => {
    const now = shapeOf();
    if (now !== shape) {
      shape = now;
      sync = build(view, fah, donor);
    }
    if (sync) {
      try {
        sync();
      } catch (e) {
        // The client is a program we do not ship, sending a shape we inferred. A fault
        // must read as a fault rather than as a page that quietly stopped moving.
        console.error('updating the client view failed', e);
      }
    }
  };

  const maybeFetchDonor = async () => {
    const name = fah.config.user;
    if (!name || name === donorFor) return;
    donorFor = name;
    try {
      // The envelope, not the entity: reading the wrapper gives an object whose every
      // field is undefined, which renders as a confident rank of #0 rather than as an
      // error.
      donor = (await api.donor(name)).data;
    } catch (e) {
      donor = null;
    }
    shape = null; // the donor block appears or disappears, so the layout changes
    paint();
  };

  fah.onChange(() => {
    paint();
    maybeFetchDonor();
  });
  fah.connect();
  paint();

  return () => fah.close();
}

/** Builds the layout for the current shape and returns the function that updates it. */
function build(view, fah, donor) {
  clear(view);
  view.append(el('div.page-head',
    el('h1.page-title', 'My folding'),
    el('p.page-sub',
      'Your own client, live from this machine. Nothing here reaches our server — ' +
      'the page talks to the client directly.')));

  if (fah.status === 'connecting') {
    view.append(skeleton(140));
    return null;
  }
  if (fah.status !== 'connected') {
    view.append(setupCard());
    return null;
  }

  // Status and actions together, above everything. The two commonest reasons to open
  // this page are "is it working" and "make it stop", and they are now one glance apart.
  const dot = el('span.dot');
  const statusText = el('span.fold-state');
  const statusSub = el('span.muted');
  const actions = el('div.fold-actions');
  view.append(el('section.section',
    el('div.fold-bar',
      el('div.fold-status', dot, statusText, statusSub),
      actions)));

  const units = fah.units.map((u) => unitCard(u));
  for (const u of units) view.append(u.node);

  if (!units.length) {
    view.append(el('section.section', card(null,
      el('div.card-body', el('p.empty', { style: 'margin:0' },
        fah.paused
          ? 'Paused, so no work is being requested.'
          : 'No work unit yet. The client asks for one when it is ready.')))));
  }

  // Account totals, clearly a different kind of number from the ones above: every
  // machine, not this one, and an hourly snapshot rather than a live reading.
  if (donor) {
    view.append(el('section.section', cardWith('Your totals',
      el('a.section-link', { href: '/donors/' + encodeURIComponent(donor.name) },
        'Full history →'),
      el('div.card-body',
        el('div.stats',
          statTile('Rank', '#' + n(donor.rank), movementText(donor.rank_change_24h)),
          statTile('Points', short(donor.points_total), n(donor.points_total)),
          statTile('Per day', short(donor.points_per_day_7d_avg),
            '7-day average, all machines'),
          statTile('Work units', short(donor.wus_total), 'returned in total'))))));
  }

  // The specification, read once and then never again.
  const spec = el('p.fold-spec');
  view.append(el('section.section', spec));

  return () => {
    const paused = fah.paused;
    const finishing = fah.finishing;
    const working = fah.units.length > 0;

    let state, sub, tone;
    if (paused) [state, sub, tone] = ['Paused', 'not requesting work', 'warn'];
    else if (finishing) [state, sub, tone] = ['Finishing', 'stopping after this unit', 'warn'];
    else if (working) [state, sub, tone] = ['Folding', plural(fah.units.length, 'work unit'), 'good'];
    else [state, sub, tone] = ['Waiting', 'asking for a work unit', 'idle'];

    statusText.textContent = state;
    statusSub.textContent = sub ? '· ' + sub : '';
    dot.className = 'dot ' + tone;

    // Actions follow the state rather than sitting as three equals. Only a state that
    // wants fixing gets a primary button; when it is folding, nothing needs doing and
    // nothing should look like it does.
    clear(actions);
    if (paused) {
      actions.append(btn('Resume folding', () => fah.setState('fold'), true));
    } else if (finishing) {
      actions.append(
        btn('Keep folding', () => fah.setState('fold'), true),
        btn('Pause now', () => fah.setState('pause')));
    } else {
      actions.append(
        btn('Finish after this', () => fah.setState('finish')),
        btn('Pause', () => fah.setState('pause')));
    }

    for (const u of units) u.sync(fah);

    const i = fah.info;
    spec.textContent = [
      i.mach_name || i.hostname,
      ...fah.gpus.map((g) => g.description),
      plural(i.cpus || 0, 'CPU'),
      'client ' + (i.version || '?'),
      fah.config.user ? 'as ' + fah.config.user : null,
      fah.config.team != null ? 'team ' + fah.config.team : null,
    ].filter(Boolean).join(' · ');
  };
}

function btn(label, onClick, primary = false) {
  return el('button.btn' + (primary ? '.btn-primary' : ''), { onClick, type: 'button' }, label);
}

function movementText(change) {
  if (!change) return 'unchanged in 24h';
  return (change > 0 ? '▲ ' : '▼ ') + Math.abs(change) + ' in 24h';
}

/**
 * One work unit, and the largest thing on the page.
 *
 * Percentage leads because it is the answer to the question that brought most people
 * here. The bar is under it rather than beside it so both get full width — a folding
 * box is watched from across a room, and a bar is readable at a distance that a number
 * is not.
 */
function unitCard(u) {
  const a = u.assignment || {};
  const pct = el('div.hero-num');
  const rate = el('div.hero-side');
  const fill = el('div.bar-fill');
  const left = el('span');
  const credit = el('span.muted');
  const meta = el('p.fold-meta');

  const node = el('section.section', card('Project ' + (a.project ?? '?'),
    el('div.card-body', { 'aria-live': 'off' },
      el('div.hero-row', pct, rate),
      el('div.bar', fill),
      el('div.bar-legend', left, credit),
      meta)));

  return {
    node,
    sync(fah) {
      const cur = fah.units.find((x) => x.id === u.id) || u;
      const at = cur.assignment || {};
      const p = Math.max(0, Math.min(100, (cur.wu_progress || 0) * 100));
      pct.textContent = p.toFixed(1) + '%';
      fill.style.width = p + '%';
      rate.textContent = cur.ppd ? short(cur.ppd) + '/day' : '';
      left.textContent = cur.eta ? cur.eta + ' left' : (cur.state === 'RUN' ? 'estimating…' : '');
      credit.textContent = at.credit ? n(at.credit) + ' points on return' : '';
      meta.textContent = [
        cur.state,
        at.core ? 'core ' + at.core.type : null,
        at.deadline ? plural(Math.round(at.deadline / 86400), 'day') + ' to return' : null,
        at.ws,
      ].filter(Boolean).join(' · ');
    },
  };
}

/** The card somebody sees before they have let us in — which is everybody, once. */
function setupCard() {
  const xml =
    '<config>\n' +
    '  <allowed-origins>https://folding.exec.codes</allowed-origins>\n' +
    '</config>';

  return el('section.section', card('Connect your client',
    el('div.card-body',
      el('p',
        'No client answered on this machine. Either it is not running, or it has not ' +
        'been told to trust this page — the client ignores any origin it does not ' +
        'recognise, which is why nothing here can touch it until you say so.'),
      el('p', 'Add this to ', el('code', '/etc/fah-client/config.xml'),
        ' (', el('code', '%ProgramData%\\FAHClient\\config.xml'), ' on Windows), then ' +
        'restart the client:'),
      el('pre.code-block', el('code', xml)),
      el('p.muted',
        'This grants any page at that address the same control the official client has, ' +
        'including reading your passkey and discarding work units. It is the same ' +
        'permission foldingathome.org already holds by default. Remove the line to ' +
        'revoke it.'),
      el('p', { style: 'margin-bottom:0' },
        'Your browser may also ask whether this page may reach devices on your local ' +
        'network. It has to, because your client is one of them.'))));
}


const BOTS = [
  {
    id: 'discord',
    platform: 'Discord',
    live: true,
    invite: DISCORD_INVITE,
    inviteText: 'Add to a server or your account →',
    blurb: [
      ['Slash commands for everything on this site: look up a folder or a team, see who ' +
       'you are about to pass, put two teams head to head, or ask what it would take to ' +
       'reach a rank. ',
       ['strong', 'Tell it your donor name once with '], ['code', '/link'],
       ['strong', ' and '], ['code', '/me'], ['strong', ' answers without typing it again.']],
      ['It installs two ways. To a server, where everyone in it can use the commands — ' +
       'or to your own account, where they follow you into any server and any DM without ' +
       'that server having to add anything.'],
      ['It can also speak first. ', ['code', '/alert add'],
       ' watches a folder or a team and posts to a channel when they pass a points ' +
       'milestone, reach a rank, or stop producing — optionally pinging a role. The ' +
       'quiet one is the useful one: a rig that died is otherwise invisible until ' +
       'somebody thinks to check.'],
      ['It reads the same public API as everyone else, from inside the network, and ' +
       'caches against the snapshot rather than polling. A busy channel costs one ' +
       'request an hour, however many alerts are watching.'],
    ],
    commands: [
      ['/me', 'Your own stats, once you have linked a name'],
      ['/link', 'Remember which donor name is yours'],
      ['/unlink', 'Forget it again'],
      ['/donor', 'Look up a folder'],
      ['/team', 'Look up a team'],
      ['/top', 'Leaderboard, by any column'],
      ['/rivals', 'Who you are about to pass, and who is about to pass you'],
      ['/compare', 'Two teams or two folders, head to head'],
      ['/movers', 'Biggest 24-hour rank movements'],
      ['/goal', 'What it would take to reach a rank'],
      ['/status', 'Project totals, and how fresh the data is'],
      ['/alert', 'Post to a channel when a folder or team hits a milestone, reaches a rank, or goes quiet'],
    ],
  },
];

/** Turns a blurb fragment into a node: a bare string, or [tag, text]. */
function frag(f) {
  return Array.isArray(f) ? el(f[0], { text: f[1] }) : f;
}

function botCard(b) {
  const badge = b.live
    ? el('span.badge', { text: 'Live' })
    : el('span.badge.warn', { text: 'In progress' });

  const body = [
    el('div.card-body',
      ...b.blurb.map((p) => el('p', ...(Array.isArray(p) ? p.map(frag) : [p]))),
      b.invite
        ? el('p', el('a', { href: b.invite, target: '_blank', rel: 'noopener noreferrer' },
            b.inviteText))
        : null),
  ];

  if (b.commands) {
    body.push(el('div.table-wrap', el('table.data',
      el('thead', el('tr', el('th.left', 'Command'), el('th.left.wrap', 'What it does'))),
      el('tbody', ...b.commands.map(([name, what]) =>
        el('tr', el('td.left', el('code', name)), el('td.left.wrap.muted', what)))))));
  }

  return el('section.section', cardWith(b.platform, badge, ...body));
}

/**
 * Every bot, on one page.
 *
 * It exists because the alternative was a nav entry per platform, and because the
 * command list is the part people actually want and there was nowhere to put eleven
 * rows of it — the API page could only afford a sentence.
 */
export async function botsPage(view) {
  clear(view);

  view.append(el('div.page-head',
    el('h1.page-title', 'Bots'),
    el('p.page-sub',
      'The same statistics, where the conversation already is. Free, no key, and ' +
      'nothing to sign up for.')));

  for (const b of BOTS) view.append(botCard(b));

  view.append(el('section.section', card('Building your own',
    el('div.card-body',
      el('p',
        'Nothing here is privileged. Every figure these commands print comes from the ' +
        'same unauthenticated endpoints anyone can call — ',
        link('/api', 'the REST API'), ' if you are writing a program, or ',
        link('/agents', 'the MCP server'), ' if you are pointing a model at it.'),
      el('p',
        'The one thing worth copying is the caching. Upstream publishes about once an ' +
        'hour, so a response is immutable until the next snapshot: cache against the ' +
        'snapshot time rather than a duration and a chatty client becomes a quiet one ' +
        'without losing a second of freshness.')))));
}

export async function agentsPage(view) {
  clear(view);
  const origin = location.origin;

  view.append(el('div.page-head',
    el('h1.page-title', 'For AI agents'),
    el('p.page-sub',
      // Counted, not spelled out. It said "seven" while eleven were being served.
      `A Model Context Protocol server, live and unauthenticated. Point a client at it ` +
      `and it gets ${MCP_TOOLS.length} tools over this data.`)));

  view.append(el('section.section', el('div.stats',
    statTile('Endpoint', '/mcp', 'JSON-RPC 2.0 over POST'),
    statTile('Auth', 'None', 'no key, no session'),
    statTile('Tools', String(MCP_TOOLS.length), 'read-only'),
    statTile('Transport', 'HTTP', 'stateless, CORS open')
  )));

  // Tabs above the instructions rather than in the card header: seven of them are
  // wider than a card title's leftovers, and on a docs page the control belongs with
  // the thing it changes.
  const clients = mcpClients(origin);
  let client = clients[0].id;
  const tabHost = el('div.tab-strip');
  const steps = el('div');

  function drawConnect() {
    const strip = segmented(
      clients.map((c) => ({ value: c.id, label: c.label })),
      client,
      (v) => { client = v; drawConnect(); }
    );
    strip.classList.add('seg-tabs');
    clear(tabHost).append(strip);

    const c = clients.find((x) => x.id === client);
    clear(steps);
    c.steps.forEach((s, i) => {
      steps.append(el('p', { style: i === 0 ? 'margin-top:var(--s3)' : undefined }, s.text));
      if (s.code) steps.append(el('pre.code-block', el('code', s.code)));
    });
    steps.append(el('p.muted', ...c.note));
  }
  drawConnect();

  view.append(el('section.section', card('Connect',
    el('div.card-body', tabHost, steps))));

  const rows = el('tbody');
  for (const [name, args, what] of MCP_TOOLS) {
    rows.append(el('tr',
      el('td.left', el('code', name)),
      el('td.left.muted', el('code', args)),
      el('td.left.wrap', what)));
  }
  view.append(el('section.section', card('The tools',
    el('div.table-wrap', el('table.data',
      el('thead', el('tr', el('th.left', 'Tool'), el('th.left', 'Arguments'), el('th.left.wrap', 'Answers'))),
      rows)))));

  view.append(el('section.section', card('Why these and not the REST routes',
    el('div.card-body',
      el('p',
        'The obvious design is one tool per endpoint. It is also the wrong one. Asking ' +
        '"is my team catching up to theirs?" that way costs five round trips — find an ' +
        'id, page a leaderboard, fetch two entities, pull two histories, do the ' +
        'arithmetic — with five chances to get the join wrong.'),
      el('p',
        el('code', 'compare'), ' answers it in one call, and states the assumption in ' +
        'the same breath as the number: the projection holds both sides at their ' +
        'current seven-day average forever, which nobody does. A caveat that travels ' +
        'separately from the figure it qualifies is a caveat nobody repeats.'),
      el('p',
        'Every answer also carries the age of the data it came from. A model quoting a ' +
        'figure without knowing how old it is is the failure an endpoint like this ' +
        'invites, and nothing downstream can catch it.')))));

  view.append(el('section.section', card('A worked call',
    el('div.card-body',
      el('pre.code-block', el('code',
        `curl -X POST ${origin}/mcp \\\n` +
        `  -H 'content-type: application/json' \\\n` +
        `  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call",\n` +
        `       "params":{"name":"compare","arguments":\n` +
        `         {"kind":"teams","a":"51","b":"32"}}}'`)),
      el('p',
        'Tool failures come back as results with ', el('code', 'isError'),
        ' rather than as protocol errors, so a wrong name tells you how to find the ' +
        'right one instead of looking like the server is down.')))));

  view.append(el('section.section', card('Crawling instead',
    el('div.card-body',
      el('p',
        'Automated clients are welcome here generally — see ',
        el('a', { href: '/robots.txt' }, 'robots.txt'),
        ', which allows everything and names the AI agents explicitly. There are no ' +
        'challenge pages and no rate limit.'),
      el('p',
        'Before scraping these pages, though: the ', link('/api', 'JSON API'),
        ' has everything the HTML shows, costs us both less, and will not change shape ' +
        'under you. Cache against ', el('code', 'next_expected_at'),
        ' rather than polling — the data changes once an hour and not otherwise.'),
      el('p',
        'And if you want to mirror the whole corpus, ',
        el('a', { href: '/api' }, el('code', '/v1/changes')),
        ' exists for exactly that. Roughly 550 teams and 1,800 donors move in a given ' +
        'hour, out of 129,958 and 2.1 million — so asking what changed costs well under ' +
        'one percent of crawling everything, and it is the same rows.')))));
}
