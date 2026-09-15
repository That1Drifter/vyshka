// Vyshka panel: the parts every view shares.
//
// The DOM is built with createElement and text nodes only. Manifest labels,
// event data, key/value payloads, audit details, and player names are all
// text some other program supplied, and none of it is ever treated as
// markup; the Content-Security-Policy the hub sends is the backstop.
//
// Nothing here knows a view. app.js holds the routing, the server and action
// views, the event feed, and the map; manage.js holds the management views;
// both import from here. The render lifecycle lives here too, so a view in
// either module guards its awaits against the same sequence number and
// registers its timers with the same teardown.

export const API = '/api/v1';
export const TOKEN_KEY = 'vyshka.adminToken';

// ---------------------------------------------------------------------------
// DOM helpers

export function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value === undefined || value === null || value === false) continue;
    if (key === 'class') {
      node.className = value;
    } else if (key.startsWith('on') && typeof value === 'function') {
      node.addEventListener(key.slice(2), value);
    } else if (value === true) {
      node.setAttribute(key, '');
    } else {
      node.setAttribute(key, String(value));
    }
  }
  append(node, children);
  return node;
}

export function append(node, children) {
  for (const child of children.flat(Infinity)) {
    if (child === null || child === undefined || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
}

export function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

export function badge(text, kind) {
  return el('span', { class: 'badge ' + (kind || '') }, text);
}

export function pretty(value) {
  return JSON.stringify(value, null, 2);
}

// randomKey mints an idempotency key. crypto.randomUUID exists only in
// secure contexts, and a hub reached over plain HTTP on a LAN is not one;
// getRandomValues is available everywhere.
export function randomKey() {
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID();
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}

export function formatTime(iso) {
  if (!iso) return '';
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return String(iso);
  return date.toLocaleString();
}

export function ago(iso) {
  if (!iso) return 'never';
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return String(iso);
  const seconds = Math.max(0, Math.round((Date.now() - date.getTime()) / 1000));
  if (seconds < 60) return seconds + ' s ago';
  if (seconds < 3600) return Math.round(seconds / 60) + ' min ago';
  if (seconds < 86400) return Math.round(seconds / 3600) + ' h ago';
  return date.toLocaleString();
}

// ---------------------------------------------------------------------------
// Rendering foreign JSON
//
// Event data, snapshot extras, and audit details are bounded in bytes by the
// hub, not in depth: a few thousand nested objects fit inside 16 KiB.
// JSON.stringify recurses, so it can overflow the stack on such a value, and
// indenting it multiplies its size by its depth. jsonDepth walks without
// recursion so a view can decide how to show a value before it tries;
// attempt keeps any failure to the one cell it belongs to.

// compactValue renders one value on a single line. A player identity
// (section 8.2) reads as platform:id; anything else nested is JSON. Every
// result is text.
export function compactValue(value) {
  if (value === null || value === undefined) return 'null';
  if (typeof value === 'string') return value;
  if (typeof value !== 'object') return String(value);
  if (!Array.isArray(value) && typeof value.platform === 'string' && typeof value.id === 'string'
      && Object.keys(value).length === 2) {
    return value.platform + ':' + value.id;
  }
  return JSON.stringify(value);
}

const SUMMARY_MAX = 240;

export function summarizeEventData(data) {
  if (!data || typeof data !== 'object' || Array.isArray(data)) return '';
  const parts = Object.entries(data).map(([key, value]) => key + ': ' + compactValue(value));
  const line = parts.join(', ');
  return line.length > SUMMARY_MAX ? line.slice(0, SUMMARY_MAX - 1) + '…' : line;
}

const PRETTY_MAX_DEPTH = 64;

export function jsonDepth(value, cap) {
  let deepest = 0;
  const stack = [{ value, depth: 1 }];
  while (stack.length > 0) {
    const { value: current, depth } = stack.pop();
    if (current === null || typeof current !== 'object') continue;
    if (depth > deepest) deepest = depth;
    if (deepest > cap) return deepest;
    for (const child of Array.isArray(current) ? current : Object.values(current)) {
      if (child !== null && typeof child === 'object') stack.push({ value: child, depth: depth + 1 });
    }
  }
  return deepest;
}

export function attempt(render, fallback) {
  try {
    return render();
  } catch {
    return fallback;
  }
}

export function eventPayloadText(data) {
  if (jsonDepth(data, PRETTY_MAX_DEPTH) > PRETTY_MAX_DEPTH) {
    return attempt(() => JSON.stringify(data), 'The data is nested too deeply to display.');
  }
  return attempt(() => pretty(data), 'The data could not be serialized.');
}

// disclosure is the one-line summary with the whole value behind it, the
// shape the event feed uses and the audit and key/value views reuse. The
// payload is serialized when the disclosure is first opened, not for every
// row on every draw.
export function disclosure(summary, value) {
  const pre = el('pre', {});
  const details = el('details', {}, el('summary', {}, summary), pre);
  details.addEventListener('toggle', () => {
    if (details.open && !pre.firstChild) pre.textContent = eventPayloadText(value);
  });
  return details;
}

// ---------------------------------------------------------------------------
// Admin API client

export class ApiError extends Error {
  constructor(status, code, message, details) {
    super(message);
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

export function token() {
  return sessionStorage.getItem(TOKEN_KEY) || '';
}

export async function api(method, path, body) {
  const headers = { Accept: 'application/json' };
  const bearer = token();
  if (bearer) headers.Authorization = 'Bearer ' + bearer;
  const init = { method, headers };
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  let response;
  try {
    response = await fetch(API + path, init);
  } catch (err) {
    throw new ApiError(0, 'unreachable', 'the hub could not be reached: ' + err.message);
  }
  const text = await response.text();
  let parsed = null;
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      parsed = null;
    }
  }
  if (!response.ok) {
    const detail = parsed && parsed.error ? parsed.error : {};
    throw new ApiError(response.status, detail.code || 'http_' + response.status,
      detail.message || ('the hub answered ' + response.status), detail.details);
  }
  return parsed;
}

// ---------------------------------------------------------------------------
// Render lifecycle
//
// One sequence number for the whole panel. A view takes the number its render
// began with and checks it after every await: a navigation, or a sign-out,
// has already replaced the view, and a late answer must not draw into
// someone else's page. One teardown hook likewise: whatever the outgoing view
// registered is run before the next one starts.

const lifecycle = { seq: 0, teardown: null };

// beginRender retires the current view and returns the sequence number the
// next one runs under.
export function beginRender() {
  if (lifecycle.teardown) {
    const teardown = lifecycle.teardown;
    lifecycle.teardown = null;
    teardown();
  }
  lifecycle.seq++;
  return lifecycle.seq;
}

// stale reports whether the view that began with seq has been replaced.
export function stale(seq) {
  return seq !== lifecycle.seq;
}

// setTeardown registers what to stop when this view goes away: timers, a map
// widget, an action being watched.
export function setTeardown(fn) {
  lifecycle.teardown = fn;
}

// ---------------------------------------------------------------------------
// The shell: session, breadcrumbs, nav

let signOutHandler = null;

// onSignOut registers what to draw once the token is gone. app.js owns the
// sign-in form, so it registers that; lib.js only forgets the token and
// retires the view.
export function onSignOut(handler) {
  signOutHandler = handler;
}

export function signOut(message) {
  sessionStorage.removeItem(TOKEN_KEY);
  // A held secret belongs to the session that minted it; signing out drops
  // it with the token, unshown if it was never dismissed.
  clearHeldSecrets();
  beginRender();
  renderSession();
  setCrumbs([]);
  if (signOutHandler) signOutHandler(message);
}

export function renderSession() {
  const session = document.getElementById('session');
  clear(session);
  const nav = document.getElementById('nav');
  const signedIn = Boolean(token());
  // The nav is dead weight on the sign-in page: every link needs a token.
  if (nav) nav.hidden = !signedIn;
  if (!signedIn) return;
  session.append(
    el('span', {}, 'signed in'),
    el('button', { class: 'small', type: 'button', id: 'sign-out', onclick: () => signOut() }, 'Sign out'),
  );
}

export function setCrumbs(items) {
  const crumbs = document.getElementById('crumbs');
  clear(crumbs);
  items.forEach((item, index) => {
    if (index > 0) crumbs.append(el('span', { class: 'sep' }, '/'));
    crumbs.append(item.href ? el('a', { href: item.href }, item.label) : el('span', {}, item.label));
  });
}

// markNav marks which of the header's sections the current route belongs to.
// The links themselves are in index.html, because they are the shell rather
// than any view's output.
export function markNav(section) {
  for (const link of document.querySelectorAll('#nav a[data-nav]')) {
    if (link.dataset.nav === section) {
      link.setAttribute('aria-current', 'page');
    } else {
      link.removeAttribute('aria-current');
    }
  }
}

// ---------------------------------------------------------------------------
// Routes
//
// The route lives in the hash as a path with an optional query, so that a
// filter survives a reload and can be handed to someone as a link.

export function playerQuery(player) {
  return player ? '?player=' + encodeURIComponent(player) : '';
}

export function serverHref(serverId, player = '') {
  return '#/servers/' + encodeURIComponent(serverId) + playerQuery(player);
}

export function actionHref(serverId, code, player = '') {
  return '#/servers/' + encodeURIComponent(serverId) + '/actions/' + encodeURIComponent(code) + playerQuery(player);
}

export function mapHref(serverId, world = '') {
  return serverHref(serverId) + '/map' + (world ? '?world=' + encodeURIComponent(world) : '');
}

export function eventsHref(serverId, types = [], follow = true) {
  const query = new URLSearchParams();
  for (const term of types) query.append('type', term);
  if (!follow) query.set('follow', 'off');
  const encoded = query.toString();
  return serverHref(serverId) + '/events' + (encoded ? '?' + encoded : '');
}

export function tokensHref() {
  return '#/tokens';
}

export function webhooksHref() {
  return '#/webhooks';
}

export function webhookHref(webhookId) {
  return '#/webhooks/' + encodeURIComponent(webhookId);
}

export function auditHref(filters = {}) {
  const query = new URLSearchParams();
  for (const key of ['tokenId', 'serverId', 'since', 'until']) {
    if (filters[key]) query.set(key, filters[key]);
  }
  const encoded = query.toString();
  return '#/audit' + (encoded ? '?' + encoded : '');
}

export function kvHref() {
  return '#/kv';
}

export function kvNamespaceHref(namespace, prefix = '') {
  return '#/kv/' + encodeURIComponent(namespace) + (prefix ? '?prefix=' + encodeURIComponent(prefix) : '');
}

let renderHandler = null;

// onRender registers the router. app.js owns it; lib.js needs it only so that
// go() can redraw a route that is already the current one.
export function onRender(handler) {
  renderHandler = handler;
}

// go navigates, and re-renders when the hash is already the one wanted (the
// browser fires no hashchange for that).
export function go(href) {
  if (location.hash === href) {
    if (renderHandler) renderHandler();
  } else {
    location.hash = href;
  }
}

// ---------------------------------------------------------------------------
// Shared view furniture

export function showError(app, err) {
  clear(app);
  app.append(el('div', { class: 'error', role: 'alert' },
    el('strong', {}, err.code ? err.code + ': ' : ''), err.message || String(err)));
}

// problemBox is a hidden error region a view keeps around and fills in when a
// call fails, so the controls that produced the failure stay on the page.
export function problemBox(id) {
  const node = el('div', { class: 'error', id, role: 'alert', hidden: true });
  return {
    node,
    show(err) {
      clear(node);
      node.append(el('strong', {}, err.code ? err.code + ': ' : ''), err.message || String(err));
      node.hidden = false;
    },
    hide() {
      node.hidden = true;
    },
  };
}

// forbidden draws the one notice a view gets when the hub refuses it for
// scope. One notice naming the scope, not one per call that failed: an
// operator whose token is narrowed needs to be told what to mint, once.
export function forbidden(app, scope, err, what) {
  clear(app);
  app.append(el('div', { class: 'notice', id: 'forbidden', role: 'alert' },
    el('strong', {}, 'This view needs the ' + scope + ' scope. '),
    'The token signed in here does not hold it, so the hub refused ' + what + ' with ',
    el('span', { class: 'mono' }, err.code || 'forbidden'),
    '. Sign in with a token that holds ' + scope + ', or mint one with the Admin API.'));
}

// isForbidden is the test every management view runs on its first load.
export function isForbidden(err) {
  return err instanceof ApiError && err.status === 403;
}

function selectContents(node) {
  const range = document.createRange();
  range.selectNodeContents(node);
  const selection = window.getSelection();
  if (!selection) return;
  selection.removeAllRanges();
  selection.addRange(range);
}

async function copyToClipboard(node, state) {
  state.hidden = false;
  try {
    if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
      await navigator.clipboard.writeText(node.textContent);
      state.textContent = 'Copied to the clipboard.';
      return;
    }
  } catch {
    // The API is there but the browser refused it. A hub reached over plain
    // http on a LAN is not a secure context, and that is the ordinary case
    // here, so the fallback below is a first-class path rather than a
    // consolation.
  }
  selectContents(node);
  state.textContent = 'The clipboard is not available on this page, so the value is selected: copy it with the keyboard.';
}

// secretOnce shows a value the hub will never show again: an enrollment
// token, an admin token's secret, a webhook's signing secret. The Copy button
// falls back to selecting the text where the clipboard API is not allowed.
export function secretOnce(options) {
  const id = options.id;
  const value = el('code', { class: 'secret-value', id: id + '-value' }, options.secret);
  const state = el('span', { class: 'muted copy-state', id: id + '-state', hidden: true }, '');
  const copy = el('button', {
    type: 'button', class: 'small', id: id + '-copy', 'data-copy': id,
    onclick: () => { copyToClipboard(value, state); },
  }, 'Copy');
  return el('div', { class: 'card secret', id, role: 'group', 'aria-label': options.label },
    el('h3', {}, options.label),
    el('p', { class: 'notice' }, options.note
      || 'Shown once. The hub keeps a digest, not the value, so nothing can show it again.'),
    el('div', { class: 'secret-row' }, value, copy),
    state,
    options.expiresAt ? el('p', { class: 'muted' }, 'Expires ' + formatTime(options.expiresAt) + '.') : null);
}

// ---------------------------------------------------------------------------
// Secrets held for a view that has gone
//
// A secret-bearing answer can land after its view has been replaced: the
// operator submits "Mint token" and clicks a nav link before the hub answers.
// The credential exists on the hub either way, and its secret is in that one
// answer and nowhere else, so discarding the answer with the view would lose
// the only copy of a live credential. A handler that finds itself stale hands
// the result here instead, and the next view to render shows it at the top,
// through the same one-time widget, until it is dismissed or the session ends.
//
// Module memory only: never sessionStorage, never localStorage, never the
// route. A secret that outlives the tab is a secret written down.

const heldSecrets = [];
let heldSerial = 0;

function heldEntryNode(entry) {
  const row = el('div', {
    class: 'held-secret', 'data-held-secret': entry.kind || 'secret', 'data-held-id': entry.heldId,
  });
  row.append(
    secretOnce(Object.assign({}, entry, { id: entry.heldId })),
    el('div', { class: 'actions-row' }, el('button', {
      type: 'button', class: 'small', 'data-dismiss-secret': entry.heldId,
      onclick: () => {
        const at = heldSecrets.findIndex((held) => held.heldId === entry.heldId);
        if (at !== -1) heldSecrets.splice(at, 1);
        row.remove();
        const tray = document.getElementById('held-secrets');
        if (heldSecrets.length === 0 && tray) tray.remove();
      },
    }, 'Dismiss')));
  return row;
}

function trayNode() {
  return el('section', { id: 'held-secrets', 'aria-label': 'Secrets the hub will not show again' },
    el('p', { class: 'notice' },
      'These answers arrived after the page that asked for them had gone. The hub keeps a digest, not the value, so this is the only copy: it is held in this tab’s memory, and goes when it is dismissed or when the session ends.'));
}

// holdSecret keeps one result the view that asked for it can no longer show.
// The options are secretOnce's (label, secret, note, expiresAt), plus kind,
// which is what the tray's data hook carries. It is put on the page at once
// when a view is already drawn, because the answer has landed and waiting for
// the next navigation to reveal it is one more chance to lose it.
export function holdSecret(options) {
  // A session that has ended is not handed a secret: sign-out drops what is
  // held, and an answer arriving after it must not reappear for whoever signs
  // in next in this tab.
  if (!token()) return;
  heldSerial++;
  const entry = Object.assign({}, options, { heldId: 'held-secret-' + heldSerial });
  heldSecrets.push(entry);
  const app = document.getElementById('app');
  if (!app) return;
  let tray = document.getElementById('held-secrets');
  if (!tray) {
    tray = trayNode();
    app.prepend(tray);
  }
  tray.append(heldEntryNode(entry));
}

export function clearHeldSecrets() {
  heldSecrets.length = 0;
}

// renderHeldSecrets puts every held secret at the top of the view that has
// just drawn. The router calls it after the view returns, so the view's own
// clear() cannot take the tray away again.
export function renderHeldSecrets(app) {
  if (!app || heldSecrets.length === 0) return;
  // A view that returned without clearing #app may still hold the tray
  // holdSecret mounted; there is one tray on a page, never two.
  const mounted = document.getElementById('held-secrets');
  if (mounted) mounted.remove();
  const tray = trayNode();
  for (const entry of heldSecrets) tray.append(heldEntryNode(entry));
  app.prepend(tray);
}

// confirmation is the explicit tick a destructive control needs, the same
// shape the action form's danger box uses.
export function confirmation(id, label) {
  const box = el('input', { type: 'checkbox', id });
  const node = el('label', { class: 'field inline destructive' }, box, el('span', { class: 'name' }, label));
  return { node, box, get checked() { return box.checked; } };
}

// linesOf reads a one-value-per-line textarea the way the action form's array
// field does: trimmed, blank lines dropped, order kept, duplicates kept.
export function linesOf(textarea) {
  return textarea.value.split('\n').map((line) => line.trim()).filter((line) => line !== '');
}

// statusKind classifies an HTTP status for a badge: what succeeded, what the
// hub refused, and what broke in the hub.
export function statusKind(status) {
  if (status >= 500) return 'failed';
  if (status >= 400) return 'warning';
  if (status >= 200 && status < 300) return 'completed';
  return '';
}
