package web

import (
	"os/exec"
	"strings"
	"testing"
)

func TestContributionCardIgnoresObsoleteLoads(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping contribution lifecycle check")
	}
	src, err := assets.ReadFile("views.js")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(src), "function contributionCard(")
	end := strings.Index(string(src), "/** A donor's production, split across the teams they fold for. */")
	if start < 0 || end < start {
		t.Fatal("contribution card not found")
	}
	check := `
import assert from 'node:assert/strict';
const GRANULARITIES = 'granularity', RATES = 'rate', SHAPES = 'shape', MAX_STACK_SERIES = 2;
const picks = {}, renders = [], charts = [], nodes = [];
let currentSnapshot = {at: '2026-10-28T12:00:00Z'}, prepared = 0;
const snapshot = () => currentSnapshot;
const el = (spec, ...args) => {
  const node = {spec, args, children: [], append(...items) { this.children.push(...items); }};
  nodes.push(node);
  return node;
};
const clear = (node) => { node.children = []; return node; };
const t = (s) => s, tr = (s) => s, short = (v) => v, chartNote = () => '';
const cardWith = () => el('card');
const segmented = (kind, current, pick) => { picks[kind] = pick; return el('seg'); };
const chartSpan = () => ({}), emptyChart = () => el('empty'), legend = () => {};
const palette = () => ['red', 'blue'], stack = (rows) => rows;
const perDayPoints = (pts) => pts.map((p) => ({...p, points: p.points * 10}));
const densify = (pts, g, {perDay}) => { prepared++; return perDay ? perDayPoints(pts) : pts; };
const productionChart = () => {
  const chart = {destroyed: false, destroy() { this.destroyed = true; },
    render(data, meta) { renders.push({data, meta}); }};
  charts.push(chart);
  return chart;
};
const seriesChart = productionChart;
const flush = () => new Promise(setImmediate);
const deferred = () => {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return {promise, resolve, reject};
};
const mount = (count = 1, refresh) => {
  const requests = [];
  const card = contributionCard({title: 'Production', noun: 'member', total: 100, refresh,
    items: Array.from({length: count}, (_, i) => ({key: String(i), label: 'member' + i, value: 10})),
    fetchFor: (key, params) => {
      const d = deferred();
      requests.push({...d, key, params, resolve: (points = 10) => d.resolve({data: {points: [
        {at: '2026-10-28T12:00:00Z', points},
      ]}})});
      return d.promise;
    },
  });
  return {card, requests};
};
` + string(src[start:end]) + `
// Cover disposal during the single, multi-series, and Other-tail awaits.
for (const count of [1, 2, 3]) {
  const {card, requests} = mount(count);
  if (count === 3) {
    requests[0].resolve(); requests[1].resolve();
    await flush();
    assert.equal(requests.length, 3);
  }
  const before = charts.length;
  card.destroy();
  for (const req of requests) req.resolve();
  await flush();
  assert.equal(charts.length, before, 'disposed card must never create a chart');
}

// A late initial refresh must not start history requests after cleanup.
const refresh = deferred();
const pending = mount(2, () => refresh.promise);
pending.card.destroy();
refresh.resolve([{key: 'new', label: 'new', value: 10}]);
await flush();
assert.equal(pending.requests.length, 0);

const {card, requests} = mount();
picks.granularity('daily');
picks.rate('per-day');
assert.equal(requests.length, 2, 'rate changes must reuse the pending history');
requests[1].resolve(20);
await flush();
const current = renders.at(-1);
assert.deepEqual(current.data[1], [200]);
requests[0].resolve(1);
await flush();
assert.equal(renders.at(-1), current, 'old response must not overwrite the current rate');
picks.granularity('monthly');
picks.granularity('weekly');
requests[3].resolve();
await flush();
requests[2].reject(new Error('obsolete failure'));
await flush();
assert.ok(!nodes.some((node) => node.spec === 'div.error'), 'obsolete failure must stay invisible');
card.destroy();

// A selected contributor can supersede a pending stack, including its Other tail.
const multi = mount(3);
multi.requests[0].resolve(); multi.requests[1].resolve();
await flush();
const tabs = nodes.findLast((node) => node.spec === 'div.tabs');
tabs.children[1].args[0].onclick();
picks.granularity('daily');
multi.requests[3].resolve(42);
await flush();
const selected = renders.at(-1);
assert.deepEqual(selected.data[1], [42]);
multi.requests[2].resolve();
await flush();
assert.equal(renders.at(-1), selected, 'old Other tail must not replace the selected contributor');
// The selected-contributor await must also obey cleanup.
picks.granularity('monthly');
const before = charts.length;
multi.card.destroy();
multi.requests[4].resolve();
await flush();
assert.equal(charts.length, before);
assert.ok(charts.every((chart) => chart.destroyed), 'all created charts must be cleaned up');

// Total/rate and contributor changes reuse resolved histories, including the tail.
const cached = mount(3);
picks.shape('overlaid');
assert.equal(cached.requests.length, 2, 'pending shape changes must share shown requests');
cached.requests[0].resolve(10); cached.requests[1].resolve(20);
await flush();
assert.equal(cached.requests.length, 3);
cached.requests[2].resolve(30);
await flush();
assert.deepEqual(renders.at(-1).data.slice(1), [[10], [20], [30]]);
const arrays = renders.at(-1).data, prep = prepared, chartCount = charts.length;
picks.shape('stacked');
assert.equal(prepared, prep, 'shape changes must reuse prepared series');
assert.equal(charts.length, chartCount, 'shape changes must reuse the chart observer');
assert.equal(renders.at(-1).data[0], arrays[0]);
assert.equal(cached.requests.length, 3);
picks.rate('per-day');
await flush();
assert.equal(cached.requests.length, 3, 'rate changes must reuse every contributor history');
assert.deepEqual(renders.at(-1).data.slice(1), [[100], [200], [300]]);
const cachedTabs = nodes.findLast((node) => node.spec === 'div.tabs');
cachedTabs.children[1].args[0].onclick();
await flush();
assert.equal(cached.requests.length, 3, 'a contributor tab must reuse its history');
assert.deepEqual(renders.at(-1).data[1], [100]);
currentSnapshot = {at: '2026-10-28T13:00:00Z'};
picks.rate('total');
assert.equal(cached.requests.length, 4, 'a new snapshot must invalidate history');
cached.requests[3].reject(new Error('retry me'));
await flush();
picks.rate('per-day');
assert.equal(cached.requests.length, 5, 'failed histories must be retryable');
cached.requests[4].resolve();
await flush();
cached.card.destroy();
assert.ok(charts.every((chart) => chart.destroyed));

const fresh = mount(2);
fresh.requests[0].resolve(); fresh.requests[1].resolve();
await flush();
currentSnapshot = {at: '2026-10-28T14:00:00Z'};
picks.shape('overlaid');
assert.equal(fresh.requests.length, 4, 'shape changes must reload after a new snapshot');
fresh.card.destroy();
fresh.requests[2].resolve(); fresh.requests[3].resolve();
await flush();
assert.ok(charts.every((chart) => chart.destroyed));
`
	if out, err := exec.Command(node, "--input-type=module", "--eval", check).CombinedOutput(); err != nil {
		t.Fatalf("contribution lifecycle check: %v\n%s", err, out)
	}
}
