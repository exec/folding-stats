package web

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func TestMilestonesDoNotBlockDetailRequests(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping milestone request check")
	}
	src, err := assets.ReadFile("views.js")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(src), "function nextPointTarget(")
	end := strings.Index(string(src), "\n/**\n * Embeddable badges")
	if start < 0 || end < start {
		t.Fatal("milestone functions not found")
	}
	// Run the shipped milestone functions with a pending API response and tiny DOM
	// stand-ins: the card must return before that response is resolved.
	check := `
import assert from 'node:assert/strict';
const nodes = [], requests = [];
const el = (spec, ...children) => {
  const node = {spec, children}; nodes.push(node); return node;
};
const card = el, statTile = el, n = String, short = String, t = String, overtakeIn = String;
const tr = (s, v) => s.replace('{0}', v);
const api = {goal: (params) => new Promise((resolve, reject) => requests.push({params, resolve, reject}))};
` + string(src[start:end]) + `
const entity = {team_id: 7, name: 'Donor', rank: 321, points_total: 1000};
const cardNode = milestoneSection(entity, 'team');
assert.ok(cardNode && !(cardNode instanceof Promise), 'goal response blocks the rest of the detail page');
assert.equal(requests.length, 1);
assert.equal(requests[0].params.who, 7);
assert.equal(requests[0].params.target_rank, 300);
const note = nodes.find((node) => node.spec === 'span');
requests[0].resolve({data: {horizons: [{days: 90, points_per_day: 500}]}});
await new Promise(setImmediate);
assert.equal(note.textContent, '500 PPD needed in 90 days');
milestoneSection(entity, 'donor');
assert.equal(requests[1].params.who, 'Donor');
requests[1].reject(new Error('offline'));
await new Promise(setImmediate);
assert.equal(nodes.filter((node) => node.spec === 'span')[1].textContent, 'Couldn’t load rank target');
milestoneSection({...entity, rank: 1}, 'team');
assert.equal(requests.length, 2, 'rank 1 needs no goal request');
`
	if out, err := exec.Command(node, "--input-type=module", "--eval", check).CombinedOutput(); err != nil {
		t.Fatalf("milestone request check: %v\n%s", err, out)
	}
}

func BenchmarkStaticAsset(b *testing.B) {
	h, err := Handler(nil)
	if err != nil {
		b.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/views.js", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}
