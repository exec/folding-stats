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

/** The client's own default. An agent elsewhere is reached through a relay URL. */
export const LOCAL_URL = 'ws://127.0.0.1:7396/api/websocket';

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
 * The state a folding client reports, and the questions worth asking of it.
 *
 * Split from the connection deliberately: a machine reached through the relay holds
 * exactly the same tree, arrived at the same way, and the view must not be able to
 * tell the two apart. Everything here is a reader over `state` — the transport owns
 * how it gets filled.
 */
export class MachineState {
  constructor(label = '') {
    this.label = label;
    this.state = {};
    this.status = 'connecting'; // connecting | connected | unreachable
    this.listeners = new Set();
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
        console.error('machine listener failed', e);
      }
    }
  }

  /** One message from a client: a whole tree, or a patch against it. */
  accept(msg) {
    if (Array.isArray(msg)) applyUpdate(this.state, msg);
    else if (msg && typeof msg === 'object') this.state = cleanKeys(msg);
    else return false; // the client also sends bare "ping"
    return true;
  }

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
    return Object.values(gs)[0] || {};
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

  /** What to call this machine: its own name if it has told us one. */
  get name() {
    return this.info.mach_name || this.info.hostname || this.label;
  }

  get working() {
    return this.units.length > 0 && !this.paused;
  }

  /** fold, pause, finish, or idle — one word for the state a reader cares about. */
  get phase() {
    if (this.status !== 'connected') return 'offline';
    if (this.paused) return 'paused';
    if (this.finishing) return 'finishing';
    return this.units.length ? 'folding' : 'waiting';
  }

  /** fold, pause or finish. Group-scoped, but this page drives them all together. */
  setState(state) {
    return this.send({ cmd: 'state', state });
  }

  send() {
    return false;
  }
}

/**
 * A live connection to the local client.
 *
 * Reconnects on its own, because the client restarts whenever its configuration
 * changes and a dashboard that needs reloading after every setting is a dashboard
 * people stop using. Backoff is capped low: this is a socket to loopback, so retrying
 * costs nothing and the reader is usually watching, waiting for it to come back.
 */
export class LocalClient extends MachineState {
  constructor(url = LOCAL_URL, label = 'this machine') {
    super(label);
    this.url = url;
    this.closed = false;
    this.attempts = 0;
    this.ws = null;
    this.timer = null;
  }

  connect() {
    if (this.closed) return;
    let ws;
    try {
      ws = new WebSocket(this.url);
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
      if (!this.accept(msg)) return;
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

  close() {
    this.closed = true;
    clearTimeout(this.timer);
    if (this.ws) {
      this.ws.onclose = null;
      this.ws.close();
    }
    this.listeners.clear();
  }

}

/**
 * Every machine the reader can see, as one thing.
 *
 * There is exactly one today — the client on this computer — and the page is built
 * around a fleet anyway. That is not speculation: the moment a second machine exists
 * the page has to change shape, and retrofitting a collection into code written for a
 * singleton is how a dashboard ends up with "machine 2" bolted beside a layout that
 * assumed one. The cost of writing it this way now is a few lines.
 *
 * A machine is a connection. Whether that connection goes to loopback or through a
 * relay to a box in another country is the transport's business, not the view's.
 */
export class Fleet {
  constructor(local, link = null) {
    this.local = local;
    this.link = link;
    this.listeners = new Set();
    local.onChange(() => this.emit());
    if (link) link.onChange(() => this.emit());
  }

  /**
   * Every machine, this one first.
   *
   * Computed rather than stored: relay machines appear and vanish as agents connect,
   * and a list captured at construction would be a list of whoever happened to be
   * online when the page loaded.
   */
  get clients() {
    return this.link ? [this.local, ...this.link.machines.values()] : [this.local];
  }

  connect() {
    this.local.connect();
    if (this.link) this.link.connect();
  }

  close() {
    this.local.close();
    if (this.link) this.link.close();
    this.listeners.clear();
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
        console.error('fleet listener failed', e);
      }
    }
  }

  /** Machines that have actually answered. A dead one still counts, and says so. */
  get online() {
    return this.clients.filter((c) => c.status === 'connected');
  }

  get ppd() {
    return this.online.reduce((n, c) => n + c.ppd, 0);
  }

  get folding() {
    return this.online.filter((c) => c.working).length;
  }

  /** True while this is one machine — the layout the single case deserves. */
  get single() {
    return this.clients.length === 1;
  }

  /**
   * Somewhere to read the account from.
   *
   * Every machine on a fleet reports the same donor, so any connected one will do —
   * but a machine that has not answered yet reports nothing, and picking it would
   * leave the account block empty until it did.
   */
  get config() {
    const c = this.online[0];
    return c ? c.config : {};
  }
}

