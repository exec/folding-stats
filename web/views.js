// Views.
//
// Each export renders one route into the main element and returns an optional
// cleanup function, which the router calls before rendering the next route so
// charts release their observers.

import { api, snapshot } from '/api.js';
import { el, clear, card, cardWith, statTile, pager, segmented, notice, loading, errorView, link } from '/ui.js';
import { n, short, ago, dateTime, delta, tierMark, nameText, plural, span, tzName } from '/format.js';
import { productionChart, stackedChart, stack, legend, densify, MAX_STACK_SERIES } from '/charts.js';

const PER_PAGE = 100;

/** Newest snapshot time in ms — the point past which no bucket can exist yet. */
/** How much history the "active" counts actually cover. */
function activeWindow() {
  const s = snapshot();
  return !s || s.avg_window_complete ? '7 days' : span(s.history_span_sec);
}

const complete = () => {
  const s = snapshot();
  return !s || s.avg_window_complete;
};

function snapshotMs() {
  const s = snapshot();
  return s ? Date.parse(s.at) : Date.now();
}

/** Empty chart state. Says what window was searched, so "nothing" is informative. */
function emptyChart(granularity) {
  const window_ = {
    hourly: 'the last 30 days',
    daily: 'the last 5 years',
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
        el('p.rail-note', 'Every figure here is available as JSON. No key, no account, no rate limit.'),
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
    statTile('Work units', short(d.wus_total), n(d.wus_total))
  );
}

/**
 * What the x-axis of a chart at this granularity actually means.
 *
 * Hourly points are instants and render in the reader's own timezone; days and months
 * are UTC periods the server aggregates once for everyone. Saying which is which is
 * the difference between a reader trusting the chart and thinking it disagrees with
 * the clock in the header.
 */
function chartNote(granularity) {
  if (granularity === 'monthly') return `Months are UTC. Gaps are months with no production.`;
  if (granularity === 'daily') return `Days are UTC. Gaps are days with no production.`;
  return `Times are ${tzName()}. Gaps are hours with no production.`;
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

  const controls = el('div.chart-toolbar');
  const noteEl = el('div.chart-note', chartNote(granularity));
  const body = el('div.card-body', { style: 'padding:0' }, legendEl, plotEl, noteEl);
  const node = cardWith(title, controls, body);

  async function load() {
    noteEl.textContent = chartNote(granularity);
    try {
      const res = await fetcher({ granularity, metric: 'points' });
      const pts = res.data.points || [];
      if (!pts.length) {
        chart.render(null);
        clear(legendEl).append(emptyChart(granularity));
        return;
      }
      clear(legendEl);
      const dense = densify(pts, granularity, { until: snapshotMs() });
      const xs = dense.map((p) => Math.floor(Date.parse(p.at) / 1000));
      const ys = dense.map((p) => p.points);
      chart.render([xs, ys], { granularity });
    } catch (err) {
      chart.render(null);
      clear(legendEl).append(el('div.error', { style: 'padding:40px 0' }, err.message));
    }
  }

  controls.append(
    segmented(
      [
        { value: 'hourly', label: 'Hourly', title: 'One point per upstream publish' },
        { value: 'daily', label: 'Daily' },
        { value: 'monthly', label: 'Monthly' },
      ],
      granularity,
      (v) => {
        granularity = v;
        clear(controls);
        controls.append(rebuildControls());
        load();
      }
    )
  );
  function rebuildControls() {
    return segmented(
      [
        { value: 'hourly', label: 'Hourly', title: 'One point per upstream publish' },
        { value: 'daily', label: 'Daily' },
        { value: 'monthly', label: 'Monthly' },
      ],
      granularity,
      (v) => {
        granularity = v;
        clear(controls);
        controls.append(rebuildControls());
        load();
      }
    );
  }

  load();
  return { node, destroy: () => chart.destroy() };
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
          statTile('Work units', short(d.wus_total), n(d.wus_total))
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

function teamTable(teams, { compact = false } = {}) {
  const head = el(
    'tr',
    el('th.left', 'Rank'),
    el('th.left', 'Team'),
    compact ? null : el('th', 'Members'),
    el('th', 'Per day'),
    compact ? null : el('th', 'Last 24h'),
    el('th', 'Points')
  );
  const body = el('tbody');
  for (const t of teams) {
    const idle = (t.points_per_day_7d_avg ?? 0) === 0;
    const nameLink = el('a', { href: `/teams/${t.team_id}` });
    nameText(nameLink, t.name);
    body.append(
      el(
        'tr',
        { class: idle ? 'dim' : null },
        el('td.rank', n(t.rank)),
        el('td.left.name-cell', tierMark(t.points_per_day_7d_avg), nameLink),
        compact ? null : el('td.num', { title: `${n(t.members_active)} active of ${n(t.members_total)}` },
          `${short(t.members_active)} / ${short(t.members_total)}`),
        el('td.num', { title: n(t.points_per_day_7d_avg) }, short(t.points_per_day_7d_avg)),
        compact ? null : el('td.num', { title: n(t.points_last_24h) }, short(t.points_last_24h)),
        el('td.num', { title: n(t.points_total) }, short(t.points_total))
      )
    );
  }
  return el('div.table-wrap', el('table.data', el('thead', head), body));
}

export async function teamsList(view, { page = 1 }, nav) {
  loading(view);
  try {
    const res = await api.teams({ page, per_page: PER_PAGE });
    clear(view);
    view.append(
      el('div.page-head',
        el('h1.page-title', 'Teams'),
        el('p.page-sub', `${n(res.page.total_items)} teams, ranked by lifetime points.`))
    );
    view.append(
      card(null,
        teamTable(res.data),
        pager(res.page.page, res.page.total_pages, res.page.total_items,
          (p) => nav(`/teams?page=${p}`)))
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

    view.append(el('section.section', productionStats(t, [
      statTile('Rank', `#${n(t.rank)}`, 'by lifetime points'),
    ])));

    const hist = historyCard('Production', (p) => api.teamHistory(t.team_id, p));
    cleanups.push(hist.destroy);
    view.append(el('section.section', hist.node));

    view.append(el('section.section', await teamMembersCard(t.team_id, nav)));
  } catch (err) {
    errorView(view, err);
  }
  return () => cleanups.forEach((f) => f());
}

async function teamMembersCard(teamID, nav) {
  let page = 1;
  let activeOnly = false;
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
      });
      clear(body);
      if (!res.data.length) {
        body.append(el('div.empty', activeOnly ? `No members produced in the last ${activeWindow()}.` : 'No members.'));
        return;
      }
      body.append(memberTable(res.data));
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

function memberTable(members) {
  const body = el('tbody');
  for (const m of members) {
    const idle = (m.points_per_day_7d_avg ?? 0) === 0;
    const nameLink = el('a', { href: `/donors/${encodeURIComponent(m.name)}` });
    nameText(nameLink, m.name);
    body.append(
      el(
        'tr',
        { class: idle ? 'dim' : null },
        el('td.rank', n(m.rank_in_team)),
        el('td.left.name-cell', tierMark(m.points_per_day_7d_avg), nameLink),
        el('td.num', { title: n(m.points_per_day_7d_avg) }, short(m.points_per_day_7d_avg)),
        el('td.num', { title: n(m.points_last_24h) }, short(m.points_last_24h)),
        el('td.num', { title: n(m.points_total) }, short(m.points_total))
      )
    );
  }
  return el(
    'div.table-wrap',
    el(
      'table.data',
      el('thead', el('tr',
        el('th.left', 'In team'), el('th.left', 'Donor'),
        el('th', 'Per day'), el('th', 'Last 24h'), el('th', 'Points'))),
      body
    )
  );
}

/* --------------------------------------------------------------- donors --- */

function donorTable(donors, { compact = false } = {}) {
  const body = el('tbody');
  for (const d of donors) {
    const idle = (d.points_per_day_7d_avg ?? 0) === 0;
    const nameLink = el('a', { href: `/donors/${encodeURIComponent(d.name)}` });
    nameText(nameLink, d.name);
    const cell = el('td.left.name-cell', tierMark(d.points_per_day_7d_avg), nameLink);
    // Shared placeholder names carry no badge in the table: the team count beside
    // them already tells the story, and a warning on every top row is noise. The
    // full explanation lives on the donor's own page, where it appears once.
    if (d.likely_not_a_person) {
      cell.title = `Appears on ${n(d.team_count)} teams — almost certainly a shared default name.`;
    }
    body.append(
      el(
        'tr',
        { class: idle ? 'dim' : null },
        el('td.rank', n(d.rank)),
        cell,
        compact ? null : el('td.num', n(d.team_count)),
        el('td.num', { title: n(d.points_per_day_7d_avg) }, short(d.points_per_day_7d_avg)),
        compact ? null : el('td.num', { title: n(d.points_last_24h) }, short(d.points_last_24h)),
        el('td.num', { title: n(d.points_total) }, short(d.points_total))
      )
    );
  }
  return el(
    'div.table-wrap',
    el(
      'table.data',
      el('thead', el('tr',
        el('th.left', 'Rank'), el('th.left', 'Donor'),
        compact ? null : el('th', 'Teams'),
        el('th', 'Per day'),
        compact ? null : el('th', 'Last 24h'),
        el('th', 'Points'))),
      body
    )
  );
}

export async function donorsList(view, { page = 1 }, nav) {
  loading(view);
  try {
    const res = await api.donors({ page, per_page: PER_PAGE });
    clear(view);
    view.append(
      el('div.page-head',
        el('h1.page-title', 'Donors'),
        el('p.page-sub',
          `${n(res.page.total_items)} donors, ranked by lifetime points across every team they fold for.`))
    );
    view.append(
      card(null,
        donorTable(res.data),
        pager(res.page.page, res.page.total_pages, res.page.total_items,
          (p) => nav(`/donors?page=${p}`)))
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

    view.append(el('section.section', productionStats(d, [
      statTile('Rank', `#${n(d.rank)}`, 'across all donors'),
    ])));

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
function breakdownCard(donor, teams) {
  const plotEl = el('div.chart');
  const legendEl = el('div.legend');
  const tabs = el('div.tabs', { role: 'tablist' });
  const controls = el('div.chart-toolbar');

  let granularity = 'hourly';
  let selected = 'all';
  let chart = null;

  const noteEl = el('div.chart-note', chartNote(granularity));
  const body = el('div.card-body', { style: 'padding:0' }, tabs, legendEl, plotEl, noteEl);
  const node = cardWith('Production by team', controls, body);

  // Series are chosen by recent production, not lifetime points. A donor's biggest
  // teams by lifetime total are frequently dormant, so ranking this card the same
  // way as the table below selects precisely the teams with nothing to plot — the
  // chart then looks broken while the donor is demonstrably producing.
  let shown = [];
  let rest = [];
  let producers = [];

  function setSeries(list) {
    producers = list.filter((t) => (t.points_last_7d ?? 0) > 0);
    const ranked = producers.length ? producers : list;
    shown = ranked.slice(0, MAX_STACK_SERIES);
    rest = ranked.slice(MAX_STACK_SERIES);
  }
  setSeries(teams);

  function renderTabs() {
    clear(tabs);
    // With a single producing team, "All teams" and that team's tab are the same
    // series; showing both is a choice between two identical views.
    if (shown.length < 2) return;
    const mk = (key, label, val) =>
      el('button.tab', {
        role: 'tab',
        'aria-selected': selected === key ? 'true' : 'false',
        onclick: () => { selected = key; load(); },
      }, label, val ? el('span.tab-val', short(val)) : null);

    tabs.append(mk('all', producers.length > 1 ? `Top ${Math.min(shown.length, MAX_STACK_SERIES)} teams` : 'All teams',
      donor.points_last_7d));
    // Tabs are the producing teams, labelled with what they produced — the figure
    // this card is actually about.
    for (const t of shown) {
      tabs.append(mk(String(t.team_id), t.team_name || `Team ${t.team_id}`, t.points_last_7d));
    }
  }

  function renderControls() {
    clear(controls).append(
      segmented(
        [
          { value: 'hourly', label: 'Hourly' },
          { value: 'daily', label: 'Daily' },
          { value: 'monthly', label: 'Monthly' },
        ],
        granularity,
        (v) => { granularity = v; load(); }
      )
    );
  }

  // The embedded breakdown is capped and ordered by lifetime points, so a donor's
  // actual producers can fall outside it entirely. Ask for them explicitly.
  async function loadProducers() {
    if (!donor.teams_truncated && producers.length) return;
    try {
      const res = await api.donorTeams(donor.name, { sort: 'production', per_page: 20 });
      if (res.data.length) setSeries(res.data);
    } catch {
      // Fall back to the embedded list; it is a worse ordering, not a broken one.
    }
  }

  async function load() {
    renderTabs();
    renderControls();
    noteEl.textContent = chartNote(granularity);
    if (chart) chart.destroy();
    clear(legendEl);

    try {
      if (selected === 'all' && !producers.length) {
        legendEl.append(el('div.chart-empty',
          el('div', 'No team has produced recently'),
          el('div', { style: 'font-size:12px;margin-top:4px' },
            `${n(donor.team_count)} teams on record, none with output in the last 7 days.`)));
        return;
      }
      // A stack of one is a line. Stacking a single series draws it as a solid
      // block, which reads as far more emphatic than the data warrants.
      if (selected === 'all' && shown.length === 1) {
        const only = shown[0];
        const res = await api.donorHistory(donor.name, { granularity, team_id: only.team_id });
        const pts = res.data.points || [];
        chart = productionChart(plotEl, only.team_name || `Team ${only.team_id}`);
        if (!pts.length) {
          legendEl.append(emptyChart(granularity));
          return;
        }
        const dense = densify(pts, granularity, { until: snapshotMs() });
        chart.render(
          [dense.map((p) => Math.floor(Date.parse(p.at) / 1000)), dense.map((p) => p.points)],
          { granularity }
        );
        return;
      }

      if (selected === 'all') {
        const fetched = await Promise.all(
          shown.map(async (t) => ({
            team: t,
            points: (await api.donorHistory(donor.name, { granularity, team_id: t.team_id }))
              .data.points || [],
          }))
        );

        // A team that produced nothing *in this window* gets no band, no legend
        // entry and no tooltip row. Selecting series by the 7-day figure is wrong
        // as soon as the chart is showing a different window — the key would name
        // colours that appear nowhere on the plot.
        const active = fetched.filter((f) => f.points.some((p) => p.points > 0));
        if (!active.length) {
          legendEl.append(emptyChart(granularity));
          return;
        }
        if (active.length === 1) {
          const only = active[0];
          chart = productionChart(plotEl, only.team.team_name || `Team ${only.team.team_id}`);
          const dense = densify(only.points, granularity, { until: snapshotMs() });
          chart.render(
            [dense.map((p) => Math.floor(Date.parse(p.at) / 1000)), dense.map((p) => p.points)],
            { granularity }
          );
          return;
        }

        const labels = active.map((f) => f.team.team_name || `Team ${f.team.team_id}`);
        const seriesData = active.map((f) => ({ data: { points: f.points } }));
        // Align every team onto the union of timestamps: a team idle in a bucket
        // contributes zero there rather than shortening the series.
        const merged = [...new Set(seriesData.flatMap((r) => (r.data.points || []).map((p) => p.at)))]
          .sort()
          .map((at) => ({ at, points: 0, wus: 0 }));
        if (!merged.length) {
          legendEl.append(emptyChart(granularity));
          return;
        }
        const times = densify(merged, granularity, { until: snapshotMs() }).map((p) => p.at);
        const idx = new Map(times.map((t, i) => [t, i]));
        const rows = seriesData.map((r) => {
          const arr = new Array(times.length).fill(0);
          for (const p of r.data.points || []) arr[idx.get(p.at)] = p.points;
          return arr;
        });
        if (rest.length) {
          // Everything past the slot count, as one band — but only if it actually
          // produced in this window.
          const others = await Promise.all(
            rest.slice(0, 8).map((t) => api.donorHistory(donor.name, { granularity, team_id: t.team_id }))
          );
          const arr = new Array(times.length).fill(0);
          for (const r of others) for (const p of r.data.points || []) {
            if (idx.has(p.at)) arr[idx.get(p.at)] += p.points;
          }
          if (arr.some((v) => v > 0)) {
            rows.push(arr);
            labels.push(`Other (${rest.length})`);
          }
        }

        chart = stackedChart(plotEl);
        legend(legendEl, labels);
        const xs = times.map((t) => Math.floor(new Date(t).getTime() / 1000));
        chart.render([xs, ...stack(rows)], { granularity, labels, stacked: true });
      } else {
        const res = await api.donorHistory(donor.name, { granularity, team_id: selected });
        const pts = res.data.points || [];
        chart = productionChart(plotEl);
        if (!pts.length) {
          legendEl.append(emptyChart(granularity));
          return;
        }
        const dense = densify(pts, granularity, { until: snapshotMs() });
        const xs = dense.map((p) => Math.floor(Date.parse(p.at) / 1000));
        chart.render([xs, dense.map((p) => p.points)], { granularity });
      }
    } catch (err) {
      legendEl.append(el('div.error', { style: 'padding:40px 0' }, err.message));
    }
  }

  (async () => {
    await loadProducers();
    load();
  })();
  return { node, destroy: () => chart && chart.destroy() };
}

function teamsCard(donor, teams) {
  const body = el('tbody');
  for (const t of teams) {
    const link = el('a', { href: `/teams/${t.team_id}` });
    nameText(link, t.team_name || `Team ${t.team_id}`);
    body.append(
      el(
        'tr',
        el('td.left.name-cell', tierMark(t.points_per_day_7d_avg), link),
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

  const endpoint = (method, path, desc) =>
    el('tr',
      el('td.left', el('code', { style: 'color:var(--series-3)' }, method)),
      el('td.left', el('a', { href: base + path.replace(/\{[^}]+\}/g, (m) => m) }, el('code', path))),
      el('td.left.muted', desc));

  view.append(
    el('div.page-head',
      el('h1.page-title', 'API'),
      el('p.page-sub',
        'Free, public, and unauthenticated. No key, no sign-up, and nothing your browser has to solve first.'))
  );

  view.append(el('section.section', el('div.stats',
    statTile('Auth', 'None', 'no key required'),
    statTile('Rate limit', 'None', 'be reasonable'),
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
        endpoint('GET', '/v1/teams', 'Team leaderboard, paginated'),
        endpoint('GET', '/v1/teams/{id}', 'One team'),
        endpoint('GET', '/v1/teams/{id}/members', 'Team roster, ?active_only=true'),
        endpoint('GET', '/v1/teams/{id}/history', '?granularity=hourly|daily|monthly'),
        endpoint('GET', '/v1/donors', 'Donor leaderboard, paginated'),
        endpoint('GET', '/v1/donors/{name}', 'Per-team breakdown, ?sort=production'),
        endpoint('GET', '/v1/donors/{name}/teams', 'Full team list, paginated'),
        endpoint('GET', '/v1/donors/{name}/history', '?team_id= to scope to one team, same granularities'),
        endpoint('GET', '/v1/search', '?q= name prefix, exact name, or team ID')
      ))))));

  view.append(el('section.section', card('Notes',
    el('div.card-body',
      el('p', { style: 'margin-top:0' },
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
        el('strong', 'Upstream does not publish on the hour. '),
        'The measured interval is about ', el('code', '3610s'), ' and drifts later every ' +
        'cycle, so a day usually has 24 hourly buckets and occasionally 23. Use ',
        el('code', 'interval_sec'), ' and ', el('code', 'next_expected_at'),
        ' rather than assuming a fixed schedule.'),
      el('p',
        el('strong', 'Clocks disagree. '), 'Every response includes ', el('code', 'server_time'),
        '. Compare timestamps against that rather than your own clock, and a countdown ' +
        'stays right on a machine whose clock is minutes off.'),
      el('p', { style: 'margin-bottom:0' },
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
