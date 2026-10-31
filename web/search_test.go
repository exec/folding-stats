package web

import (
	"os/exec"
	"strings"
	"testing"
)

func TestLiveSearchCancelsObsoleteWork(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping live search lifecycle check")
	}
	src, err := assets.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(src), "const input = document.getElementById('q');")
	end := strings.Index(string(src), "document.addEventListener('keydown', (e) => {")
	if start < 0 || end < start {
		t.Fatal("live search not found")
	}
	check := `
import assert from 'node:assert/strict';
class Node {
  value = '';
  children = [];
  events = {};
  constructor(spec, args = []) { this.spec = spec; this.args = args; }
  append(...items) { this.children.push(...items); }
  addEventListener(event, fn) { this.events[event] = fn; }
  querySelectorAll() { return this.children.filter((c) => c.spec === 'div.search-result'); }
  blur() {}
  click() { this.args[0].onclick(); }
}
const field = new Node('input'), list = new Node('results'), events = {}, navigations = [];
const document = {
  getElementById: (id) => id === 'q' ? field : list,
  addEventListener: (event, fn) => events[event] = fn,
  querySelectorAll: () => [],
};
const el = (spec, ...args) => new Node(spec, args);
const clear = (node) => { node.children = []; return node; };
const tr = (s) => s, n = (v) => v, short = (v) => v;
const nameText = (node, name) => node.append(name);
const navigate = (url) => navigations.push(url);
const timers = new Map();
let nextTimer = 0;
const setTimeout = (fn) => { timers.set(++nextTimer, fn); return nextTimer; };
const clearTimeout = (id) => timers.delete(id);
const fireTimers = () => {
  for (const [id, fn] of [...timers]) { timers.delete(id); fn(); }
};
const requests = [];
const api = {search: (q, type, opts) => new Promise((resolve, reject) => {
  requests.push({q, opts, reject, resolve: (name = q) => resolve({data: {
    donors: [{name, points_total: 100}], teams: [], exact_donor: true,
  }})});
})};
const flush = () => new Promise(setImmediate);
const type = (q) => { field.value = q; field.events.input(); };
const key = (key) => field.events.keydown({key, preventDefault() {}});
` + string(src[start:end]) + `
type('a'); type('ab');
assert.equal(timers.size, 1, 'typing must replace the pending search');
assert.equal(requests.length, 0);
fireTimers();
assert.equal(requests.length, 1);
assert.equal(requests[0].q, 'ab');
assert.equal(requests[0].opts.limit, 8);
type('abc');
assert.ok(requests[0].opts.signal.aborted, 'typing must abort the old fetch immediately');
requests[0].resolve();
await flush();
assert.equal(list.children.length, 0, 'old results must stay hidden during debounce');
fireTimers();
requests[1].resolve();
await flush();
assert.equal(list.children.length, 1);
const current = list.children[0];

type('old'); fireTimers();
type('new'); fireTimers();
requests[3].resolve();
await flush();
const newest = list.children[0];
assert.notEqual(newest, current);
requests[2].reject(new Error('late obsolete failure'));
await flush();
assert.equal(list.children[0], newest, 'an obsolete failure must not dismiss new results');

type('escape'); fireTimers();
key('Escape');
assert.ok(requests[4].opts.signal.aborted);
requests[4].resolve();
await flush();
assert.equal(list.children.length, 0, 'dismissed results must not reopen');
type('queued escape');
key('Escape');
fireTimers();
assert.equal(requests.length, 5, 'dismissal must cancel the debounce timer');

type('outside'); fireTimers();
events.click({target: {closest: () => null}});
requests[5].resolve();
await flush();
assert.equal(list.children.length, 0, 'an outside click must invalidate pending results');

type('whole query');
key('Enter');
fireTimers();
assert.equal(navigations.at(-1), '/search?q=whole%20query');
assert.equal(requests.length, 6, 'navigation must cancel the queued dropdown request');
type('click'); fireTimers();
requests[6].resolve();
await flush();
list.children[0].click();
assert.equal(navigations.at(-1), '/donors/click');
assert.equal(field.value, '');
assert.equal(list.children.length, 0);

type('erase'); fireTimers();
type('');
assert.ok(requests[7].opts.signal.aborted);
requests[7].resolve();
await flush();
fireTimers();
assert.equal(requests.length, 8);
assert.equal(list.children.length, 0, 'clearing input must invalidate pending results');
type('current failure'); fireTimers();
requests[8].reject(new Error('unavailable'));
await flush();
assert.equal(list.children.length, 0, 'current errors must still dismiss results');
`
	if out, err := exec.Command(node, "--input-type=module", "--eval", check).CombinedOutput(); err != nil {
		t.Fatalf("live search lifecycle check: %v\n%s", err, out)
	}
}
