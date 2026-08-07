// The local Folding@home client, over its own websocket.
//
// This page talks to a program on the reader's own machine. Nothing about it goes
// through our server — it cannot, because fah-client binds 127.0.0.1 and refuses
// every other interface, so the only code that can reach it is code already running
// there. That is the whole architecture: the browser is the mediator, we are just the
// people who shipped the page.
//
// Two consequences worth stating where somebody will read them. The client will not
// talk to an origin it has not been told to trust, so a reader has to add ours by hand
// before any of this works — see the setup card on the page. And everything the client
// reports, including the passkey, arrives in the reader's browser and stays there
// unless they explicitly ask us to do something with it.

const WS_URL = 'ws://127.0.0.1:7396/api/websocket';

/**
 * Key normalisation, copied from the official client rather than invented.
 *
 * Short keys have their first hyphen turned into an underscore; long ones are left
 * alone so unit ids — 43 characters of base64 — are never mangled. Reimplementing this
 * differently would desynchronise our state tree from the one the client thinks it is
 * patching, and the symptom would be a field that silently stops updating.
 */
export function cleanKey(key) {
  if (typeof key === 'string' && key.length <= 16) return key.replace('-', '_');
  return key;
}

function cleanKeys(data) {
  if (Array.isArray(data)) return data.map(cleanKeys);
  if (data !== null && typeof data === 'object') {
    const out = {};
    for (const [k, v] of Object.entries(data)) out[cleanKey(k)] = cleanKeys(v);
    return out;
  }
  return data;
}

/**
 * Apply one update message to the state tree.
 *
 * Updates are a path followed by a value — `["units", 0, "eta", "1h 46m"]` — which is
 * why they cost 29 bytes instead of resending three kilobytes of state every second.
 * The last two elements are the key and the value; everything before them is the path,
 * and missing containers are created as they are walked.
 *
 * The four special cases are the ones that bite: -1 pushes, -2 appends a whole list
 * (that is how log lines arrive), a null value against an array index splices it out,
 * and a null value anywhere else deletes the key. Treating any of them as an ordinary
 * assignment produces a tree that looks right and drifts.
 */
export function applyUpdate(root, update) {
  let obj = root;
  let i = 0;
  while (i < update.length - 2) {
    const key = cleanKey(update[i++]);
    if (obj[key] === undefined) obj[key] = Number.isInteger(update[i]) ? [] : {};
    obj = obj[key];
  }
  const isArray = Array.isArray(obj);
  const key = cleanKey(update[i++]);
  const value = update[i];

  if (isArray && key === -1) obj.push(value);
  else if (isArray && key === -2) obj.splice(obj.length, 0, ...value);
  else if (isArray && value === null) obj.splice(key, 1);
  else if (value === null) delete obj[key];
  else obj[key] = value;
  return root;
}

/**
 * A live connection to the local client.
 *
 * Reconnects on its own, because the client restarts whenever its configuration
 * changes and a dashboard that needs reloading after every setting is a dashboard
 * people stop using. Backoff is capped low: this is a socket to loopback, so retrying
 * costs nothing and the reader is usually watching, waiting for it to come back.
 */
export class LocalClient {
  constructor() {
    this.state = {};
    this.status = 'connecting'; // connecting | connected | unreachable
    this.listeners = new Set();
    this.closed = false;
    this.attempts = 0;
    this.ws = null;
    this.timer = null;
  }

  onChange(fn) {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  emit() {
    for (const fn of this.listeners) {
      try {
        fn(this);
      } catch (e) {
        console.error('fah listener failed', e);
      }
    }
  }

  connect() {
    if (this.closed) return;
    let ws;
    try {
      ws = new WebSocket(WS_URL);
    } catch (e) {
      // Older browsers throw synchronously on a blocked scheme rather than firing
      // onerror, which would otherwise leave the page saying "connecting" forever.
      this.fail();
      return;
    }
    this.ws = ws;

    ws.onopen = () => {
      this.attempts = 0;
      this.status = 'connected';
      // A fresh socket sends the whole tree first, so the old one must go: keeping it
      // would leave a unit that has since finished sitting in the list forever.
      this.state = {};
      this.emit();
    };

    ws.onmessage = (ev) => {
      let msg;
      try {
        msg = JSON.parse(ev.data);
      } catch (e) {
        return;
      }
      if (Array.isArray(msg)) applyUpdate(this.state, msg);
      else this.state = cleanKeys(msg);
      this.status = 'connected';
      this.emit();
    };

    ws.onerror = () => {};
    ws.onclose = () => {
      if (this.closed) return;
      this.fail();
    };
  }

  fail() {
    this.status = 'unreachable';
    this.emit();
    this.attempts++;
    const wait = Math.min(1000 * 2 ** Math.min(this.attempts, 4), 15000);
    this.timer = setTimeout(() => this.connect(), wait);
  }

  send(msg) {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(msg));
      return true;
    }
    return false;
  }

  /** fold, pause or finish. Group-scoped, but this page drives them all together. */
  setState(state) {
    return this.send({ cmd: 'state', state });
  }

  close() {
    this.closed = true;
    clearTimeout(this.timer);
    if (this.ws) {
      this.ws.onclose = null;
      this.ws.close();
    }
    this.listeners.clear();
  }

  /* ------------------------------------------------------------ readers --- */

  get config() {
    return this.state.config || {};
  }

  get info() {
    return this.state.info || {};
  }

  get units() {
    return this.state.units || [];
  }

  /** The one group this client has; the API allows several but the client ships one. */
  get group() {
    const gs = this.state.groups || {};
    const first = Object.values(gs)[0];
    return first || {};
  }

  get groupConfig() {
    return this.group.config || {};
  }

  get paused() {
    return !!this.groupConfig.paused;
  }

  get finishing() {
    return !!this.groupConfig.finish;
  }

  /** Points per day across every running unit — what the machine is worth right now. */
  get ppd() {
    return this.units.reduce((n, u) => n + (u.ppd || 0), 0);
  }

  get gpus() {
    return Object.entries(this.info.gpus || {})
      .map(([id, g]) => ({ id, ...g }))
      .filter((g) => g.supported);
  }
}
