package web

import (
	"os/exec"
	"strings"
	"testing"
)

func TestNavigationOwnsItsViewAndCleanup(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping navigation race check")
	}
	src, err := assets.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(src), "const SORT_VALUES =")
	end := strings.Index(string(src), "// Intercept in-app links")
	if start < 0 || end < start {
		t.Fatal("router not found")
	}
	// Run the shipped router with deferred handlers so both response orders are deterministic.
	check := `
import assert from 'node:assert/strict';
class Node {
  childNodes = [];
  parentNode = null;
  classes = new Set();
  classList = {
    toggle: (name, on) => on ? this.classes.add(name) : this.classes.delete(name),
    add: (name) => this.classes.add(name),
    remove: (name) => this.classes.delete(name),
  };
  get firstChild() { return this.childNodes[0]; }
  append(...nodes) {
    for (const node of nodes) {
      node.parentNode?.removeChild(node);
      this.childNodes.push(node);
      node.parentNode = this;
    }
  }
  removeChild(node) {
    this.childNodes.splice(this.childNodes.indexOf(node), 1);
    node.parentNode = null;
  }
}

const view = new Node(), summary = new Node();
view.append(summary);
const el = () => new Node();
const clear = (node) => {
  while (node.firstChild) node.removeChild(node.firstChild);
  return node;
};
const events = {}, calls = [], disposed = [];
const document = {querySelectorAll: () => [], addEventListener: (name, fn) => events[name] = fn};
const window = {scrollTo: () => {}};
const location = {pathname: '/teams/1', search: ''};
const history = {pushState: (_, __, to) => {
  const url = new URL(to, 'http://localhost');
  location.pathname = url.pathname;
  location.search = url.search;
}};
let cleanup = null, quietMode = false, cacheMode = 'default';
const setQuiet = (v) => quietMode = v;
const setCacheMode = (v) => cacheMode = v;
const views = {teamDetail: (target, {id}) => new Promise((resolve) => {
  const call = {target, id, resolve: () => {
    clear(target).append(el());
    resolve(() => disposed.push(call));
  }};
  calls.push(call);
})};
` + strings.ReplaceAll(string(src[start:end]), "export function navigate", "function navigate") + `
const a = render();
assert.equal(calls[0].target.firstChild, summary, 'initial server summary is preserved');
location.pathname = '/teams/2';
const b = render();
calls[1].resolve();
await b;
const current = view.firstChild;
calls[0].resolve();
await a;
assert.equal(view.firstChild, current, 'late A must not replace B');
assert.equal(calls.length, 2, 'late A must not reload B');
assert.deepEqual(disposed, [calls[0]], 'late A cleans up itself, not B');

// A quiet refresh keeps the visible content and cannot reset a newer tab's state.
const refresh = events.dataupdate();
assert.equal(view.firstChild, current);
assert.equal(cacheMode, 'reload');
location.pathname = '/teams/3';
const tab = render({keepContent: true});
assert.equal(view.firstChild, current);
calls[2].resolve();
await refresh;
assert.equal(rendering, true);
assert.equal(quietMode, true);
assert.ok(view.classes.has('swapping'));
assert.ok(!view.classes.has('refreshed'), 'superseded refresh must not flash the new page');
calls[3].resolve();
await tab;
assert.equal(view.firstChild, calls[3].target);
assert.equal(rendering, false);
assert.equal(quietMode, false);
assert.equal(cacheMode, 'default');
assert.ok(!view.classes.has('swapping'));
assert.equal(disposed.filter((call) => call === calls[1]).length, 1, 'B cleanup runs exactly once');

// The older render may also finish first, even when both renders have the same URL.
const old = render({keepContent: true});
const newest = render({quiet: true});
calls[4].resolve();
await old;
assert.equal(cacheMode, 'reload', 'older finally must not reset the refresh cache mode');
assert.equal(rendering, true);
calls[5].resolve();
await newest;
assert.equal(view.firstChild, calls[5].target);
assert.ok(disposed.includes(calls[4]));

// A missing route still supersedes pending work and releases the current page.
const pending = render();
location.pathname = '/missing';
await render();
const missing = [...view.childNodes];
calls[6].resolve();
await pending;
assert.deepEqual(view.childNodes, missing);
assert.ok(disposed.includes(calls[5]));
assert.ok(disposed.includes(calls[6]));
assert.equal(calls.length, 7);
`
	if out, err := exec.Command(node, "--input-type=module", "--eval", check).CombinedOutput(); err != nil {
		t.Fatalf("navigation race check: %v\n%s", err, out)
	}
}

func TestChartCannotRenderAfterNavigationCleanup(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping late chart response check")
	}
	src, err := assets.ReadFile("charts.js")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(src), "class Chart {")
	if start < 0 {
		t.Fatal("chart class not found")
	}
	end := strings.Index(string(src[start:]), "\n/**") + start
	if end < start {
		t.Fatal("chart class not found")
	}
	check := `
import assert from 'node:assert/strict';
let plots = 0, destroyed = 0, disconnected = 0;
const listeners = new Set();
const document = {
  createElement: () => ({}),
  addEventListener: (_, fn) => listeners.add(fn),
  removeEventListener: (_, fn) => listeners.delete(fn),
};
const theme = () => ({});
class ResizeObserver { observe() {} disconnect() { disconnected++; } }
class uPlot { constructor() { plots++; } destroy() { destroyed++; } }
` + string(src[start:end]) + `
const host = {style: {}, appendChild() {}, classList: {add() {}, remove() {}}};
const chart = new Chart(host, () => ({}));
chart.render([[1], [2]], {});
chart.destroy();
chart.render([[1], [3]], {});
assert.equal(plots, 1, 'a late history response must not recreate the plot');
assert.equal(destroyed, 1);
assert.equal(disconnected, 1);
assert.equal(listeners.size, 0);
`
	if out, err := exec.Command(node, "--input-type=module", "--eval", check).CombinedOutput(); err != nil {
		t.Fatalf("late chart response check: %v\n%s", err, out)
	}
}
