// Small DOM helpers and shared components.

/** el('div.card', {title: 'x'}, child, child) */
export function el(spec, attrs, ...children) {
  const [tag, ...classes] = spec.split('.');
  const node = document.createElement(tag || 'div');
  if (classes.length) node.className = classes.join(' ');
  if (attrs && (attrs.nodeType || typeof attrs === 'string')) {
    children.unshift(attrs);
  } else if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v === undefined || v === null || v === false) continue;
      if (k === 'text') node.textContent = v;
      else if (k === 'html') node.innerHTML = v;
      else if (k.startsWith('on')) node.addEventListener(k.slice(2).toLowerCase(), v);
      else node.setAttribute(k, v === true ? '' : v);
    }
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    node.append(c.nodeType ? c : document.createTextNode(String(c)));
  }
  return node;
}

export const clear = (node) => {
  while (node.firstChild) node.removeChild(node.firstChild);
  return node;
};

export function card(title, ...body) {
  const head = title ? el('div.card-head', el('div.card-title', { text: title })) : null;
  return el('section.card', head, ...body);
}

/** A card whose header carries controls on the right. */
export function cardWith(title, controls, ...body) {
  return el(
    'section.card',
    el('div.card-head', el('div.card-title', { text: title }), controls),
    ...body
  );
}

export function statTile(label, value, sub, hint) {
  return el(
    'div.stat',
    el('div.stat-label', { title: hint }, label),
    el('div.stat-value.num', { title: typeof value === 'string' ? undefined : value }, value),
    sub === undefined || sub === null ? null : el('div.stat-sub', sub)
  );
}

/* ------------------------------------------------------- balancing .stats --- */

// The minimum tile width, which must match the minmax() floor in the .stats rule.
const STAT_MIN = 180;
// The grid's gap, in the same place for the same reason.
const STAT_GAP = 1;

/**
 * How many columns to use for `count` tiles when `fits` would have fitted.
 *
 * `repeat(auto-fit, minmax(180px, 1fr))` packs as many columns as fit and lets the
 * remainder fall onto the next row, so eight tiles in a five-column width render as a
 * full row of five with a stranded trio beneath it. It reads as a layout bug because
 * in every sense except the CSS it is one: the grid had no reason to prefer five.
 *
 * Grid cannot express "and balance the rows" — the choice depends on the item count,
 * which is arithmetic CSS has no access to. So: take the columns that would have
 * fitted, work out how many rows that needs, then use the fewest columns that still
 * fills those rows. Eight in a five-wide space become four and four; nine stay five
 * and four, because that is already even.
 *
 * Never more columns than would have fitted, so this only ever makes tiles wider.
 *
 * Split out from the DOM so the arithmetic can be checked directly — it is the whole
 * behaviour, and the rest is reading a width and writing a style.
 */
export function statColumns(count, fits) {
  const usable = Math.min(count, Math.max(1, fits));
  const rows = Math.ceil(count / usable);
  return Math.ceil(count / rows);
}

function balanceOne(grid) {
  const n = grid.childElementCount;
  const w = grid.clientWidth;
  if (!n || !w) return;
  const fits = Math.floor((w + STAT_GAP) / (STAT_MIN + STAT_GAP));
  grid.style.gridTemplateColumns = `repeat(${statColumns(n, fits)}, minmax(0, 1fr))`;
}

/** Balance every stat grid under root. Cheap enough to do wholesale. */
export function balanceStats(root = document) {
  for (const grid of root.querySelectorAll('.stats')) balanceOne(grid);
}

export function skeleton(height = 200) {
  return el('div.skeleton', { style: `height:${height}px` });
}

// A quiet render replaces the page's data without tearing it down first.
//
// Every view opens with loading(), which is right on navigation — there is nothing
// on screen worth keeping. On an in-place refresh it is wrong: the reader is looking
// at the very content it would blank, and a full skeleton flash every hour to swap
// numbers that barely moved reads as a fault, not an update.
let quiet = false;

export function setQuiet(v) {
  quiet = v;
}

export function loading(view) {
  if (quiet) return;
  clear(view).append(
    el('div.page-head', el('div.skeleton', { style: 'height:32px;width:280px' })),
    el('div', { style: 'height:16px' }),
    skeleton(96),
    el('div', { style: 'height:24px' }),
    skeleton(320)
  );
}

export function errorView(view, err) {
  clear(view).append(
    el(
      'div.card',
      el(
        'div.error',
        el('div', { style: 'font-size:15px;margin-bottom:8px' },
          err?.status === 404 ? 'Not found' : 'Something went wrong'),
        el('div.muted', { text: err?.message || String(err) })
      )
    )
  );
}

/**
 * Pagination.
 *
 * The leaderboards run to tens of thousands of pages, so the control shows the
 * ends, a window around the current page, and a jump box — never a strip of a
 * thousand numbers.
 */
export function pager(page, totalPages, totalItems, onGo) {
  const wrap = el('div.pager');
  const btn = (label, target, opts = {}) =>
    el('button', {
      text: label,
      disabled: opts.disabled,
      'aria-current': opts.current ? 'true' : null,
      title: opts.title,
      onclick: () => onGo(target),
    });

  if (totalPages <= 1) {
    wrap.append(el('span.count', `${totalItems.toLocaleString()} total`));
    return wrap;
  }

  wrap.append(btn('‹', page - 1, { disabled: page <= 1, title: 'Previous page' }));

  const window_ = [];
  const push = (p) => { if (p >= 1 && p <= totalPages && !window_.includes(p)) window_.push(p); };
  push(1);
  for (let p = page - 2; p <= page + 2; p++) push(p);
  push(totalPages);
  window_.sort((a, b) => a - b);

  let prev = 0;
  for (const p of window_) {
    if (p - prev > 1) wrap.append(el('span.muted', { style: 'padding:0 2px' }, '…'));
    wrap.append(btn(p.toLocaleString(), p, { current: p === page }));
    prev = p;
  }

  wrap.append(btn('›', page + 1, { disabled: page >= totalPages, title: 'Next page' }));
  wrap.append(el('span.count', `${totalItems.toLocaleString()} total`));
  return wrap;
}

/** A segmented control. Returns the element; `onPick` receives the chosen value. */
export function segmented(options, current, onPick) {
  const wrap = el('div.seg');
  for (const o of options) {
    wrap.append(
      el('button', {
        text: o.label,
        title: o.title,
        'aria-pressed': o.value === current ? 'true' : 'false',
        onclick: () => onPick(o.value),
      })
    );
  }
  return wrap;
}

/**
 * A warning beside a ⚠, for something the reader needs before they act on the page.
 *
 * Variadic rather than one string so a notice can carry a link or an emphasised lead —
 * a caveat people are meant to act on usually has somewhere to send them. Passing a
 * single string still builds exactly what it always did.
 */
export function notice(...body) {
  return el('div.notice', el('span', '⚠'), el('span', ...body));
}

/** Link that routes through the SPA rather than reloading. */
export function link(href, ...children) {
  return el('a', { href }, ...children);
}
