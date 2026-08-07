// Machines that are not this computer.
//
// The browser can reach the folding client on the machine it is running on and
// nowhere else, so everything else arrives through a relay: an agent on each machine
// holds a connection out to it, and this holds one in. The relay checks signatures and
// forwards frames; it is not trusted with anything, and it is not asked for anything.
//
// The identity here is an ed25519 keypair that lives in this browser. It is what makes
// a fleet yours: machines are enrolled against it, and the relay will hand out nothing
// that was not. There is no account, no password and no server-side record of who you
// are — losing the key means the machines are stranded, which is why it can be
// exported and carried to another browser.

import { MachineState } from '/fah.js';

const RELAY_URL = 'wss://folding.exec.codes/relay/browser';

const AUTH_CONTEXT = 'folding-relay-auth\0';
const ENROL_CONTEXT = 'folding-relay-enrol\0';
const ROLE_OWNER = 'owner';

/* ------------------------------------------------------------- identity --- */

const DB = 'folding';
const STORE = 'identity';

function idb() {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(DB, 1);
    req.onupgradeneeded = () => req.result.createObjectStore(STORE);
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

function idbGet(db, key) {
  return new Promise((resolve, reject) => {
    const r = db.transaction(STORE, 'readonly').objectStore(STORE).get(key);
    r.onsuccess = () => resolve(r.result);
    r.onerror = () => reject(r.error);
  });
}

function idbPut(db, key, value) {
  return new Promise((resolve, reject) => {
    const r = db.transaction(STORE, 'readwrite').objectStore(STORE).put(value, key);
    r.onsuccess = () => resolve();
    r.onerror = () => reject(r.error);
  });
}

export function b64(bytes) {
  let s = '';
  for (const b of new Uint8Array(bytes)) s += String.fromCharCode(b);
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function bytes(str) {
  return new TextEncoder().encode(str);
}

/** Joins a domain separator, fields and a nonce exactly as the relay does. */
function joined(...parts) {
  return bytes(parts.join('\0'));
}

/**
 * The owner identity, created on first use.
 *
 * Stored extractable, which is a deliberate trade. A non-extractable key could not be
 * read by any script — including a compromised copy of this page — but it also could
 * not be carried to a phone, and a fleet you can only see from one browser is not much
 * of a fleet. Losing it strands every machine enrolled against it, so it can be
 * exported and imported.
 */
export async function identity() {
  const db = await idb();
  const saved = await idbGet(db, 'owner');
  if (saved) return await importKey(saved);

  const kp = await crypto.subtle.generateKey({ name: 'Ed25519' }, true, ['sign', 'verify']);
  const jwk = await crypto.subtle.exportKey('jwk', kp.privateKey);
  await idbPut(db, 'owner', jwk);
  return await importKey(jwk);
}

async function importKey(jwk) {
  const priv = await crypto.subtle.importKey('jwk', jwk, { name: 'Ed25519' }, true, ['sign']);
  // The public half is the x coordinate, which is already in the JWK — no need to
  // derive it, and no need to keep a second key handle around.
  const pub = jwk.x;
  return {
    key: pub,
    jwk,
    async sign(message) {
      return b64(await crypto.subtle.sign({ name: 'Ed25519' }, priv, message));
    },
  };
}

/** Replaces the stored identity, which is how a fleet moves to another browser. */
export async function adoptIdentity(jwk) {
  const test = await crypto.subtle.importKey('jwk', jwk, { name: 'Ed25519' }, true, ['sign']);
  await crypto.subtle.sign({ name: 'Ed25519' }, test, bytes('check'));
  const db = await idb();
  await idbPut(db, 'owner', jwk);
  return await importKey(jwk);
}

/**
 * Mints an enrolment token authorising exactly one machine.
 *
 * Signed here rather than issued by the relay: the relay stores no tokens and can mint
 * none, so the only thing that can add a machine to this fleet is this key. Short
 * lived because on a rented box it travels in an environment variable, and those end
 * up in logs.
 */
export async function mintToken(id, lifeSeconds = 900) {
  const nonce = b64(crypto.getRandomValues(new Uint8Array(12)));
  const exp = Math.floor(Date.now() / 1000) + lifeSeconds;
  const sig = await id.sign(joined(ENROL_CONTEXT + id.key, String(exp), nonce));
  return { owner: id.key, exp, nonce, sig };
}

/* ---------------------------------------------------------------- link --- */

/**
 * One connection to the relay, and the machines behind it.
 *
 * Each machine is a MachineState like any other, so the fleet view cannot tell a box
 * in another country from the client on this desk — which is the point of having put
 * the readers on a base class.
 */
export class RelayLink {
  constructor(url = RELAY_URL) {
    this.url = url;
    this.machines = new Map(); // key -> RelayMachine
    this.listeners = new Set();
    this.status = 'connecting';
    this.closed = false;
    this.attempts = 0;
    this.ws = null;
    this.timer = null;
    this.id = null;
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
        console.error('relay listener failed', e);
      }
    }
  }

  async connect() {
    if (this.closed) return;
    if (!this.id) this.id = await identity();

    let ws;
    try {
      ws = new WebSocket(this.url);
    } catch (e) {
      this.fail();
      return;
    }
    this.ws = ws;

    ws.onmessage = async (ev) => {
      let f;
      try {
        f = JSON.parse(ev.data);
      } catch (e) {
        return;
      }
      await this.handle(f);
    };
    ws.onerror = () => {};
    ws.onclose = () => {
      if (this.closed) return;
      // Every machine goes with the connection: reporting a box as online because we
      // once saw it is worse than admitting we cannot currently tell.
      for (const m of this.machines.values()) {
        m.status = 'unreachable';
      }
      this.fail();
    };
  }

  async handle(f) {
    switch (f.type) {
      case 'hello': {
        const sig = await this.id.sign(joined(AUTH_CONTEXT + ROLE_OWNER, f.nonce));
        this.send({ type: 'auth', role: ROLE_OWNER, pubkey: this.id.key, sig });
        break;
      }

      case 'ready':
        this.attempts = 0;
        this.status = 'connected';
        this.emit();
        break;

      case 'machines': {
        const seen = new Set();
        for (const m of f.machines || []) {
          seen.add(m.key);
          let machine = this.machines.get(m.key);
          if (!machine) {
            machine = new RelayMachine(this, m.key, m.name);
            this.machines.set(m.key, machine);
          }
          machine.label = m.name || machine.label;
          machine.lastSeen = m.last_seen;
          if (m.online) {
            // A machine that has just come online has already sent its state to
            // nobody, so ask for it. The client only volunteers a full tree when
            // something connects to it, and that happened before we were listening.
            if (machine.status !== 'connected') {
              machine.status = 'connecting';
              this.send({ type: 'resync', machine: m.key });
            }
          } else {
            machine.status = 'unreachable';
          }
        }
        for (const [key, machine] of this.machines) {
          if (!seen.has(key)) {
            machine.status = 'unreachable';
            this.machines.delete(key);
          }
        }
        this.emit();
        break;
      }

      case 'from': {
        const machine = this.machines.get(f.machine);
        if (!machine || !machine.accept(f.data)) return;
        machine.status = 'connected';
        machine.emit();
        this.emit();
        break;
      }

      case 'error':
        console.warn('relay:', f.error, f.machine || '');
        break;
    }
  }

  send(obj) {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(obj));
      return true;
    }
    return false;
  }

  fail() {
    this.status = 'unreachable';
    this.emit();
    this.attempts++;
    const wait = Math.min(1000 * 2 ** Math.min(this.attempts, 5), 30000);
    this.timer = setTimeout(() => this.connect(), wait);
  }

  /** Revokes a machine. The agent is disconnected and cannot come back. */
  forget(key) {
    this.send({ type: 'forget', machine: key });
    this.machines.delete(key);
    this.emit();
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

/** One remote machine, indistinguishable from a local one to anything that reads it. */
class RelayMachine extends MachineState {
  constructor(link, key, name) {
    super(name || 'remote machine');
    this.link = link;
    this.key = key;
    this.status = 'connecting';
  }

  send(msg) {
    return this.link.send({ type: 'to', machine: this.key, data: msg });
  }

  get name() {
    return this.info.mach_name || this.info.hostname || this.label;
  }
}
