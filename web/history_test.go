package web

import (
	"os/exec"
	"strings"
	"testing"
)

func TestHistoryCardReusesDataForPresentationChanges(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping history request check")
	}
	src, err := assets.ReadFile("views.js")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(src), "function historyCard(")
	end := strings.Index(string(src), "\n/**\n * Who turned up in the last day.")
	if start < 0 || end < start {
		t.Fatal("history card not found")
	}
	check := `
import assert from 'node:assert/strict';
const nodes = [], requests = [], renders = [], downloads = [], picks = {};
const HISTORY_SHAPES = 'shape', GRANULARITIES = 'granularity', RATES = 'rate';
const DAY_MS = 86400e3;
let currentSnapshot = {at: '2026-10-28T12:00:00Z'};
let prepared = 0, destroyed = 0;
const snapshot = () => currentSnapshot;
const snapshotMs = () => Date.parse(snapshot().at);
const chartSpan = () => ({until: snapshotMs(), since: null});
const t = (v) => v, chartNote = (g, r) => g + ':' + r;
const el = (spec, ...args) => {
  const node = {spec, args, children: [...args], append(...items) { this.children.push(...items); }};
  nodes.push(node);
  return node;
};
const clear = (node) => { node.children = []; return node; };
const cardWith = (title, controls, body) => el('section.card', controls, body);
const segmented = (kind, current, pick) => { picks[kind] = pick; return el('seg', current); };
const emptyChart = (g) => el('empty', g);
const productionHeatmap = (points) => el('heatmap', points);
const productionChart = () => ({
  render: (data, meta) => renders.push({data, meta}),
  destroy: () => { destroyed++; },
});
const densify = (points, granularity, {perDay}) => {
  prepared++;
  return points.map((p) => ({...p, points: p.points * (perDay ? 10 : 1)}));
};
const downloadHistory = (title, history) => downloads.push({title, history});
const fetcher = (params) => new Promise((resolve, reject) => {
  const request = {params, reject, resolve: (points = [{at: snapshot().at, points: 12, wus: 2}]) => {
    request.data = {granularity: params.granularity, points};
    resolve({data: request.data});
  }};
  requests.push(request);
});
const flush = () => new Promise(setImmediate);
const lastRender = () => renders.at(-1);
const download = () => {
  const controls = nodes.find((node) => node.spec === 'div.chart-toolbar');
  const button = controls.children.find((node) => node.spec === 'button.linkish.export-btn');
  assert.ok(button, 'daily and monthly history must remain exportable');
  button.args[0].onclick();
};
` + string(src[start:end]) + `
const card = historyCard('Production', fetcher);
assert.equal(requests.length, 1);
picks.shape('bars');
picks.rate('per-day');
assert.equal(requests.length, 1, 'appearance changes must share the pending fetch');
requests[0].resolve();
await flush();
assert.equal(renders.length, 1, 'only the newest presentation should render');
assert.deepEqual(lastRender().meta, {granularity: 'hourly', perDay: true, shape: 'bars'});
assert.deepEqual(lastRender().data[1], [120]);
const perDay = lastRender().data;
picks.shape('lines');
await flush();
assert.equal(lastRender().data, perDay, 'line/bar changes must reuse prepared arrays');
picks.rate('total');
await flush();
assert.deepEqual(lastRender().data[1], [12]);
picks.rate('per-day');
await flush();
assert.equal(lastRender().data, perDay, 'switching back must reuse the prepared rate');
assert.equal(prepared, 2);
assert.equal(requests.length, 1);

picks.granularity('daily');
assert.equal(requests.length, 2, 'granularity changes need new history');
picks.rate('total');
assert.equal(requests.length, 2);
requests[1].resolve();
await flush();
download();
assert.equal(downloads.at(-1).history, requests[1].data);
picks.shape('bars');
await flush();
assert.equal(requests.length, 2);
download();
assert.equal(downloads.at(-1).history, requests[1].data);

// Heatmap and daily chart requests have different ranges, despite sharing a granularity.
picks.shape('heatmap');
assert.equal(requests.length, 3);
assert.equal(requests[2].params.granularity, 'daily');
assert.equal(Date.parse(requests[2].params.from), snapshotMs() - 5 * 365 * DAY_MS);
assert.equal(Date.parse(requests[2].params.to), snapshotMs());
requests[2].resolve();
await flush();
assert.equal(nodes.find((node) => node.spec === 'div.history-visual').children[0].spec, 'heatmap');
download();
assert.equal(downloads.at(-1).history, requests[2].data);
picks.shape('lines');
assert.equal(requests.length, 4);
assert.equal(requests[3].params.from, undefined);
requests[3].resolve();
await flush();

// Snapshot changes invalidate both the loaded response and its prepared arrays.
currentSnapshot = {at: '2026-10-28T13:00:00Z'};
picks.shape('bars');
assert.equal(requests.length, 5);
requests[4].resolve([]);
await flush();
assert.equal(lastRender().data, null);
picks.rate('per-day');
await flush();
assert.equal(requests.length, 5, 'an empty history response is still reusable');

// Failed fetches can be retried; older completions cannot replace a newer range.
picks.granularity('monthly');
requests[5].reject(new Error('offline'));
await flush();
picks.shape('lines');
assert.equal(requests.length, 7, 'a rejected fetch must not stay cached');
picks.granularity('weekly');
requests[7].resolve();
await flush();
const weekly = lastRender();
requests[6].resolve();
await flush();
assert.equal(lastRender(), weekly, 'late monthly history must not replace weekly history');
download();
assert.equal(downloads.at(-1).history, requests[7].data);

picks.granularity('daily');
picks.granularity('monthly');
requests[9].resolve();
await flush();
const monthly = lastRender();
requests[8].reject(new Error('late failure'));
await flush();
assert.equal(lastRender(), monthly, 'an obsolete failure must not erase the current chart');

picks.granularity('hourly');
card.destroy();
requests[10].resolve();
await flush();
assert.equal(lastRender(), monthly, 'disposed cards must ignore pending history');
assert.equal(destroyed, 1);
`
	if out, err := exec.Command(node, "--input-type=module", "--eval", check).CombinedOutput(); err != nil {
		t.Fatalf("history request check: %v\n%s", err, out)
	}
}
