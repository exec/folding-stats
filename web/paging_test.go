package web

import (
	"os/exec"
	"strings"
	"testing"
)

func TestPagingCardsKeepTheLatestSelection(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping paging race check")
	}
	src, err := assets.ReadFile("views.js")
	if err != nil {
		t.Fatal(err)
	}
	part := func(start, end string) string {
		t.Helper()
		a, b := strings.Index(string(src), start), strings.Index(string(src), end)
		if a < 0 || b < a {
			t.Fatalf("missing card %s", start)
		}
		return string(src[a:b])
	}
	check := `
import assert from 'node:assert/strict';
const RIVALS_PER_PAGE = 10, PER_PAGE = 50;
const warnings = [];
console.warn = (...args) => warnings.push(args);
const el = (spec, ...args) => {
  const node = {spec, args, children: [...args], classes: new Set(),
    append(...items) { this.children.push(...items); }};
  node.classList = {add: (name) => node.classes.add(name), remove: (name) => node.classes.delete(name)};
  return node;
};
const clear = (node) => { node.children = []; return node; };
const cardWith = (title, controls, body) => el('card', controls, body);
const card = (title, body) => el('card', body);
const t = (s) => s, tr = (s) => s, activeWindow = () => 'week';
const rivalsTable = (data) => el('rivals', data);
const pager = (page, pages, count, go) => ({page, go});
let filterPick, sortPick;
const segmented = (values, current, pick) => { filterPick = pick; return el('seg'); };
const memberTable = (data, sort, pick) => { sortPick = pick; return el('members', {data, sort}); };
const deferred = () => {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return {promise, resolve, reject};
};
const requests = [];
const load = (params) => {
  const d = deferred();
  requests.push({...d, params, resolve: (name) => d.resolve({
    data: {name, horizon_days: 3650}, page: {page: params.page, total_pages: 10, total_items: 100},
  })});
  return d.promise;
};
const api = {teamMembers: (team, params) => {
  const d = deferred();
  requests.push({...d, params, resolve: (name) => d.resolve({
    data: [{name}], page: {page: params.page, total_pages: 10, total_items: 100},
  })});
  return d.promise;
}};
const flush = () => new Promise(setImmediate);
const loading = (view) => clear(view), n = (v) => v;
const nameText = (node, text) => node.append(text), errorView = (view, err) => { throw err; };
const urls = [], history = {replaceState: (_, __, url) => urls.push(url)};
api.donorRivals = (id, params) => load(params);
api.teamRivals = api.donorRivals;
` + part("function rivalsCard(", "/**\n * The rivals card, or a note saying why it is not there.") +
		part("async function teamMembersCard(", "/**\n * The columns a team's roster can be ordered by") +
		strings.Replace(part("export async function rivalsPage(", "export async function searchPage("), "export async", "async", 1) + `
const rivals = rivalsCard(load, 'donor');
const body = rivals.node.children[0];
const a = rivals.go(1), b = rivals.go(2);
requests[0].resolve('old');
await a;
assert.ok(body.classes.has('swapping'), 'old completion must not clear current loading state');
assert.equal(body.children.length, 0, 'old page must not render while a newer page is pending');
requests[1].resolve('new');
await b;
assert.equal(body.children[0].args[0].name, 'new');
assert.ok(!body.classes.has('swapping'));
const c = rivals.go(3), d = rivals.go(4);
requests[3].resolve('newest');
await d;
requests[2].reject(new Error('old failure'));
await c;
assert.equal(body.children[0].args[0].name, 'newest', 'obsolete error must not erase the current page');
assert.equal(warnings.length, 0);
const e = rivals.go(5);
requests[4].reject(new Error('current failure'));
await e;
assert.equal(warnings.length, 1, 'current failures must remain visible');
assert.ok(!body.classes.has('swapping'));

requests.length = 0;
const mounting = teamMembersCard(32);
requests[0].resolve('initial');
const roster = await mounting;
const membersBody = roster.children[1];
const shown = () => membersBody.children[0].args[0];
sortPick('this_week');
sortPick('this_month');
assert.equal(requests[1].params.sort, 'this_week');
assert.equal(requests[2].params.sort, 'this_month');
requests[2].resolve('monthly');
await flush();
requests[1].reject(new Error('old sort failure'));
await flush();
assert.equal(shown().sort, 'this_month');
assert.equal(shown().data[0].name, 'monthly');
filterPick('active');
filterPick('all');
assert.equal(requests[3].params.active_only, 'true');
assert.equal(requests[4].params.active_only, undefined);
requests[3].resolve('old active');
await flush();
assert.equal(membersBody.children[0].spec, 'div', 'obsolete filter must leave the new skeleton alone');
requests[4].resolve('all');
await flush();
assert.equal(shown().data[0].name, 'all');
const go = membersBody.children[1].go;
go(2); go(3);
requests[5].reject(new Error('old page failure'));
await flush();
assert.equal(membersBody.children[0].spec, 'div');
assert.equal(requests[6].params.page, 3);
requests[6].resolve('third page');
await flush();
assert.equal(shown().data[0].name, 'third page');
filterPick('active');
requests[7].reject(new Error('current roster failure'));
await flush();
assert.equal(membersBody.children[0].spec, 'div.error', 'current roster failures must remain visible');

// The standalone page already fetched its heading data; the card reuses it.
requests.length = 0;
const pageView = el('div');
const showing = rivalsPage(pageView, {kind: 'donor', id: 'DH', page: 2, teamID: '32'});
assert.equal(requests[0].params.team_id, '32');
requests[0].resolve('DH');
await flush();
assert.equal(requests.length, 1, 'the standalone rivals page must fetch its initial data only once');
await showing;
const section = pageView.children.at(-1);
const pageBody = section.children[0].children[0];
const goPage = pageBody.children[1].go;
goPage(3);
assert.equal(urls.at(-1), '/donors/DH/rivals?page=3&team_id=32', 'paging must preserve team scope in shared links');
requests[1].resolve('page 3');
await flush();
goPage(1);
assert.equal(urls.at(-1), '/donors/DH/rivals?team_id=32');
requests[2].resolve('page 1');
await flush();
`
	if out, err := exec.Command(node, "--input-type=module", "--eval", check).CombinedOutput(); err != nil {
		t.Fatalf("paging race check: %v\n%s", err, out)
	}
}
