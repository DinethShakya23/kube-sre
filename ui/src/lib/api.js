// Talking to the kube-sre server. The page itself is static; every call here
// carries the API key, and the server decides what this key may see or do.

const KEYSTORE = 'kubesre.key';

let key = '';

function safe(fn) {
  try {
    return fn();
  } catch {
    return null;
  }
}

export function loadKey() {
  key = safe(() => sessionStorage.getItem(KEYSTORE) || localStorage.getItem(KEYSTORE) || '') || '';
  return key;
}

export function setKey(k, remember) {
  key = k || '';
  safe(() => {
    sessionStorage.removeItem(KEYSTORE);
    localStorage.removeItem(KEYSTORE);
  });
  if (key) safe(() => (remember ? localStorage : sessionStorage).setItem(KEYSTORE, key));
}

export function hasKey() {
  return key !== '';
}

// The CSRF header proves a cookie authenticated request came from this page.
export function headers(extra) {
  const h = { 'X-KubeSre-CSRF': '1', ...(extra || {}) };
  if (key) h.Authorization = 'Bearer ' + key;
  return h;
}

export async function publicConfig() {
  try {
    const r = await fetch('/v1/public/config');
    return r.ok ? await r.json() : {};
  } catch {
    return {};
  }
}

export function errText(t) {
  try {
    const j = JSON.parse(t);
    let m = j.detail || j.error || j.message;
    if (m && typeof m === 'object') m = m.message || JSON.stringify(m);
    return m || t;
  } catch {
    return t;
  }
}

export async function api(path, opts = {}) {
  const r = await fetch(path, { ...opts, headers: headers(opts.headers) });
  if (r.ok) return r;
  const t = await r.text();
  const err = new Error(errText(t) || r.statusText);
  err.status = r.status;
  throw err;
}

export const getJSON = async path => (await api(path)).json();
export const getText = async path => (await api(path)).text();

export function sendJSON(path, method, body) {
  return api(path, {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body)
  });
}

// Reads a Server Sent Events body and calls onData with each data: payload.
export async function readSSE(response, onData) {
  const reader = response.body.getReader();
  const dec = new TextDecoder();
  let buf = '';
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += dec.decode(value, { stream: true });
    const parts = buf.split('\n\n');
    buf = parts.pop();
    for (const p of parts) {
      for (const l of p.split('\n')) if (l.startsWith('data:')) onData(l.slice(5).trim());
    }
  }
}

export function newSessionId() {
  return globalThis.crypto?.randomUUID?.() ?? String(Date.now()) + Math.random().toString(16).slice(2);
}

export const WRITE_ROLES = ['operator', 'admin', 'superadmin'];
export const canWrite = role => WRITE_ROLES.includes(role);
export const isAdmin = role => role === 'admin' || role === 'superadmin';

// When things happened, in words a person would use.
export function when(t) {
  if (!t) return '';
  const d = new Date(typeof t === 'number' ? t * 1000 : t);
  if (isNaN(d.getTime())) return String(t);
  const s = Math.round((Date.now() - d.getTime()) / 1000);
  if (s < 0) return d.toLocaleString();
  if (s < 45) return 'just now';
  if (s < 90) return 'a minute ago';
  const m = Math.round(s / 60);
  if (m < 60) return m + ' minutes ago';
  const h = Math.round(m / 60);
  if (h < 24) return h === 1 ? 'an hour ago' : h + ' hours ago';
  return d.toLocaleString();
}

export function fullTime(t) {
  if (!t) return '';
  const d = new Date(typeof t === 'number' ? t * 1000 : t);
  return isNaN(d.getTime()) ? String(t) : d.toLocaleString();
}
