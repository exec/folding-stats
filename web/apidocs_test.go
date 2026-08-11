package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The smallest DOM these views actually touch. Not a browser and not trying to be —
// enough to run the builders and catch the failure that matters, which is one of them
// throwing. A page that throws while rendering leaves a blank panel and a console
// nobody is watching, and every check in this package until now inspected the source
// rather than running it.
//
// nodeType is the load-bearing part. el() distinguishes an attributes object from a
// child by testing for it, so a node without one is read as a bag of attributes and
// the tree comes out empty rather than wrong-looking — which is exactly how this shim
// failed first time and would fail again if it were trimmed.
const domShim = `
class N {
  get nodeType(){ return this.tagName === '#TEXT' ? 3 : 1; }
  get firstChild(){ return this.children[0] ?? null; }
  removeChild(c){ const i=this.children.indexOf(c); if(i>=0) this.children.splice(i,1); return c; }
  set innerHTML(v){ this._text=String(v); this.children=[]; }
  constructor(tag){ this.tagName=(tag||'').toUpperCase(); this.children=[]; this.attrs={}; this.style={}; this.classList=new Set(); this._text=''; }
  append(...cs){ for(const c of cs){ if(c===null||c===undefined) continue; this.children.push(c); } }
  appendChild(c){ this.append(c); }
  setAttribute(k,v){ this.attrs[k]=String(v); }
  removeAttribute(k){ delete this.attrs[k]; }
  addEventListener(){}
  _match(sel){ return sel.startsWith('.') ? this.classList.has(sel.slice(1)) : this.tagName === sel.toUpperCase(); }
  querySelectorAll(sel){
    const out=[];
    for (const c of this.children){
      if (typeof c === 'string') continue;
      if (c._match && c._match(sel)) out.push(c);
      if (c.querySelectorAll) out.push(...c.querySelectorAll(sel));
    }
    return out;
  }
  querySelector(sel){ return this.querySelectorAll(sel)[0] || null; }
  get textContent(){ return this._text + this.children.map(c=>typeof c==='string'?c:c.textContent).join(''); }
  set textContent(v){ this._text=String(v); this.children=[]; }
  set className(v){ this.classList=new Set(String(v).split(/\s+/).filter(Boolean)); }
  get className(){ return [...this.classList].join(' '); }
  remove(){}
}
globalThis.document = {
  createElement:(t)=>new N(t),
  createElementNS:(ns,t)=>new N(t),
  createTextNode:(t)=>{ const n=new N('#text'); n._text=String(t); return n; },
  documentElement:new N('html'), head:new N('head'), body:new N('body'),
  addEventListener(){}, querySelector(){return null;},
};
globalThis.window = { addEventListener(){}, dispatchEvent(){}, location:{origin:'https://foldingstats.org', pathname:'/api', search:''} };
globalThis.location = window.location;
globalThis.history = { pushState(){}, replaceState(){} };
globalThis.setInterval = () => 0;
globalThis.clearInterval = () => {};
globalThis.requestAnimationFrame = () => 0;
globalThis.devicePixelRatio = 1;
globalThis.matchMedia = () => ({ matches:false, addEventListener(){}, removeEventListener(){}, addListener(){}, removeListener(){} });
window.matchMedia = globalThis.matchMedia;
Object.defineProperty(globalThis,'navigator',{value:{language:'en-US',userAgent:'node'},configurable:true});
globalThis.CustomEvent = class { constructor(t,o){ this.type=t; Object.assign(this,o||{}); } };
// Every fetch fails, so this also asserts the page survives having no API at all —
// which is the state a first-time visitor on a cold CDN briefly has.
globalThis.fetch = () => Promise.reject(new Error('offline render'));
export { N };
`

const apiDocsDriver = `
import './shim.mjs';
const views = await import('./views.mjs');
const { N } = await import('./shim.mjs');

const view = new N('div');
await views.apiDocs(view);

const counts = {};
(function walk(n){
  if (typeof n === 'string') return;
  for (const c of n.classList || []) counts[c] = (counts[c]||0)+1;
  for (const c of n.children || []) walk(c);
})(view);

let bad = 0;
const need = (cls, min) => {
  if ((counts[cls] || 0) < min) {
    console.log('FAIL .' + cls + ': ' + (counts[cls] || 0) + ', want at least ' + min);
    bad++;
  }
};
// Every documented route, and parameters actually rendered rather than the empty
// scaffolding a broken loop would leave behind.
need('endpoint', 14);
need('endpoint-method', 14);
need('endpoint-path', 14);
need('endpoint-sum', 14);
need('params', 8);
need('param-name', 30);
need('param-values', 30);
need('param-note', 30);

// The regression this layout exists to prevent: a summary and its query parameters
// sharing one cell. Nothing may put a '?' in the summary line any more.
const sums = [];
(function walk(n){
  if (typeof n === 'string') return;
  if ((n.classList || new Set()).has('endpoint-sum')) sums.push(n.textContent);
  for (const c of n.children || []) walk(c);
})(view);
for (const s of sums) {
  if (s.includes('?')) { console.log('FAIL summary carries a query parameter: ' + s); bad++; }
}
if (!bad) console.log('OK');
`

// TestAPIDocsRender runs the /api page's builder against a minimal DOM.
//
// It is the only check here that executes a view rather than reading it. The page is
// the front door for anybody deciding whether to build against this service, and the
// way it breaks is silent: a throw part-way through leaves whatever had been appended
// and nothing else, which looks like a short page rather than an error.
//
// It also pins the shape the redesign was for. Query parameters used to share a table
// cell with the summary, so a route with four of them became a paragraph wedged into a
// third of the width; the assertion that no summary contains a "?" is what stops that
// creeping back one convenient string at a time.
func TestAPIDocsRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping API docs render")
	}

	dir := t.TempDir()
	names, err := scriptNames()
	if err != nil {
		t.Fatal(err)
	}
	abs := regexp.MustCompile(`'/(vendor/)?([A-Za-z.]+)\.(esm\.js|js)'`)
	for _, name := range names {
		src, err := assets.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		out := abs.ReplaceAllString(string(src), "'./${1}${2}.mjs'")
		dst := filepath.Join(dir, strings.TrimSuffix(name, ".js")+".mjs")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte(out), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"shim.mjs": domShim, "driver.mjs": apiDocsDriver} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	out, err := exec.Command(node, filepath.Join(dir, "driver.mjs")).CombinedOutput()
	if err != nil {
		t.Fatalf("the /api page threw while rendering: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "OK" {
		t.Errorf("/api rendered the wrong shape:\n%s", got)
	}
}
