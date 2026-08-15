import { el, clear } from '/ui.js';
import { n, short } from '/format.js';

let d3Ready;

function loadD3() {
  if (globalThis.d3) return Promise.resolve(globalThis.d3);
  if (!d3Ready) {
    d3Ready = new Promise((resolve, reject) => {
      const script = el('script', { src: '/vendor/d3.v7.min.js' });
      script.onload = () => resolve(globalThis.d3);
      script.onerror = () => reject(new Error('Could not load the globe renderer'));
      document.head.append(script);
    });
  }
  return d3Ready;
}

function tooltipBody(tip, country) {
  clear(tip).append(
    el('div.globe-tip-name', country.name),
    el('div.globe-tip-stats',
      `${short(country.points_per_day_24h_avg)} PPD · ${short(country.points_total)} points`),
    el('div.globe-tip-count',
      `${n(country.teams_active)} active of ${n(country.teams_total)} counted teams`),
    el('div.globe-tip-teams', ...country.teams.slice(0, 10).map((team) =>
      el('a', { href: `/teams/${team.team_id}` },
        el('span', team.name), el('span.num', short(team.points_total))))),
    el('a.globe-tip-more', { href: `/teams/around-the-globe/${country.code.toLowerCase()}` },
      country.teams_total > 10 ? `View all ${n(country.teams_total)} teams →` : 'View country page →')
  );
}

/** Render an orthographic earth with mouse/touch rotation, wheel zoom and country details. */
export async function createGlobe(host, countries) {
  const [d3, world] = await Promise.all([
    loadD3(),
    fetch('/vendor/countries.geojson').then((r) => {
      if (!r.ok) throw new Error('Could not load country boundaries');
      return r.json();
    }),
  ]);
  const byCode = new Map(countries.map((c) => [c.code, c]));
  for (const feature of world.features) feature.country = byCode.get(feature.properties.code);

  clear(host);
  const tip = el('div.globe-tooltip', { hidden: true });
  const svg = d3.select(host).append('svg')
    .attr('class', 'globe-svg')
    .attr('role', 'group')
    .attr('aria-label', 'Interactive globe. Drag to rotate and use the mouse wheel to zoom.');
  host.append(tip);

  const projection = d3.geoOrthographic().precision(0.4).clipAngle(90).rotate([-10, -25]);
  const path = d3.geoPath(projection);
  const sphere = svg.append('path').datum({ type: 'Sphere' }).attr('class', 'globe-ocean');
  const maxPPD = Math.max(...countries.map((c) => c.points_per_day_24h_avg), 1);
  const countriesPath = svg.append('g').selectAll('path')
    .data(world.features)
    .join('path')
    .attr('class', (d) => d.country ? 'globe-country active' : 'globe-country')
    .attr('fill-opacity', (d) => {
      if (!d.country) return 1;
      return 0.42 + 0.5 * Math.log1p(d.country.points_per_day_24h_avg) / Math.log1p(maxPPD);
    })
    .attr('tabindex', (d) => d.country ? 0 : null)
    .attr('role', (d) => d.country ? 'link' : null)
    .attr('aria-label', (d) => d.country
      ? `${d.country.name}, ${n(d.country.teams_total)} counted teams, ${n(d.country.points_per_day_24h_avg)} points per day`
      : null);

  let width = 0;
  let zoom = 1;
  let hideTimer;
  let pinned = null;

  function redraw() {
    sphere.attr('d', path);
    countriesPath.attr('d', path);
  }

  function resize() {
    width = host.clientWidth || 720;
    const height = Math.max(360, Math.min(680, width * 0.72));
    svg.attr('viewBox', `0 0 ${width} ${height}`);
    projection.translate([width / 2, height / 2]).scale(Math.min(width, height) * 0.43 * zoom);
    redraw();
  }

  function placeTip(clientX, clientY) {
    const rect = host.getBoundingClientRect();
    const left = Math.max(12, Math.min(rect.width - 332, clientX - rect.left + 14));
    const top = Math.max(12, Math.min(rect.height - tip.offsetHeight - 12, clientY - rect.top + 14));
    tip.style.left = `${left}px`;
    tip.style.top = `${top}px`;
  }

  function show(event, feature, pin = false) {
    if (!feature.country) return;
    clearTimeout(hideTimer);
    if (pin) pinned = feature;
    tooltipBody(tip, feature.country);
    tip.hidden = false;
    placeTip(event.clientX, event.clientY);
  }

  function hideSoon(feature) {
    if (pinned === feature) return;
    hideTimer = setTimeout(() => { tip.hidden = true; }, 120);
  }

  countriesPath.filter((d) => !!d.country)
    .on('mouseenter', (event, d) => show(event, d))
    .on('mousemove', (event, d) => { if (!pinned) show(event, d); })
    .on('mouseleave', (_, d) => hideSoon(d))
    .on('click', (event, d) => { event.stopPropagation(); show(event, d, true); })
    .on('keydown', (event, d) => {
      if (event.key !== 'Enter' && event.key !== ' ') return;
      event.preventDefault();
      const rect = event.currentTarget.getBoundingClientRect();
      show({ clientX: rect.left + rect.width / 2, clientY: rect.top + rect.height / 2 }, d, true);
    });

  tip.addEventListener('mouseenter', () => clearTimeout(hideTimer));
  tip.addEventListener('mouseleave', () => {
    if (!pinned) tip.hidden = true;
  });
  svg.on('click', () => { pinned = null; tip.hidden = true; });

  let start;
  svg.call(d3.drag()
    .on('start', (event) => { start = { x: event.x, y: event.y, rotate: projection.rotate() }; })
    .on('drag', (event) => {
      pinned = null;
      tip.hidden = true;
      projection.rotate([
        start.rotate[0] + (event.x - start.x) * 0.28,
        Math.max(-90, Math.min(90, start.rotate[1] - (event.y - start.y) * 0.28)),
        0,
      ]);
      redraw();
    }));
  svg.call(d3.zoom()
    .filter((event) => event.type === 'wheel')
    .scaleExtent([0.75, 4])
    .on('zoom', (event) => { zoom = event.transform.k; resize(); }));

  const ro = new ResizeObserver(resize);
  ro.observe(host);
  resize();
  return () => {
    ro.disconnect();
    clearTimeout(hideTimer);
    svg.on('.drag', null).on('.zoom', null);
  };
}
