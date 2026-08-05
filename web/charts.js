// Chart layer.
//
// One chart type carries almost the whole site: production over time. Every one of
// them is stepped, because what is plotted is what a bucket produced during a period
// rather than a sample of something continuous, and a sloped line between two buckets
// asserts output in the gap between them. A donor who banked everything in two hours
// on Tuesday should not get a smooth ramp across the days they folded nothing.
//
// That also means the single-series and stacked forms read the same way: gaining a
// second team changes how many bands there are and nothing else.
//
// Because an HTML chart is interactive by nature, every chart ships a crosshair and
// tooltip rather than making people squint at gridlines.
//
// Colours come from CSS custom properties rather than literals, so the theme
// toggle repaints charts through the same mechanism as everything else, and the
// series slots stay in their fixed order.

import uPlot from '/vendor/uPlot.esm.min.js';
import { short, n } from '/format.js';

const css = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

function theme() {
  return {
    ink: css('--ink'),
    muted: css('--ink-muted'),
    grid: css('--grid'),
    axis: css('--line-strong'),
    surface: css('--surface'),
    series: [1, 2, 3, 4, 5, 6, 7, 8].map((i) => css(`--series-${i}`)),
  };
}

/** Translucent version of a hex colour, for area fills under a line. */
function fade(hex, alpha) {
  const m = /^#?([a-f\d]{2})([a-f\d]{2})([a-f\d]{2})$/i.exec(hex);
  if (!m) return hex;
  const [r, g, b] = [1, 2, 3].map((i) => parseInt(m[i], 16));
  return `rgba(${r},${g},${b},${alpha})`;
}

/**
 * Buckets are UTC, so they are rendered in UTC.
 *
 * Rendering them locally shifts every label by the viewer's offset, which is not a
 * cosmetic problem: the July monthly bucket is 2026-07-01T00:00:00Z, and west of
 * Greenwich that formats as "Jun 30" — so the tooltip says June while the axis tick
 * beside it says July.
 *
 * The resolution is not one timezone everywhere, because the two kinds of timestamp
 * are not the same kind of thing:
 *
 *   - An *instant* — a publish time, an hourly point — is a moment that happened. It
 *     belongs in the reader's own clock, which the browser already knows. "Aug 2,
 *     8:30 PM" beats "Aug 3, 01:30 UTC" for someone in Chicago, and both name the
 *     same moment, so nothing is lost.
 *   - A *calendar bucket* — a day, a month — is a named period, not a moment. The
 *     server aggregates one set of rollups on UTC boundaries for every reader; there
 *     is no per-viewer version of "July". Its label is its identity, and rendering
 *     that identity's start instant through a timezone doesn't translate it, it
 *     renames it to the wrong month.
 *
 * So instants follow the browser and buckets keep their own name, which is also what
 * the API says they are (points_today_utc and friends).
 */
const fmtDate = (ts, gran) => {
  const d = new Date(ts * 1000);
  if (gran === 'monthly') {
    return d.toLocaleDateString(undefined, { timeZone: 'UTC', month: 'short', year: 'numeric' });
  }
  if (gran === 'daily') {
    return d.toLocaleDateString(undefined, { timeZone: 'UTC', month: 'short', day: 'numeric' });
  }
  return d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric' });
};

// uPlot picks tick boundaries in local time by default, which is right for instants
// and wrong for buckets — it would put the gridlines an offset away from the periods
// they label.
const utcDate = (ts) => uPlot.tzDate(new Date(ts * 1000), 'Etc/UTC');
const localDate = (ts) => new Date(ts * 1000);
const dateFn = (gran) => (gran === 'daily' || gran === 'monthly' ? utcDate : localDate);

/**
 * Chart wraps a uPlot instance with the behaviour every chart here needs:
 * responsive width, a tooltip, and rebuild-on-theme-change.
 */
class Chart {
  constructor(el, build) {
    this.el = el;
    this.build = build;
    this.plot = null;
    this.data = null;

    this.tip = document.createElement('div');
    this.tip.className = 'u-tooltip';
    el.style.position = 'relative';
    el.appendChild(this.tip);

    this.ro = new ResizeObserver(() => this.resize());
    this.ro.observe(el);

    this.onTheme = () => this.render(this.data, this.meta);
    document.addEventListener('themechange', this.onTheme);
  }

  render(data, meta) {
    this.data = data;
    this.meta = meta;
    this.destroyPlot();
    if (!data || data[0].length === 0) {
      this.el.classList.add('is-empty');
      return;
    }
    this.el.classList.remove('is-empty');
    const opts = this.build(theme(), meta, this);
    opts.width = this.el.clientWidth || 600;
    opts.height = this.el.clientHeight || 300;
    this.plot = new uPlot(opts, data, this.el);
  }

  resize() {
    if (this.plot && this.el.clientWidth) {
      this.plot.setSize({ width: this.el.clientWidth, height: this.el.clientHeight || 300 });
    }
  }

  destroyPlot() {
    if (this.plot) {
      this.plot.destroy();
      this.plot = null;
    }
  }

  destroy() {
    this.destroyPlot();
    this.ro.disconnect();
    document.removeEventListener('themechange', this.onTheme);
  }
}

/**
 * Ticks placed on the buckets themselves, for granularities coarser than the
 * range they span.
 *
 * uPlot picks tick intervals from the time range, not from the data. Over sixteen
 * days it chooses roughly daily ticks — which a month-only label collapses into
 * eleven identical "Jul 2026"s. Placing ticks on actual buckets guarantees one
 * label per bucket and gridlines that mean something.
 */
function bucketSplits(u, axisIdx, min, max) {
  const xs = u.data[0].filter((t) => t >= min && t <= max);
  if (!xs.length) return [min];
  const width = u.bbox.width / (window.devicePixelRatio || 1);
  const maxTicks = Math.max(2, Math.floor(width / 110));
  if (xs.length <= maxTicks) return xs;
  const step = Math.ceil(xs.length / maxTicks);
  return xs.filter((_, i) => i % step === 0);
}

/** Axis and grid styling shared by every chart: present, but recessive. */
function axes(t, gran) {
  return [
    {
      stroke: t.muted,
      grid: { stroke: t.grid, width: 1 },
      ticks: { stroke: t.grid, width: 1, size: 4 },
      font: '11px system-ui, sans-serif',
      // Date labels are wide; the default spacing runs them together. Reserving
      // room per tick makes uPlot choose a coarser interval rather than overlap.
      space: 110,
      // Hourly labels carry a time, so uPlot's own boundaries read well. Coarser
      // labels need to sit on the buckets or they repeat.
      splits: gran === 'hourly' || !gran ? undefined : bucketSplits,
      values: (u, splits) => {
        // Belt and braces: never print the same label twice in a row, whatever
        // the range and granularity happen to be.
        let prev = null;
        return splits.map((s) => {
          const label = fmtDate(s, gran);
          if (label === prev) return '';
          prev = label;
          return label;
        });
      },
    },
    {
      stroke: t.muted,
      grid: { stroke: t.grid, width: 1 },
      ticks: { show: false },
      font: '11px system-ui, sans-serif',
      size: 56,
      values: (u, splits) => splits.map((v) => short(v)),
    },
  ];
}

/**
 * The bucket the pointer is inside, rather than the boundary it is nearest.
 *
 * uPlot's cursor.idx is always the closest x point, and with stepped paths a bucket
 * owns [its own timestamp, the next one) — so past the halfway mark of any plateau
 * the nearest boundary belongs to the following bucket, and the tooltip names a
 * period the pointer is not over.
 *
 * cursor.dataIdx does not fix this on its own: uPlot applies it to the per-series
 * indices it draws points and legend values from, while cursor.idx keeps the nearest
 * one. Anything reading cursor.idx — this tooltip — has to floor for itself, so both
 * callers share this rather than each carrying a copy to drift apart.
 */
function bucketIdx(u, closestIdx, xVal) {
  if (closestIdx == null || closestIdx <= 0) return closestIdx;
  return u.data[0][closestIdx] > xVal ? closestIdx - 1 : closestIdx;
}

/** Cursor configuration. */
function cursor() {
  return {
    // No hover dots. They were wrong twice over on a stepped chart.
    //
    // Position: a dot is drawn at its data point, and with align: 1 that point is the
    // left edge of the plateau, not the period the plateau represents — so it read as
    // though the value belonged to the boundary.
    //
    // Meaning: on a stacked chart the y it marks is the *cumulative* total at that
    // band, so the dot sat at the top of each band while the tooltip beside it
    // reported that band's own contribution. Two marks disagreeing about what they
    // measure is worse than one mark fewer.
    //
    // What is left says everything the dots did: the crosshair gives the position,
    // and the tooltip names the bucket and lists each band's own figure against its
    // colour.
    points: { show: false },
    // Snap to the bucket the pointer is inside, not the nearest boundary. A bucket
    // owns [its own timestamp, the next one), so nearest-point snapping hands the
    // right half of every plateau to the following bucket — naming one period while
    // the pointer is over another.
    dataIdx: (u, seriesIdx, closestIdx, xVal) => bucketIdx(u, closestIdx, xVal),
    // A vertical crosshair only: a horizontal one implies reading a value off the
    // y-axis, which the tooltip already does more precisely.
    y: false,
    focus: { prox: 24 },
  };
}

/**
 * The tooltip hook.
 *
 * uPlot takes `hooks` at the top level of its options, not inside `cursor` —
 * nesting it there is silently ignored, which leaves the crosshair working and no
 * tooltip at all.
 */
function tooltipHooks(chart, label) {
  return {
    setCursor: [
      (u) => {
        const { left, top } = u.cursor;
        if (u.cursor.idx == null || left == null || left < 0) {
          chart.tip.classList.remove('on');
          return;
        }
        // cursor.idx is the nearest boundary; the reader is pointing at a period.
        const idx = bucketIdx(u, u.cursor.idx, u.posToVal(left, 'x'));
        const ts = u.data[0][idx];
        const rows = [];
        for (let s = 1; s < u.series.length; s++) {
          const v = u.data[s][idx];
          if (v == null) continue;
          // Stacked series carry cumulative values; show the band's own
          // contribution, which is what the colour in front of the reader means.
          const below = s + 1 < u.series.length ? u.data[s + 1][idx] ?? 0 : 0;
          const shown = chart.meta?.stacked ? v - below : v;
          // A team that contributed nothing in this bucket earns no row. Listing
          // every series with a zero beside it buries the one that matters.
          if (chart.meta?.stacked && shown === 0) continue;
          rows.push({ label: u.series[s].label || label, value: shown, color: u.series[s]._swatch });
        }
        chart.tip.innerHTML =
          `<div class="t-date">${escapeHtml(fmtDate(ts, chart.meta?.granularity))}</div>` +
          (rows.length
            ? rows
                .map(
                  (r) =>
                    `<div class="t-row"><span class="swatch" style="background:${escapeHtml(r.color)}"></span>` +
                    `${escapeHtml(r.label)}<span class="t-val">${n(r.value)}</span></div>`
                )
                .join('')
            : '<div class="t-row t-none">No production</div>');
        chart.tip.style.left = `${left}px`;
        chart.tip.style.top = `${top}px`;
        chart.tip.classList.add('on');
      },
    ],
  };
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

/**
 * Single-series production over time.
 *
 * No legend: the card title names the series, and one series plus a legend box is
 * redundant chrome.
 */
/**
 * What the tooltip calls the series once it is showing a rate.
 *
 * A single-series chart is labelled "Points", which becomes plain PPD — the term
 * Folding@home uses, so it needs no gloss. A per-team series is labelled with the
 * team's name, which has to keep it and take the unit as a suffix.
 */
const rateLabel = (label, perDay) =>
  !perDay ? label : label === 'Points' ? 'PPD' : `${label} PPD`;

export function productionChart(el, label = 'Points') {
  const chart = new Chart(el, (t, meta, self) => ({
    // A rate and a total are different quantities, so the tooltip has to say which
    // one it is quoting — the bars look identical either way.
    padding: [12, 24, 0, 0],
    tzDate: dateFn(meta?.granularity),
    axes: axes(t, meta?.granularity),
    cursor: cursor(),
    hooks: tooltipHooks(self, rateLabel(label, meta?.perDay)),
    legend: { show: false },
    scales: { y: { range: (u, min, max) => [0, max === 0 ? 1 : max * 1.08] } },
    series: [
      { label: 'Time' },
      {
        label: rateLabel(label, meta?.perDay),
        stroke: t.series[0],
        _swatch: t.series[0],
        width: 2,
        fill: fade(t.series[0], 0.14),
        // Stepped, like the stacked view, so the whole site draws production the
        // same way and gaining a second team changes how many bands there are and
        // nothing else. A bucket is what was produced during a period, not a sample
        // of something continuous: a sloped line between two buckets claims output in
        // the gap, which for an individual is routinely a lie — somebody who banked
        // everything in two hours on Tuesday got a smooth ramp across days they
        // folded nothing at all.
        //
        // align: 1 holds the value forward from its timestamp, which is what a bucket
        // labelled by its start means.
        paths: uPlot.paths.stepped({ align: 1 }),
        // No static markers. On a line they showed which points were real among the
        // interpolation; on a step there is no interpolation — every plateau is an
        // observation. They also sat at the left edge of their own step rather than on
        // it, reading as though the value belonged to the boundary.
        points: { show: false },
      },
    ],
  }));
  return chart;
}

/**
 * Per-team contribution over time, stacked.
 *
 * Stacking answers both questions a multi-team donor has at once — total output,
 * and where it went — from one figure. Beyond the slot count the tail folds into
 * "Other" rather than generating a ninth hue.
 */
/**
 * Per-team contribution over time, stacked.
 *
 * Stepped rather than sloped because each bucket is a discrete quantity, not a
 * sample of something continuous. Production is also spiky and sparse: a donor
 * typically banks points in a handful of hours across a week, and joining those
 * isolated spikes with sloped lines implies steady output between them.
 *
 * The single-series chart is stepped for the same reason, so gaining a second team
 * changes how many bands there are and nothing else about how to read the figure.
 */
export function stackedChart(el) {
  const chart = new Chart(el, (t, meta, self) => {
    const labels = meta?.labels || [];
    // Stepped areas, not bars.
    //
    // uPlot's bar renderer distributes multiple bar series side by side within a
    // bucket rather than overlapping them, so the cumulative-overpaint trick that
    // produces a stack does not apply — the largest contributor ends up drawn in
    // the smallest one's colour. Stepped areas fill down to the baseline, so the
    // overpaint works, while the flat-then-vertical shape still reads as a discrete
    // per-bucket quantity rather than a continuous ramp between spikes.
    const stepped = uPlot.paths.stepped({ align: 1 });
    return {
      padding: [12, 24, 0, 0],
      tzDate: dateFn(meta?.granularity),
      axes: axes(t, meta?.granularity),
      cursor: cursor(),
      hooks: tooltipHooks(self),
      legend: { show: false },
      scales: { x: { time: true }, y: { range: (u, min, max) => [0, max === 0 ? 1 : max * 1.08] } },
      series: [
        { label: 'Time' },
        ...labels.map((label, i) => ({
          label,
          // Values arrive already accumulated from the top, so drawing in order
          // leaves each segment visible between its neighbour and itself.
          paths: stepped,
          // A hairline of surface between segments keeps adjacent bands legible
          // where they meet.
          stroke: t.surface,
          width: 1,
          fill: t.series[i % t.series.length],
          _swatch: t.series[i % t.series.length],
          points: { show: false },
        })),
      ],
    };
  });
  return chart;
}

export const MAX_STACK_SERIES = 6;

/**
 * Convert per-series values into the cumulative form a stacked chart needs.
 *
 * Series are ordered largest-first by the caller. The returned rows are cumulative
 * sums taken from the *last* series backwards, so uPlot draws the full total first
 * and each smaller sum paints over it — leaving every band visible in its own
 * colour without any custom renderer.
 */
export function stack(seriesValues) {
  const len = seriesValues[0]?.length ?? 0;
  const out = seriesValues.map(() => new Array(len).fill(0));
  for (let i = 0; i < len; i++) {
    let running = 0;
    for (let s = seriesValues.length - 1; s >= 0; s--) {
      running += seriesValues[s][i] ?? 0;
      out[s][i] = running;
    }
  }
  return out;
}

/** Legend markup for a stacked chart. Always present when there are ≥2 series. */
export function legend(container, labels) {
  container.innerHTML = '';
  const t = theme();
  labels.forEach((label, i) => {
    const item = document.createElement('div');
    item.className = 'legend-item';
    const sw = document.createElement('span');
    sw.className = 'swatch';
    sw.style.background = t.series[i % t.series.length];
    item.append(sw, document.createTextNode(label));
    container.appendChild(item);
  });
}


/**
 * Fill gaps in a sparse series with explicit zeros.
 *
 * Only buckets with production are stored, so an idle entity's series arrives full
 * of holes. Plotted as-is, two points 18 hours apart get joined by a straight line
 * that implies steady output across the gap, and the cursor snaps to whichever real
 * point is nearest — which is how hovering over "Jul 28, 12:00" ends up reporting
 * "Jul 27, 5 PM".
 *
 * A missing bucket means the entity produced nothing, so zero is the true value.
 * Filling only between the first and last observation avoids claiming coverage for
 * periods before collection began.
 */
/**
 * Bucket width in ms, for every function that has to walk from one bucket to the
 * next. Monthly is not a fixed width, so it returns 0 and callers step by date.
 *
 * One definition on purpose. This was inlined separately in densify and in padLone,
 * and adding weekly to the first and missing the second gave a lone weekly bucket
 * zero-padding an hour either side — three points an hour apart on a chart whose
 * footnote said the buckets were weeks. Anything gappy enough to need densifying is
 * gappy enough that the padded path is the one real data hits first.
 */
function bucketStep(granularity) {
  switch (granularity) {
    case 'monthly': return 0; // variable; stepped by date instead
    case 'weekly': return 7 * 86400e3;
    case 'daily': return 86400e3;
    default: return 3600e3;
  }
}

/**
 * How many days this bucket covers, for rendering production as a rate.
 *
 * The newest bucket is nearly always in progress — today, this week, this month — so
 * dividing it by its nominal length reports a collapse that has not happened. Divide
 * by however much of it has actually elapsed instead; the same correction
 * metrics.PerDay makes on the server, where assuming a full window once made the
 * project's per-day figure read 8.8x low.
 *
 * Hourly points are the delta between two publishes, which drifts a few seconds each
 * cycle and is complete the moment it exists. They take the nominal hour — the step
 * densify already walks — rather than a per-point measurement the rest of the chart
 * does not use.
 */
function bucketDays(at, granularity, until, since) {
  const t = Date.parse(at);
  let nominal;
  if (granularity === 'monthly') {
    const d = new Date(t);
    nominal = (Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + 1, 1)
      - Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), 1)) / 86400e3;
  } else {
    nominal = bucketStep(granularity) / 86400e3;
  }
  if (granularity !== 'daily' && granularity !== 'weekly' && granularity !== 'monthly') return nominal;
  if (until == null) return nominal;
  // Only count time we were actually watching. A bucket can be older than the
  // service: with two days of history the week that began on Sunday is three days
  // long and one day observed, and dividing by three reports a team producing 2.8B
  // a day as if it were doing 1.9B. The two coarse views would disagree with the two
  // fine ones for a month, which is exactly when someone is looking.
  const from = since == null ? t : Math.max(t, since);
  // One hour is the floor. Extrapolating a whole day from the first minutes after
  // 00:00 UTC divides by something near zero and throws the bar off the top of the
  // chart; an hour in, the estimate is merely noisy, which is what a rate this fresh
  // honestly is.
  return Math.max(Math.min(nominal, (until - from) / 86400e3), 1 / 24);
}

/** Production per bucket, restated as production per day. */
export function perDayPoints(points, granularity, until, since) {
  return points.map((p) => {
    const d = bucketDays(p.at, granularity, until, since);
    return { ...p, points: Math.round(p.points / d), wus: Math.round(p.wus / d) };
  });
}

export function densify(points, granularity, { limit = 5000, until, since, perDay = false } = {}) {
  if (perDay) points = perDayPoints(points, granularity, until, since);
  if (points.length < 2) return padLone(points, granularity, limit, until);

  const step = bucketStep(granularity);
  const next = (t) => {
    if (granularity === 'monthly') {
      const d = new Date(t);
      return Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + 1, 1);
    }
    return t + step;
  };
  // Half a bucket of slack. Upstream publishes drift by a few seconds each hour —
  // 23:30:17, then 00:30:30, then 01:30:40 — so an exact-interval walk misses every
  // real bucket after the first and replaces it with a zero. Anchoring on the
  // observations themselves and only filling genuine gaps keeps the real
  // timestamps intact.
  const slack = (granularity === 'monthly' ? 28 * 86400e3 : step) / 2;

  const out = [points[0]];
  for (let i = 1; i < points.length && out.length < limit; i++) {
    const cur = Date.parse(points[i].at);
    let t = next(Date.parse(out[out.length - 1].at));
    while (t < cur - slack && out.length < limit) {
      out.push(zeroAt(t));
      t = next(t);
    }
    out.push(points[i]);
  }
  return out;
}

const zeroAt = (t) => ({ at: new Date(t).toISOString(), points: 0, wus: 0 });

/**
 * A single observation has no width — an area of one point draws nothing at all.
 * It gets one zero bucket either side so the spike has something to stand on.
 *
 * Never forward past the newest snapshot: buckets after it have not happened, and
 * drawing them asserts a future we cannot know.
 */
function padLone(points, granularity, limit, until) {
  if (points.length !== 1) return points;
  const step = bucketStep(granularity);
  const t = Date.parse(points[0].at);
  const shift = (dir) => {
    if (granularity === 'monthly') {
      const d = new Date(t);
      return Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + dir, 1);
    }
    return t + dir * step;
  };
  const out = [zeroAt(shift(-1)), points[0]];
  const after = shift(1);
  if (until == null || after <= until) out.push(zeroAt(after));
  return out;
}

