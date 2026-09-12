// Vyshka panel: a thin client over the Admin API (/api/v1).
//
// Nothing here knows a game. Servers come from GET /servers, actions from the
// manifest each plugin published, and every action form is generated from the
// params schema in that manifest (protocol section 6.1's JSON Schema subset,
// plus the x-vyshka-widget hint). Dispatching is POST /servers/{id}/actions
// and watching the result is GET /actions/{id}, the same calls curl makes.
// The event feed is GET /servers/{id}/events (protocol section 8.5), paged
// with the hub's own cursor. The live map is GET /servers/{id}/state/players
// (section 8.3) drawn over a basemap the hub serves from its maps directory,
// with the map widget itself in map.js.
//
// The DOM is built with createElement and text nodes only. Manifest labels,
// result payloads, event data, and player names are plugin-supplied text and
// are never treated as markup; the Content-Security-Policy the hub sends is
// the backstop.

import { createMap, validateManifest, worldPoint } from './map.js';

const API = '/api/v1';
const TOKEN_KEY = 'vyshka.adminToken';
const SERVER_LIST_REFRESH_MS = 5000;
const ACTION_POLL_MS = 1000;
const TERMINAL_STATES = new Set(['completed', 'failed', 'expired']);
// The feed follows new events on the server-list cadence and asks for the
// hub's default page (section 8.5: reference default 100, cap 500).
const EVENT_FEED_REFRESH_MS = SERVER_LIST_REFRESH_MS;
const EVENT_PAGE_SIZE = 100;
// The map re-reads the latest snapshot on the server-list cadence too. The
// snapshot's own age is what says how live the picture is: a plugin on a
// held long-poll publishes less often than the panel asks.
const MAP_REFRESH_MS = SERVER_LIST_REFRESH_MS;
// Map tilesets are served by the panel's own handler beside the page, so
// the path is relative to it rather than to the Admin API.
const MAPS_PATH = 'maps/';
// The core event types of section 8.1, offered as filter suggestions. The
// feed itself accepts whatever the hub's grammar accepts.
const CORE_EVENT_TYPES = [
  'core.player.connect', 'core.player.disconnect', 'core.player.death', 'core.player.damage',
  'core.player.chat', 'core.player.kick', 'core.player.ban', 'core.vehicle.spawn',
  'core.vehicle.destroy', 'core.object.placed', 'core.item.interact', 'core.server.start',
  'core.server.fps', 'core.server.stop',
];

// ---------------------------------------------------------------------------
// DOM helpers

function el(tag, attrs = {}, ...children) {
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

function append(node, children) {
  for (const child of children.flat(Infinity)) {
    if (child === null || child === undefined || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
}

function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

function badge(text, kind) {
  return el('span', { class: 'badge ' + (kind || '') }, text);
}

function pretty(value) {
  return JSON.stringify(value, null, 2);
}

// randomKey mints an idempotency key. crypto.randomUUID exists only in
// secure contexts, and a hub reached over plain HTTP on a LAN is not one;
// getRandomValues is available everywhere.
function randomKey() {
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID();
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}

function formatTime(iso) {
  if (!iso) return '';
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return String(iso);
  return date.toLocaleString();
}

function ago(iso) {
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
// Admin API client

class ApiError extends Error {
  constructor(status, code, message, details) {
    super(message);
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

function token() {
  return sessionStorage.getItem(TOKEN_KEY) || '';
}

async function api(method, path, body) {
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
// Routing and view lifecycle

let renderSeq = 0;
let teardown = null;

// The route lives in the hash as a path with an optional query, so that a
// feed's filter survives a reload and can be handed to someone as a link:
// #/servers/{id}/events?type=core.player.*&follow=off.
function parseRoute() {
  const hash = location.hash.replace(/^#/, '');
  const mark = hash.indexOf('?');
  const path = mark === -1 ? hash : hash.slice(0, mark);
  const query = new URLSearchParams(mark === -1 ? '' : hash.slice(mark + 1));
  const parts = path.split('/').filter(Boolean).map(decodeURIComponent);
  // A player chosen on the map travels to the action list and on into an
  // action form as ?player=, the platform id that is a player target's
  // referenceKey. It is a preselection, never a filter: the list is whole.
  const player = query.get('player') || '';
  if (parts[0] === 'servers' && parts.length === 2) {
    return { view: 'server', serverId: parts[1], player };
  }
  if (parts[0] === 'servers' && parts.length === 3 && parts[2] === 'map') {
    // ?world= overrides the world the server reported, for a server whose
    // start event the token cannot read, or to look at another map.
    return { view: 'map', serverId: parts[1], world: query.get('world') || '' };
  }
  if (parts[0] === 'servers' && parts.length === 3 && parts[2] === 'events') {
    // Type terms travel to the hub as they are. An empty term in a link is
    // the hub's to refuse (section 8.5: a filter it could not parse is a
    // bad request, never silently a wider feed); the form never makes one.
    return {
      view: 'events', serverId: parts[1],
      types: query.getAll('type'),
      follow: query.get('follow') !== 'off',
    };
  }
  if (parts[0] === 'servers' && parts.length === 4 && parts[2] === 'actions') {
    return { view: 'action', serverId: parts[1], code: parts[3], player };
  }
  return { view: 'servers' };
}

function playerQuery(player) {
  return player ? '?player=' + encodeURIComponent(player) : '';
}

function serverHref(serverId, player = '') {
  return '#/servers/' + encodeURIComponent(serverId) + playerQuery(player);
}

function actionHref(serverId, code, player = '') {
  return '#/servers/' + encodeURIComponent(serverId) + '/actions/' + encodeURIComponent(code) + playerQuery(player);
}

function mapHref(serverId, world = '') {
  return serverHref(serverId) + '/map' + (world ? '?world=' + encodeURIComponent(world) : '');
}

function eventsHref(serverId, types = [], follow = true) {
  const query = new URLSearchParams();
  for (const term of types) query.append('type', term);
  if (!follow) query.set('follow', 'off');
  const encoded = query.toString();
  return serverHref(serverId) + '/events' + (encoded ? '?' + encoded : '');
}

function setCrumbs(items) {
  const crumbs = document.getElementById('crumbs');
  clear(crumbs);
  items.forEach((item, index) => {
    if (index > 0) crumbs.append(el('span', { class: 'sep' }, '/'));
    crumbs.append(item.href ? el('a', { href: item.href }, item.label) : el('span', {}, item.label));
  });
}

function renderSession() {
  const session = document.getElementById('session');
  clear(session);
  if (!token()) return;
  session.append(
    el('span', {}, 'signed in'),
    el('button', { class: 'small', type: 'button', id: 'sign-out', onclick: () => signOut() }, 'Sign out'),
  );
}

function signOut(message) {
  sessionStorage.removeItem(TOKEN_KEY);
  if (teardown) {
    teardown();
    teardown = null;
  }
  renderSeq++;
  renderSession();
  setCrumbs([]);
  renderLogin(document.getElementById('app'), message);
}

function showError(app, err) {
  clear(app);
  app.append(el('div', { class: 'error', role: 'alert' },
    el('strong', {}, err.code ? err.code + ': ' : ''), err.message || String(err)));
}

async function render() {
  if (teardown) {
    teardown();
    teardown = null;
  }
  const seq = ++renderSeq;
  const app = document.getElementById('app');
  renderSession();
  if (!token()) {
    setCrumbs([]);
    renderLogin(app);
    return;
  }
  const route = parseRoute();
  try {
    if (route.view === 'server') {
      await viewServer(app, route, seq);
    } else if (route.view === 'action') {
      await viewAction(app, route, seq);
    } else if (route.view === 'events') {
      await viewEvents(app, route, seq);
    } else if (route.view === 'map') {
      await viewMap(app, route, seq);
    } else {
      await viewServers(app, seq);
    }
  } catch (err) {
    if (seq !== renderSeq) return;
    if (err instanceof ApiError && err.status === 401) {
      signOut('The hub rejected this token. Sign in again with a live one.');
      return;
    }
    showError(app, err);
  }
}

// ---------------------------------------------------------------------------
// Sign in

function renderLogin(app, message) {
  clear(app);
  const input = el('input', {
    type: 'password', id: 'token', name: 'token', autocomplete: 'off',
    required: true, placeholder: 'vya_...', spellcheck: 'false',
  });
  const error = el('div', { class: 'error', role: 'alert', id: 'login-error', hidden: !message }, message || '');
  const button = el('button', { type: 'submit', class: 'primary', id: 'sign-in' }, 'Sign in');
  const form = el('form', {
    class: 'stack card login', id: 'login',
    onsubmit: async (event) => {
      event.preventDefault();
      const value = input.value.trim();
      if (!value) return;
      button.disabled = true;
      sessionStorage.setItem(TOKEN_KEY, value);
      try {
        // Any authenticated answer will do, including a 403 from a token
        // scoped away from the server list; only a 401 means the token
        // itself is no good.
        await api('GET', '/servers');
      } catch (err) {
        if (err instanceof ApiError && err.status === 401) {
          sessionStorage.removeItem(TOKEN_KEY);
          error.textContent = 'The hub rejected this token (' + err.code + ').';
          error.hidden = false;
          button.disabled = false;
          return;
        }
        if (err instanceof ApiError && err.status === 0) {
          sessionStorage.removeItem(TOKEN_KEY);
          error.textContent = err.message;
          error.hidden = false;
          button.disabled = false;
          return;
        }
      }
      render();
    },
  },
  el('h1', {}, 'Sign in'),
  el('p', { class: 'muted' },
    'Paste an Admin API token. It stays in this browser tab and travels to the hub as a bearer token, exactly as it would from curl.'),
  el('label', { class: 'field' }, el('span', { class: 'name' }, 'Admin token'), input),
  error,
  el('div', { class: 'actions-row' }, button));
  app.append(form);
  input.focus();
}

// ---------------------------------------------------------------------------
// Servers

async function viewServers(app, seq) {
  setCrumbs([{ label: 'Servers' }]);
  const load = async () => {
    const data = await api('GET', '/servers');
    if (seq !== renderSeq) return;
    drawServers(app, data.servers || []);
  };
  await load();
  // A navigation during the first load has already replaced this view; a
  // timer armed now would outlive it.
  if (seq !== renderSeq) return;
  const timer = setInterval(() => {
    load().catch((err) => {
      // A refresh that began under a token since replaced must not sign
      // out whoever signed in after it: clearing the interval does not
      // recall a request already in flight.
      if (seq !== renderSeq) return;
      if (err instanceof ApiError && err.status === 401) signOut('The hub rejected this token.');
    });
  }, SERVER_LIST_REFRESH_MS);
  teardown = () => clearInterval(timer);
}

function drawServers(app, servers) {
  clear(app);
  app.append(el('h1', {}, 'Servers'));
  if (servers.length === 0) {
    app.append(el('p', { class: 'notice' },
      'No servers yet. Create one with POST /api/v1/servers (scripts/demo-enrollment.sh walks through it), enroll a plugin, and it will appear here.'));
    return;
  }
  const rows = servers.map((server) => el('tr', {
    class: 'row-link', 'data-server-id': server.id,
    onclick: () => { location.hash = serverHref(server.id); },
  },
  el('td', {}, el('a', { href: serverHref(server.id) }, server.name)),
  el('td', {}, server.game || el('span', { class: 'muted' }, 'any')),
  el('td', {}, badge(server.linkState || 'unknown', server.linkState)),
  el('td', {}, badge(server.credentialState || '', server.credentialState)),
  el('td', {}, server.plugin ? server.plugin.name + ' ' + (server.plugin.version || '') : el('span', { class: 'muted' }, 'none')),
  el('td', {}, ago(server.lastSeenAt)),
  el('td', {}, server.pendingEnvelopeCount > 0
    ? badge(String(server.pendingEnvelopeCount), 'pending')
    : el('span', { class: 'muted' }, '0'))));
  app.append(el('table', { id: 'servers' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Name'), el('th', {}, 'Game'), el('th', {}, 'Link'), el('th', {}, 'Credentials'),
      el('th', {}, 'Plugin'), el('th', {}, 'Last seen'), el('th', {}, 'Queued'))),
    el('tbody', {}, rows)));
  app.append(el('p', { class: 'muted' }, 'Refreshes every ' + (SERVER_LIST_REFRESH_MS / 1000) + ' s.'));
}

async function loadServerAndManifest(serverId) {
  const [server, manifest] = await Promise.all([
    api('GET', '/servers/' + encodeURIComponent(serverId)),
    api('GET', '/servers/' + encodeURIComponent(serverId) + '/manifest').catch((err) => {
      if (err instanceof ApiError && err.status === 404) return null;
      throw err;
    }),
  ]);
  return { server, manifest };
}

function serverSummary(server) {
  const session = server.session;
  return el('div', { class: 'card' },
    el('h1', {}, server.name, ' ', badge(server.linkState || 'unknown', server.linkState)),
    el('dl', { class: 'kv' },
      el('dt', {}, 'Game'), el('dd', {}, server.game || 'any'),
      el('dt', {}, 'Plugin'), el('dd', {}, server.plugin ? server.plugin.name + ' ' + (server.plugin.version || '') : 'none enrolled'),
      el('dt', {}, 'Credentials'), el('dd', {}, badge(server.credentialState || '', server.credentialState)),
      el('dt', {}, 'Last seen'), el('dd', {}, ago(server.lastSeenAt)),
      el('dt', {}, 'Session'), el('dd', {}, session
        ? 'live, polls held up to ' + session.pollTimeoutSeconds + ' s'
        : 'none'),
      el('dt', {}, 'Queued for delivery'), el('dd', {}, String(server.pendingEnvelopeCount || 0)),
      el('dt', {}, 'Server id'), el('dd', { class: 'mono' }, server.id)));
}

async function viewServer(app, route, seq) {
  const { server, manifest } = await loadServerAndManifest(route.serverId);
  if (seq !== renderSeq) return;
  // A preselected player is labelled from the latest snapshot when it is
  // there; the id alone is still a valid target when it is not (the player
  // may have left since the map was drawn).
  let target = null;
  if (route.player) {
    const players = await loadPlayers(server.id);
    if (seq !== renderSeq) return;
    target = players.find((entry) => entry.id === route.player) || { id: route.player, platform: '', name: '' };
  }
  setCrumbs([{ label: 'Servers', href: '#/' }, { label: server.name }]);
  clear(app);
  app.append(serverSummary(server));
  app.append(el('p', { class: 'view-links' },
    el('a', { class: 'button', href: mapHref(server.id), id: 'map-link' }, 'Live map'),
    ' ',
    el('a', { class: 'button', href: eventsHref(server.id), id: 'events-link' }, 'Event feed')));
  if (target) {
    app.append(el('div', { class: 'notice target', id: 'target-player' },
      el('strong', {}, 'Target player: '),
      target.name ? target.name + ' ' : '',
      el('span', { class: 'mono' }, (target.platform ? target.platform + ':' : '') + target.id),
      '. Player actions below open with this target filled in. ',
      el('a', { href: serverHref(server.id), id: 'target-clear' }, 'Clear')));
  }

  if (!manifest) {
    app.append(el('p', { class: 'notice', id: 'no-manifest' },
      'This server has not published a manifest yet. Actions appear here once its plugin connects and publishes one.'));
    return;
  }
  const body = manifest.manifest || {};
  const actions = Array.isArray(body.actions) ? body.actions : [];
  app.append(el('h2', {}, 'Actions'),
    el('p', { class: 'muted' },
      'Manifest revision ' + manifest.revision + ', published ' + formatTime(manifest.publishedAt),
      body.plugin && body.plugin.name ? ' by ' + body.plugin.name + ' ' + (body.plugin.version || '') : ''));
  if (actions.length === 0) {
    app.append(el('p', { class: 'notice' }, 'The manifest declares no actions.'));
    return;
  }
  const byNamespace = new Map();
  for (const action of actions) {
    const namespace = action.namespace || '';
    if (!byNamespace.has(namespace)) byNamespace.set(namespace, []);
    byNamespace.get(namespace).push(action);
  }
  for (const [namespace, group] of byNamespace) {
    if (byNamespace.size > 1 || namespace) app.append(el('h3', {}, namespace || 'no namespace'));
    app.append(el('div', { class: 'action-list' }, group.map((action) => el('a', {
      class: 'action-item',
      href: actionHref(server.id, action.code, action.context === 'player' ? route.player : ''),
      'data-action-code': action.code,
    },
    el('span', { class: 'name' }, action.name || action.code),
    el('span', { class: 'code mono' }, action.code),
    el('span', { class: 'spacer' }),
    action.context ? badge(action.context) : null,
    action.danger && action.danger !== 'none' ? badge(action.danger, action.danger) : null))));
  }
}

// ---------------------------------------------------------------------------
// Event feed
//
// One server's events, newest first, straight from GET /servers/{id}/events
// with the hub's own ordering and cursor. The page holds a set of events
// keyed by id in the hub's order (occurredAt descending, then id descending,
// the total order section 8.5 requires) and merges every answer into it:
// the first page on load, older pages behind the hub's cursor on request,
// and, in follow mode, the first page again every EVENT_FEED_REFRESH_MS.
//
// Following re-reads the first page rather than asking for events since the
// newest one seen, because the feed is ordered by occurredAt, which is the
// game server's clock: a batch flushed late, or an event the hub stamped with
// its own receipt time, can land below events already on the page, and a
// since= query keyed on the newest occurredAt would never see it. Merging by
// id in the hub's order places it where the hub would. What follow cannot
// see is a late event that lands below the newest EVENT_PAGE_SIZE; a reload
// or an older page picks it up.

function compareEvents(a, b) {
  const at = Date.parse(a.occurredAt);
  const bt = Date.parse(b.occurredAt);
  if (at !== bt) return at > bt ? -1 : 1;
  if (a.id === b.id) return 0;
  return a.id > b.id ? -1 : 1;
}

// mergeEvents adds the events the page has not seen to feed.order, keeping
// it sorted, and returns how many were new.
function mergeEvents(feed, incoming) {
  let added = 0;
  for (const event of incoming) {
    if (!event || typeof event.id !== 'string' || feed.byId.has(event.id)) continue;
    feed.byId.set(event.id, event);
    let index = feed.order.findIndex((known) => compareEvents(event, known) < 0);
    if (index === -1) index = feed.order.length;
    feed.order.splice(index, 0, event);
    added++;
  }
  return added;
}

// compactValue renders one value of an event's data on a single line. A
// player identity (section 8.2) reads as platform:id; anything else nested
// is JSON. Every result is text.
function compactValue(value) {
  if (value === null || value === undefined) return 'null';
  if (typeof value === 'string') return value;
  if (typeof value !== 'object') return String(value);
  if (!Array.isArray(value) && typeof value.platform === 'string' && typeof value.id === 'string'
      && Object.keys(value).length === 2) {
    return value.platform + ':' + value.id;
  }
  return JSON.stringify(value);
}

const EVENT_SUMMARY_MAX = 240;

function summarizeEventData(data) {
  if (!data || typeof data !== 'object' || Array.isArray(data)) return '';
  const parts = Object.entries(data).map(([key, value]) => key + ': ' + compactValue(value));
  const line = parts.join(', ');
  return line.length > EVENT_SUMMARY_MAX ? line.slice(0, EVENT_SUMMARY_MAX - 1) + '…' : line;
}

// Event data is bounded in bytes by the hub (16 KiB), not in depth: a few
// thousand nested objects fit in that. JSON.stringify recurses, so it can
// overflow the stack on such a value, and indenting it multiplies its size
// by its depth. jsonDepth walks without recursion so the feed can decide
// how to show a value before it tries; attempt keeps any failure to the
// one cell it belongs to.
const EVENT_PRETTY_MAX_DEPTH = 64;

function jsonDepth(value, cap) {
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

function attempt(render, fallback) {
  try {
    return render();
  } catch {
    return fallback;
  }
}

function eventPayloadText(data) {
  if (jsonDepth(data, EVENT_PRETTY_MAX_DEPTH) > EVENT_PRETTY_MAX_DEPTH) {
    return attempt(() => JSON.stringify(data), 'The data is nested too deeply to display.');
  }
  return attempt(() => pretty(data), 'The data could not be serialized.');
}

// declaredEventNames maps the custom event types a manifest declares
// (section 6.3) to their display names; the feed labels a matching event
// with its name beside the type. Declaration is advisory, so an undeclared
// type simply has no label.
function declaredEventNames(manifest) {
  const names = new Map();
  const body = manifest && manifest.manifest ? manifest.manifest : {};
  for (const declared of Array.isArray(body.events) ? body.events : []) {
    if (declared && typeof declared.id === 'string' && typeof declared.name === 'string' && declared.name) {
      names.set(declared.id, declared.name);
    }
  }
  return names;
}

function eventTypeSuggestions(names) {
  const namespaces = new Set(['core']);
  const suggestions = new Set(['*']);
  for (const type of [...CORE_EVENT_TYPES, ...names.keys()]) {
    suggestions.add(type);
    const dot = type.indexOf('.');
    if (dot > 0) namespaces.add(type.slice(0, dot));
  }
  for (const namespace of namespaces) suggestions.add(namespace + '.*');
  return [...suggestions];
}

function eventRow(event, names) {
  const data = event.data && typeof event.data === 'object' && !Array.isArray(event.data) ? event.data : {};
  const type = typeof event.type === 'string' ? event.type : '';
  const summary = attempt(() => summarizeEventData(data), 'The data is nested too deeply to summarize.');
  const hasData = Object.keys(data).length > 0;
  const custom = !type.startsWith('core.');
  const name = names.get(type);
  // The full payload is serialized when the disclosure is first opened,
  // not for every row on every draw.
  const pre = el('pre', {});
  const details = el('details', {}, el('summary', {}, summary), pre);
  details.addEventListener('toggle', () => {
    if (details.open && !pre.firstChild) pre.textContent = eventPayloadText(data);
  });
  return el('tr', { 'data-event-id': event.id, 'data-event-type': type },
    el('td', { class: 'when', title: 'received ' + formatTime(event.receivedAt) }, formatTime(event.occurredAt)),
    el('td', {},
      el('span', { class: 'mono type' }, type), ' ',
      badge(custom ? 'custom' : 'core', custom ? 'custom' : 'core'),
      name ? el('span', { class: 'muted' }, ' ', name) : null),
    el('td', { class: 'data' }, hasData ? details : el('span', { class: 'muted' }, 'no data')));
}

// A row that cannot be built for any reason is one row's problem, not the
// feed's: the event stays in the list with its id and type and nothing else.
function eventRowOrFallback(event, names) {
  try {
    return eventRow(event, names);
  } catch {
    return el('tr', { 'data-event-id': event.id, 'data-event-type': String(event.type) },
      el('td', { class: 'when' }, formatTime(event.occurredAt)),
      el('td', {}, el('span', { class: 'mono type' }, String(event.type))),
      el('td', { class: 'data muted' }, 'This event could not be rendered.'));
  }
}

async function viewEvents(app, route, seq) {
  const { server, manifest } = await loadServerAndManifest(route.serverId);
  if (seq !== renderSeq) return;
  setCrumbs([{ label: 'Servers', href: '#/' }, { label: server.name, href: serverHref(server.id) }, { label: 'Events' }]);
  clear(app);

  const names = declaredEventNames(manifest);
  // Two cursors, kept apart because they mean different things. nextCursor
  // is the walk's own: where the next older page starts, from the first
  // page or the last "Load older", null once a walk has reached the end.
  // gapCursor is set by a follow tick that finds the hub has more below
  // its first page than the feed has walked (see the tick), and it is
  // where the next walk starts once the current one is done. Holding it
  // separately is what keeps a walk that ends while a follow tick's
  // discovery is pending from discarding that discovery. topHadCursor
  // remembers whether the last first-page read carried a cursor at all,
  // so a hub crossing the page boundary is noticed even when the page
  // itself has not changed.
  // gapSerial counts gap discoveries, so a walk that started from a gap
  // retires only the discovery it started from: a gap re-recorded with
  // the same cursor text (the boundary event has not moved) while the
  // walk was in flight is a new discovery, not the one just walked.
  const feed = { byId: new Map(), order: [], nextCursor: null, gapCursor: null, gapSerial: 0, topHadCursor: false };
  const follow = route.follow;
  const types = route.types;

  // The filter and the follow switch are both part of the route: applying
  // either navigates, and the view starts over from the hub's first page.
  const go = (nextTypes, nextFollow) => {
    const next = eventsHref(server.id, nextTypes, nextFollow);
    if (location.hash === next) {
      render();
    } else {
      location.hash = next;
    }
  };
  const list = el('datalist', { id: 'event-types' }, eventTypeSuggestions(names).map((type) => el('option', { value: type })));
  const filterInput = el('input', {
    type: 'text', id: 'event-filter', name: 'type', list: 'event-types', value: types.join(' '),
    placeholder: 'core.player.* example-mod.raid.started', spellcheck: 'false', autocomplete: 'off',
  });
  const followBox = el('input', { type: 'checkbox', id: 'event-follow' });
  followBox.checked = follow;
  followBox.addEventListener('change', () => { go(types, followBox.checked); });
  const filterForm = el('form', {
    class: 'card feed-controls', id: 'event-filter-form',
    onsubmit: (event) => {
      event.preventDefault();
      const terms = [...new Set(filterInput.value.split(/[\s,]+/).map((term) => term.trim()).filter(Boolean))];
      go(terms, followBox.checked);
    },
  },
  el('label', { class: 'field' },
    el('span', { class: 'name' }, 'Type filter'),
    el('span', { class: 'filter-row' }, filterInput, list, el('button', { type: 'submit', class: 'small', id: 'event-filter-apply' }, 'Apply')),
    el('span', { class: 'hint' }, 'Exact types or {namespace}.* patterns, separated by spaces; empty shows every type the token may read.')),
  el('label', { class: 'field inline' }, followBox,
    el('span', { class: 'name' }, 'Follow: check for new events every ' + (EVENT_FEED_REFRESH_MS / 1000) + ' s')));

  const status = el('p', { class: 'muted', id: 'event-status' }, 'Loading…');
  const problem = el('div', { class: 'error', id: 'event-error', role: 'alert', hidden: true });
  const tbody = el('tbody', {});
  const table = el('table', { id: 'events', hidden: true },
    el('thead', {}, el('tr', {}, el('th', {}, 'When'), el('th', {}, 'Type'), el('th', {}, 'Data'))),
    tbody);
  const empty = el('p', { class: 'notice', id: 'events-empty', hidden: true },
    'No events' + (types.length > 0 ? ' match this filter' : ' yet') +
    '. Events arrive in event.batch envelopes from the plugin (protocol section 8.1).');
  const older = el('button', { type: 'button', id: 'events-older', hidden: true }, 'Load older');
  app.append(
    el('h1', {}, server.name, ' ', badge(server.linkState || 'unknown', server.linkState),
      el('span', { class: 'muted title-tail' }, ' event feed')),
    filterForm, problem, status, empty, table,
    el('div', { class: 'actions-row' }, older));

  // Rows are keyed by event id and moved rather than rebuilt, so an open
  // <details> stays open across a follow tick and an insertion above it.
  const rows = new Map();
  const draw = (fresh) => {
    for (const event of feed.order) {
      let row = rows.get(event.id);
      if (!row) {
        row = eventRowOrFallback(event, names);
        if (fresh) row.classList.add('new');
        rows.set(event.id, row);
      }
      tbody.append(row);
    }
    table.hidden = feed.order.length === 0;
    empty.hidden = feed.order.length > 0;
    const more = Boolean(feed.nextCursor || feed.gapCursor);
    older.hidden = !more;
    status.textContent = (feed.order.length === 0 ? 'No events shown' : feed.order.length + ' event' + (feed.order.length === 1 ? '' : 's') + ' shown, newest first') +
      (follow ? '; following' : '; not following') +
      (more ? '; older events are available' : '');
  };
  const showProblem = (err) => {
    clear(problem);
    problem.append(el('strong', {}, err.code ? err.code + ': ' : ''), err.message || String(err));
    problem.hidden = false;
  };

  const query = (cursor) => {
    const params = new URLSearchParams();
    for (const term of types) params.append('type', term);
    params.set('limit', String(EVENT_PAGE_SIZE));
    if (cursor) params.set('cursor', cursor);
    return api('GET', '/servers/' + encodeURIComponent(server.id) + '/events?' + params.toString());
  };

  // The first page. A filter the hub refuses (bad_request) or the token
  // does not cover (forbidden) is shown beside the form, which stays so the
  // operator can change it.
  let page;
  try {
    page = await query(null);
  } catch (err) {
    if (seq !== renderSeq) return;
    if (err instanceof ApiError && err.status === 401) throw err;
    status.textContent = 'The feed could not be read.';
    showProblem(err);
    return;
  }
  if (seq !== renderSeq) return;
  mergeEvents(feed, page.events || []);
  feed.nextCursor = page.nextCursor || null;
  feed.topHadCursor = Boolean(page.nextCursor);
  draw(false);

  let loadingOlder = false;
  older.addEventListener('click', async () => {
    // The walk in progress continues first; a gap a follow tick found
    // starts a new walk once the current one has reached the end.
    const fromGap = !feed.nextCursor;
    const from = fromGap ? feed.gapCursor : feed.nextCursor;
    const discovery = feed.gapSerial;
    if (loadingOlder || !from) return;
    loadingOlder = true;
    older.disabled = true;
    try {
      const next = await query(from);
      if (seq !== renderSeq) return;
      mergeEvents(feed, next.events || []);
      feed.nextCursor = next.nextCursor || null;
      // A gap recorded while this page was in flight is a newer discovery
      // and is kept, whatever its text; only the one this walk started
      // from is spent.
      if (fromGap && feed.gapSerial === discovery) feed.gapCursor = null;
      problem.hidden = true;
      draw(false);
    } catch (err) {
      if (seq !== renderSeq) return;
      if (err instanceof ApiError && err.status === 401) {
        signOut('The hub rejected this token.');
        return;
      }
      showProblem(err);
    } finally {
      loadingOlder = false;
      older.disabled = false;
    }
  });

  if (!follow) return;
  let inFlight = false;
  const tick = async () => {
    // A hidden tab keeps its timer but does not spend requests on a page
    // nobody is watching; the next visible tick catches up.
    if (inFlight || document.hidden) return;
    inFlight = true;
    try {
      const latest = await query(null);
      if (seq !== renderSeq) return;
      problem.hidden = true;
      const events = Array.isArray(latest.events) ? latest.events : [];
      // The hub can hold more below its first page than the feed has
      // walked, and this page's cursor is where that starts. The feed
      // cannot know for certain what lies below a cursor without walking
      // it, so it records the cursor as a gap on the two signs it can see,
      // and not otherwise. First, the page ends in an event the feed had
      // not seen: more than a page of new events has landed, and the rest
      // are below. Second, the page carries a cursor where the previous
      // first-page read carried none: the hub crossed the page boundary,
      // and if nothing on the page is new, what made it cross is below.
      // A page that merely shifted by a new event at the top, or that is
      // unchanged over a walk already completed, records nothing, which is
      // what keeps a finished walk from being offered again on every new
      // event. The case this misses is a late event landing below the
      // first page of a hub already past the boundary. The false alarms
      // include a late event landing exactly at the page's edge, a feed
      // of exactly one page growing by one at the top, and exactly a
      // page of new events over walked history; each offers one walk
      // that finds nothing new, and a hub unchanged after it offers no
      // more.
      const tail = events.length > 0 ? events[events.length - 1] : null;
      const tailUnseen = tail !== null && typeof tail.id === 'string' && !feed.byId.has(tail.id);
      const crossed = Boolean(latest.nextCursor) && !feed.topHadCursor;
      feed.topHadCursor = Boolean(latest.nextCursor);
      mergeEvents(feed, events);
      if (latest.nextCursor && (tailUnseen || crossed)) {
        feed.gapCursor = latest.nextCursor;
        feed.gapSerial++;
      }
      draw(true);
    } catch (err) {
      if (seq !== renderSeq) return;
      if (err instanceof ApiError && err.status === 401) {
        signOut('The hub rejected this token.');
        return;
      }
      showProblem(err);
    } finally {
      inFlight = false;
    }
  };
  const timer = setInterval(tick, EVENT_FEED_REFRESH_MS);
  teardown = () => clearInterval(timer);
}

// ---------------------------------------------------------------------------
// Live map
//
// One server's latest state.players snapshot (section 8.3), straight from
// GET /servers/{id}/state/players, drawn on a basemap and listed beside it.
// The basemap is a tileset the hub serves from its maps directory under the
// panel's own path, chosen by the world the server reported in its latest
// core.server.start event (or by ?world= in the route). A server with no
// tileset installed for its world still gets the list, with positions as
// numbers; the view is never blank because imagery is missing.
//
// The snapshot is whole and replaces its predecessor, so every refresh
// replaces every marker: a player absent from the latest snapshot is gone
// from the map. What the map cannot know is how fresh the snapshot is
// beyond what capturedAt says, so that age is always on the page.

// fetchPanelJSON reads a file the panel's own handler serves (a map index
// or manifest): no bearer token, same origin, null when there is none.
async function fetchPanelJSON(path) {
  let response;
  try {
    response = await fetch(path, { headers: { Accept: 'application/json' } });
  } catch (err) {
    throw new ApiError(0, 'unreachable', 'the hub could not be reached: ' + err.message);
  }
  if (response.status === 404) return null;
  if (!response.ok) throw new ApiError(response.status, 'http_' + response.status, 'the hub answered ' + response.status + ' for ' + path);
  let body;
  try {
    body = await response.json();
  } catch {
    throw new ApiError(response.status, 'malformed', path + ' is not JSON');
  }
  return { body, url: response.url };
}

// reportedWorld is the world the server's plugin last announced (the
// DayZ plugin puts the mission's world name in core.server.start). A token
// without events:read cannot see it, which is not an error: the operator
// picks a map by hand.
async function reportedWorld(serverId) {
  try {
    const page = await api('GET', '/servers/' + encodeURIComponent(serverId) + '/events?type=core.server.start&limit=1');
    const event = Array.isArray(page.events) && page.events.length > 0 ? page.events[0] : null;
    const world = event && event.data && typeof event.data === 'object' ? event.data.world : undefined;
    return typeof world === 'string' ? world : '';
  } catch (err) {
    if (err instanceof ApiError && err.status === 403) return '';
    throw err;
  }
}

// snapshotPlayers reads the entries of a state.players read into what the
// map and the list need: identity as a key, the display label, the raw
// position (the widget decides whether it can plot it), and the extras.
function snapshotPlayers(response) {
  const entries = response && response.snapshot && Array.isArray(response.snapshot.players) ? response.snapshot.players : [];
  const players = [];
  for (const entry of entries) {
    if (!entry || !entry.player || typeof entry.player.id !== 'string' || entry.player.id === '') continue;
    const platform = typeof entry.player.platform === 'string' ? entry.player.platform : '';
    players.push({
      key: platform + ':' + entry.player.id,
      id: entry.player.id,
      platform,
      name: typeof entry.name === 'string' ? entry.name : '',
      position: entry.position,
      data: entry.data && typeof entry.data === 'object' && !Array.isArray(entry.data) ? entry.data : {},
    });
  }
  return players;
}

function positionText(manifest, position) {
  if (!Array.isArray(position)) return '';
  const point = manifest ? worldPoint(manifest, position) : null;
  if (point) {
    return manifest.axes.east.name + ' ' + Math.round(point.x) + ', ' + manifest.axes.north.name + ' ' + Math.round(point.z);
  }
  return position.map((value) => (typeof value === 'number' ? String(Math.round(value)) : JSON.stringify(value))).join(', ');
}

async function viewMap(app, route, seq) {
  const server = await api('GET', '/servers/' + encodeURIComponent(route.serverId));
  if (seq !== renderSeq) return;
  const [reported, index] = await Promise.all([reportedWorld(server.id), fetchPanelJSON(MAPS_PATH)]);
  if (seq !== renderSeq) return;
  const installed = index && index.body && Array.isArray(index.body.worlds)
    ? index.body.worlds.filter((world) => typeof world === 'string' && world !== '')
    : [];
  const world = route.world || reported;

  // The tileset for the world, when there is one. A manifest the panel
  // cannot read is reported beside the map controls, not thrown: the list
  // still works without imagery.
  let dataset = null;
  let datasetProblem = null;
  if (world) {
    try {
      const fetched = await fetchPanelJSON(MAPS_PATH + encodeURIComponent(world) + '/manifest.json');
      if (seq !== renderSeq) return;
      if (fetched) dataset = { manifest: validateManifest(fetched.body), baseURL: fetched.url };
    } catch (err) {
      if (seq !== renderSeq) return;
      if (err instanceof ApiError && err.status === 401) throw err;
      datasetProblem = err;
    }
  }
  const manifest = dataset ? dataset.manifest : null;

  setCrumbs([{ label: 'Servers', href: '#/' }, { label: server.name, href: serverHref(server.id) }, { label: 'Map' }]);
  clear(app);

  const worldSelect = el('select', { id: 'map-world' },
    el('option', { value: '' }, reported ? 'as reported by the server (' + reported + ')' : 'none chosen'),
    installed.map((entry) => el('option', { value: entry }, entry)));
  worldSelect.value = installed.includes(route.world) ? route.world : '';
  worldSelect.addEventListener('change', () => { location.hash = mapHref(server.id, worldSelect.value); });
  const controls = el('div', { class: 'card map-controls' },
    el('label', { class: 'field' },
      el('span', { class: 'name' }, 'Map'),
      worldSelect,
      el('span', { class: 'hint' }, installed.length > 0
        ? 'Installed maps: ' + installed.join(', ') + '. The server reports its world in core.server.start' + (reported ? ' (' + reported + ')' : ' (none readable)') + '.'
        : 'No map tilesets are installed on this hub (see panel/README.md, "Map tilesets"); players are listed with their positions as numbers.')));

  const status = el('p', { class: 'muted', id: 'map-status' }, 'Loading…');
  const problem = el('div', { class: 'error', id: 'map-error', role: 'alert', hidden: true });
  const tbody = el('tbody', {});
  const table = el('table', { id: 'players', hidden: true },
    el('thead', {}, el('tr', {}, el('th', {}, 'Player'), el('th', {}, 'Identity'), el('th', {}, 'Position'), el('th', {}, 'Data'), el('th', {}, ''))),
    tbody);
  const empty = el('p', { class: 'notice', id: 'players-empty', hidden: true }, 'Nobody is online in the latest snapshot.');

  let notice = null;
  if (datasetProblem) {
    notice = el('div', { class: 'error', id: 'map-missing', role: 'alert' },
      'The map for world ' + world + ' could not be loaded: ' + (datasetProblem.message || String(datasetProblem)));
  } else if (!manifest) {
    notice = el('p', { class: 'notice', id: 'map-missing' }, world
      ? 'No map is installed for world ' + world + '. Players are listed below with their positions.'
      : 'This server has not reported a world (no core.server.start event is readable), and no map is chosen. Players are listed below with their positions.');
  }
  const mapContainer = manifest ? el('div', { class: 'map', id: 'map', 'data-world': manifest.world }) : null;

  // The panel's own append, which skips the notice or the container that
  // is null; the DOM's would print the word.
  append(app, [
    el('h1', {}, server.name, ' ', badge(server.linkState || 'unknown', server.linkState),
      el('span', { class: 'muted title-tail' }, ' live map')),
    controls, problem, status, notice, mapContainer, empty, table]);

  const state = { response: null, players: [], loaded: false };

  // The widget is created once the container is laid out, so its first
  // measurement is the real one.
  let map = null;
  if (mapContainer) {
    map = createMap(mapContainer, {
      onSelect: (key) => {
        const player = state.players.find((entry) => entry.key === key);
        if (player) location.hash = serverHref(server.id, player.id);
      },
    });
    map.setDataset(manifest, dataset.baseURL);
  }

  const draw = () => {
    clear(tbody);
    let plotted = 0;
    for (const player of state.players) {
      const point = manifest ? worldPoint(manifest, player.position) : null;
      if (point) plotted++;
      const row = el('tr', { 'data-player-key': player.key, 'data-player-id': player.id },
        el('td', {}, player.name || el('span', { class: 'muted' }, 'unnamed')),
        el('td', { class: 'mono' }, player.key),
        el('td', { class: 'position' }, Array.isArray(player.position)
          ? positionText(manifest, player.position)
          : el('span', { class: 'muted' }, 'no position')),
        el('td', { class: 'data' }, Object.keys(player.data).length > 0
          ? attempt(() => summarizeEventData(player.data), 'The data is nested too deeply to summarize.')
          : el('span', { class: 'muted' }, 'none')),
        el('td', { class: 'row-actions' },
          point ? el('button', {
            type: 'button', class: 'small', 'data-show': player.key,
            onclick: () => { map.setHighlight(player.key); map.focus(player.key); },
          }, 'Show') : null,
          ' ',
          el('a', { class: 'button small', href: serverHref(server.id, player.id), 'data-target': player.id }, 'Actions')));
      if (map) {
        row.addEventListener('mouseenter', () => map.setHighlight(player.key));
        row.addEventListener('mouseleave', () => map.setHighlight(null));
      }
      tbody.append(row);
    }
    if (map) {
      map.setMarkers(state.players.map((player) => ({
        key: player.key, label: player.name || player.id, position: player.position,
      })));
    }
    table.hidden = state.players.length === 0;
    empty.hidden = !state.loaded || state.players.length > 0;
    status.dataset.plotted = String(plotted);
    status.dataset.players = String(state.players.length);
    updateStatus();
  };
  const updateStatus = () => {
    if (!state.loaded) return;
    if (!state.response) {
      status.textContent = 'No player snapshot yet. The plugin publishes state.players snapshots (protocol section 8.3); until the hub accepts one there is nothing to show.';
      return;
    }
    const count = state.players.length;
    const unplotted = count - Number(status.dataset.plotted || 0);
    status.textContent = count + ' player' + (count === 1 ? '' : 's') + ' in the latest snapshot, captured ' +
      ago(state.response.capturedAt) + ', received ' + ago(state.response.receivedAt) +
      (manifest && unplotted > 0 ? '; ' + unplotted + ' without a position the map can plot' : '') +
      (manifest ? '' : '; listed without a map') +
      '. Re-read every ' + (MAP_REFRESH_MS / 1000) + ' s.';
  };
  const showProblem = (err) => {
    clear(problem);
    problem.append(el('strong', {}, err.code ? err.code + ': ' : ''), err.message || String(err));
    problem.hidden = false;
  };

  const load = async () => {
    let response = null;
    try {
      response = await api('GET', '/servers/' + encodeURIComponent(server.id) + '/state/players');
    } catch (err) {
      if (!(err instanceof ApiError && err.status === 404)) throw err;
    }
    if (seq !== renderSeq) return;
    state.response = response;
    state.players = snapshotPlayers(response);
    state.loaded = true;
    problem.hidden = true;
    draw();
  };

  try {
    await load();
  } catch (err) {
    if (seq !== renderSeq) return;
    if (err instanceof ApiError && err.status === 401) throw err;
    status.textContent = 'The snapshot could not be read.';
    showProblem(err);
  }
  if (seq !== renderSeq) return;

  let inFlight = false;
  const tick = async () => {
    if (inFlight || document.hidden) return;
    inFlight = true;
    try {
      await load();
    } catch (err) {
      if (seq !== renderSeq) return;
      if (err instanceof ApiError && err.status === 401) {
        signOut('The hub rejected this token.');
        return;
      }
      showProblem(err);
    } finally {
      inFlight = false;
    }
  };
  const timer = setInterval(tick, MAP_REFRESH_MS);
  const ager = setInterval(updateStatus, 1000);
  teardown = () => {
    clearInterval(timer);
    clearInterval(ager);
    if (map) map.destroy();
  };
}

// ---------------------------------------------------------------------------
// Schema-driven forms
//
// A field is { node, read(errors), setError(message) }. read returns the JSON
// value the field holds, or undefined for an optional field left empty (an
// omitted key, not an empty string, so an optional integer with a minimum is
// not sent as ""). Client-side checks are the minimum needed to build a
// value at all; the hub validates against the schema and its faults come
// back by path, which fieldsByPath maps onto the inputs.

let fieldCounter = 0;

function nextId(prefix) {
  fieldCounter++;
  return prefix + '-' + fieldCounter;
}

function widgetOf(schema) {
  const hint = schema['x-vyshka-widget'];
  return typeof hint === 'string' ? hint : '';
}

function describe(schema) {
  const parts = [];
  if (schema.type) parts.push(schema.type);
  if (schema.minimum !== undefined) parts.push('min ' + schema.minimum);
  if (schema.exclusiveMinimum !== undefined) parts.push('above ' + schema.exclusiveMinimum);
  if (schema.maximum !== undefined) parts.push('max ' + schema.maximum);
  if (schema.exclusiveMaximum !== undefined) parts.push('below ' + schema.exclusiveMaximum);
  if (schema.default !== undefined) parts.push('default ' + JSON.stringify(schema.default));
  const widget = widgetOf(schema);
  if (widget) parts.push('widget ' + widget);
  return parts.join(', ');
}

// Every field answers three questions. entered(): has the operator put
// anything into it (a value different from what the form started with)?
// read(errors, present): its JSON value, or undefined to omit the key, with
// present saying whether the object it belongs to is being sent at all; a
// required field reports emptiness only when present. setError(message):
// show a fault from the hub or from read.
//
// Presence is what makes optional objects behave: an optional object with
// nothing entered anywhere inside it is omitted whole, whatever its children
// require, and a required child's auto-valued control (a checkbox, a select
// with no blank option) does not count as entering anything. Once something
// is entered, the object is present, every required child is enforced, and
// every fault inside it is reported, so invalid input is never silently
// dropped by the omission rule.
//
// enforced says whether the browser itself should refuse a submit with the
// field empty: a required field of an object that is always present. Inside
// an optional object the mark stays soft, and read enforces it instead once
// the object is present.
function enforced(opts) {
  return Boolean(opts.required) && !opts.soft;
}

function requireIf(opts, present, errors) {
  if (opts.required && present) errors.push({ path: opts.path, message: 'is required' });
}

// constrained applies the input's own HTML constraints (min, max, step) to
// a non-empty value, standing in for the native check the form turns off
// with novalidate; the message is the browser's.
function constrained(input, opts, errors) {
  // Emptiness is the caller's (requireIf), so that a required field's
  // message is the same whether the browser marks it required or not.
  if (input.validity.valid || input.validity.valueMissing) return true;
  errors.push({ path: opts.path, message: input.validationMessage || 'is not valid' });
  return false;
}

// Include boxes are recognised by a marker the form sets, not by their name:
// a manifest may legally declare a boolean property called __include, and its
// checkbox must stay an ordinary data control.
function isIncludeBox(node) {
  return node instanceof HTMLInputElement && node.dataset.include === 'true';
}

function wrap(opts, control, hint, inline) {
  const errorNode = el('span', { class: 'field-error', hidden: true });
  const node = el('label', { class: 'field' + (inline ? ' inline' : ''), 'data-path': opts.path },
    inline ? control : null,
    el('span', { class: 'name' }, opts.label, opts.required ? el('span', { class: 'req', title: 'required' }, '*') : null),
    inline ? null : control,
    hint ? el('span', { class: 'hint' }, hint) : null,
    errorNode);
  return {
    node,
    setError(message) {
      errorNode.textContent = message || '';
      errorNode.hidden = !message;
      node.classList.toggle('invalid', Boolean(message));
    },
  };
}

function buildField(schema, opts) {
  schema = schema && typeof schema === 'object' && !Array.isArray(schema) ? schema : {};
  if (Array.isArray(schema.enum)) return enumField(schema, opts);
  switch (schema.type) {
    case 'boolean': return booleanField(schema, opts);
    case 'integer':
    case 'number': return numberField(schema, opts);
    case 'string': return stringField(schema, opts);
    case 'array': return arrayField(schema, opts);
    case 'object': return objectField(schema, opts);
    case 'null': return nullField(schema, opts);
    default: return jsonField(schema, opts);
  }
}

function enumField(schema, opts) {
  const select = el('select', { name: opts.name, id: nextId('field'), required: enforced(opts) });
  if (!opts.required) select.append(el('option', { value: '' }, '(not set)'));
  const wanted = schema.default !== undefined ? JSON.stringify(schema.default) : null;
  schema.enum.forEach((member, index) => {
    const text = typeof member === 'string' ? member : JSON.stringify(member);
    const option = el('option', { value: String(index) }, text);
    if (wanted !== null && JSON.stringify(member) === wanted) option.selected = true;
    select.append(option);
  });
  const wrapped = wrap(opts, select, describe(schema));
  const initial = select.value;
  return {
    node: wrapped.node, setError: wrapped.setError,
    entered() { return select.value !== initial; },
    read(errors, present) {
      if (select.value === '') {
        requireIf(opts, present, errors);
        return undefined;
      }
      return schema.enum[Number(select.value)];
    },
  };
}

function booleanField(schema, opts) {
  // A checkbox has no "not set". A required boolean, or one with a default,
  // is always sent; an optional one without a default gets a three-way
  // select so the key can stay absent, which a plugin may treat differently
  // from false.
  if (!opts.required && typeof schema.default !== 'boolean') {
    const select = el('select', { name: opts.name, id: nextId('field') },
      el('option', { value: '' }, '(not set)'), el('option', { value: 'true' }, 'true'), el('option', { value: 'false' }, 'false'));
    const wrapped = wrap(opts, select, describe(schema));
    return {
      node: wrapped.node, setError: wrapped.setError,
      entered() { return select.value !== ''; },
      read() { return select.value === '' ? undefined : select.value === 'true'; },
    };
  }
  const input = el('input', { type: 'checkbox', name: opts.name, id: nextId('field') });
  if (schema.default === true) input.checked = true;
  const initial = input.checked;
  const wrapped = wrap(opts, input, describe(schema), true);
  return {
    node: wrapped.node, setError: wrapped.setError,
    entered() { return input.checked !== initial; },
    read() { return input.checked; },
  };
}

// numberBounds turns the schema's bounds into what an HTML number input can
// enforce. HTML has inclusive bounds only, so an integer's exclusive bounds
// move inward by one and fractional bounds round inward (an integer above
// 0.5 is at least 1); a real number's exclusive bound cannot be expressed
// and is left to the hub's validation, which the form surfaces by path.
function numberBounds(schema) {
  const integer = schema.type === 'integer';
  const finite = (value) => (typeof value === 'number' && Number.isFinite(value) ? value : undefined);
  let min = finite(schema.minimum);
  let max = finite(schema.maximum);
  const exclusiveMin = finite(schema.exclusiveMinimum);
  const exclusiveMax = finite(schema.exclusiveMaximum);
  if (integer) {
    if (min !== undefined) min = Math.ceil(min);
    if (max !== undefined) max = Math.floor(max);
    if (exclusiveMin !== undefined) {
      const shifted = Math.floor(exclusiveMin) + 1;
      min = min === undefined ? shifted : Math.max(min, shifted);
    }
    if (exclusiveMax !== undefined) {
      const shifted = Math.ceil(exclusiveMax) - 1;
      max = max === undefined ? shifted : Math.min(max, shifted);
    }
  }
  return { min, max, step: integer ? '1' : 'any' };
}

function numberField(schema, opts) {
  const bounds = numberBounds(schema);
  const input = el('input', {
    type: 'number', name: opts.name, id: nextId('field'), required: enforced(opts),
    min: bounds.min, max: bounds.max, step: bounds.step,
    value: typeof schema.default === 'number' ? String(schema.default) : undefined,
  });
  const wrapped = wrap(opts, input, describe(schema));
  const initial = input.value;
  return {
    node: wrapped.node, setError: wrapped.setError,
    entered() { return input.value !== initial; },
    read(errors, present) {
      // A number input holding text it cannot parse ("1e") reports an
      // empty value with badInput set; that is a fault, not an omission.
      if (!constrained(input, opts, errors)) return undefined;
      const text = input.value.trim();
      if (text === '') {
        requireIf(opts, present, errors);
        return undefined;
      }
      const value = Number(text);
      if (!Number.isFinite(value)) {
        errors.push({ path: opts.path, message: 'must be a number' });
        return undefined;
      }
      if (schema.type === 'integer' && !Number.isInteger(value)) {
        errors.push({ path: opts.path, message: 'must be an integer' });
        return undefined;
      }
      return value;
    },
  };
}

function playerDatalist(players) {
  if (!players || players.length === 0) return null;
  const list = el('datalist', { id: nextId('players') });
  for (const entry of players) {
    list.append(el('option', { value: entry.id }, entry.name ? entry.name + ' (' + entry.platform + ')' : entry.platform));
  }
  return list;
}

function stringField(schema, opts) {
  const widget = widgetOf(schema);
  // Hints shape the input, never its validation (section 6.1: a hint does
  // not constrain the data model), so a webhook stays a text input with a
  // URL keyboard rather than a URL input that would refuse a relative path
  // the schema allows.
  const attrs = {
    type: 'text', name: opts.name, id: nextId('field'), required: enforced(opts),
    value: typeof schema.default === 'string' ? schema.default : undefined,
    spellcheck: 'false', autocomplete: 'off',
  };
  let hint = describe(schema);
  let datalist = null;
  if (widget === 'player') {
    datalist = playerDatalist(opts.players);
    if (datalist) attrs.list = datalist.id;
    attrs.placeholder = 'platform player id';
    hint = 'player identity (the platform id, e.g. the Steam64 id on DayZ)' + (datalist ? '; online players are suggested' : '');
  } else if (widget === 'webhook') {
    attrs.inputmode = 'url';
    attrs.placeholder = 'https://';
  } else if (widget === 'vector') {
    attrs.placeholder = 'x y z';
  } else if (widget === 'itemlist') {
    attrs.placeholder = 'item class name';
  }
  const input = el('input', attrs);
  const wrapped = wrap(opts, datalist ? el('span', {}, input, datalist) : input, hint);
  const initial = input.value;
  return {
    node: wrapped.node, setError: wrapped.setError,
    entered() { return input.value !== initial; },
    // set fills the field in from outside the form, as a preselected
    // target arriving in the route does; it counts as entered.
    set(value) { input.value = value; },
    read(errors, present) {
      const value = input.value;
      if (value === '') {
        requireIf(opts, present, errors);
        return undefined;
      }
      return value;
    },
  };
}

function coerceItem(itemSchema, text, errors, path) {
  const type = itemSchema && itemSchema.type;
  switch (type) {
    case 'integer':
    case 'number': {
      const value = Number(text);
      if (!Number.isFinite(value) || (type === 'integer' && !Number.isInteger(value))) {
        errors.push({ path, message: 'must be ' + (type === 'integer' ? 'an integer' : 'a number') + ', got ' + JSON.stringify(text) });
        return undefined;
      }
      return value;
    }
    case 'boolean':
      if (text === 'true') return true;
      if (text === 'false') return false;
      errors.push({ path, message: 'must be true or false, got ' + JSON.stringify(text) });
      return undefined;
    case 'string':
      return text;
    default:
      try {
        return JSON.parse(text);
      } catch {
        errors.push({ path, message: 'is not valid JSON' });
        return undefined;
      }
  }
}

function arrayField(schema, opts) {
  const items = schema.items && typeof schema.items === 'object' ? schema.items : {};
  const numericItems = items.type === 'number' || items.type === 'integer';
  if (widgetOf(schema) === 'vector' && numericItems) {
    const axes = ['x', 'y', 'z'].map((axis) => el('input', {
      type: 'number', step: items.type === 'integer' ? '1' : 'any', placeholder: axis,
      name: opts.name + '.' + axis, 'aria-label': opts.label + ' ' + axis,
    }));
    if (Array.isArray(schema.default)) {
      schema.default.slice(0, 3).forEach((value, index) => { axes[index].value = String(value); });
    }
    const wrapped = wrap(opts, el('span', { class: 'vector' }, axes),
      'vector: x, y, and z, or x and y for a flat position');
    const initial = axes.map((axis) => axis.value);
    return {
      node: wrapped.node, setError: wrapped.setError,
      entered() { return axes.some((axis, index) => axis.value !== initial[index]); },
      read(errors, present) {
        // Malformed text in an axis reads as empty with badInput set; it
        // must not pass for a deliberately blank z or an untouched vector.
        let malformed = false;
        axes.forEach((axis, index) => {
          if (axis.validity.badInput) {
            errors.push({ path: opts.path + '[' + index + ']', message: 'coordinate ' + ['x', 'y', 'z'][index] + ' is not a number' });
            malformed = true;
          }
        });
        if (malformed) return undefined;
        const texts = axes.map((axis) => axis.value.trim());
        if (texts.every((text) => text === '')) {
          requireIf(opts, present, errors);
          return undefined;
        }
        // A blank coordinate is never a zero. The z axis alone may be left
        // blank, for the two-number positions section 8.3 allows.
        const used = texts[2] === '' ? texts.slice(0, 2) : texts;
        let complete = true;
        used.forEach((text, index) => {
          if (text === '') {
            errors.push({ path: opts.path + '[' + index + ']', message: 'coordinate ' + ['x', 'y', 'z'][index] + ' is required' });
            complete = false;
          }
        });
        if (!complete) return undefined;
        const values = used.map((text, index) => coerceItem(items, text, errors, opts.path + '[' + index + ']'));
        return values.some((value) => value === undefined) ? undefined : values;
      },
    };
  }
  const textarea = el('textarea', { name: opts.name, id: nextId('field'), spellcheck: 'false' });
  if (Array.isArray(schema.default)) {
    textarea.value = schema.default.map((value) => (typeof value === 'string' ? value : JSON.stringify(value))).join('\n');
  }
  const kind = items.type ? items.type + ' items' : 'JSON items';
  const wrapped = wrap(opts, textarea, 'one value per line, ' + kind +
    (items.type === 'string' ? '; whitespace is kept, an empty line is not an item' : ''));
  const initial = textarea.value;
  return {
    node: wrapped.node, setError: wrapped.setError,
    entered() { return textarea.value !== initial; },
    read(errors, present) {
      // String items are taken as typed, whitespace and all, and only an
      // empty line is not an item, so an empty string cannot be expressed
      // here (a limitation of the one-per-line form, not of the protocol).
      // Other kinds are trimmed before parsing and blank lines dropped.
      const raw = textarea.value.split('\n');
      const lines = items.type === 'string'
        ? raw.filter((line) => line !== '')
        : raw.map((line) => line.trim()).filter((line) => line !== '');
      // Empty: a required array of a present object is sent empty; an
      // optional one is omitted.
      if (lines.length === 0) return opts.required && present ? [] : undefined;
      const values = lines.map((line, index) => coerceItem(items, line, errors, opts.path + '[' + index + ']'));
      return values.some((value) => value === undefined) ? undefined : values;
    },
  };
}

function objectField(schema, opts) {
  const properties = schema.properties && typeof schema.properties === 'object' ? schema.properties : null;
  if (!properties || Object.keys(properties).length === 0) return jsonField(schema, opts);
  const required = new Set(Array.isArray(schema.required) ? schema.required : []);
  // An optional object is sent only when included, and inclusion is a
  // control of its own (a checkbox in the legend) rather than an inference
  // from its children: it ticks itself the moment anything inside is
  // entered, so the common case costs nothing, and it can be ticked by hand
  // to send an object whose only values are its initial ones (a required
  // boolean left false, a prefilled default), which no inference from
  // "was something typed" could express. Left unticked, the object is
  // omitted whole, which is valid whatever it requires of its children, so
  // their required marks are soft in the browser and enforced by read once
  // the object is present. A required object is present whenever its parent
  // is, whatever an optional ancestor made of its browser-side softness.
  const optional = !opts.required;
  const keys = Object.keys(properties);
  const children = keys.map((key) => buildField(properties[key], {
    name: opts.name + '.' + key,
    path: opts.path ? opts.path + '.' + key : key,
    label: key,
    required: required.has(key),
    soft: optional || opts.soft,
    players: opts.players,
    register: opts.register,
  }));
  const includeBox = optional && opts.path !== ''
    ? el('input', { type: 'checkbox', name: opts.name + '.__include', 'data-include': 'true', 'aria-label': 'include ' + opts.label })
    : null;
  const errorNode = el('span', { class: 'field-error', hidden: true });
  const node = el('fieldset', { 'data-path': opts.path },
    el('legend', {}, includeBox ? el('label', { class: 'include' }, includeBox, ' ') : null,
      opts.label, opts.required ? el('span', { class: 'req' }, '*') : null,
      includeBox ? el('span', { class: 'hint' }, ' (optional: ticked when included)') : null),
    children.map((child) => child.node), errorNode);
  if (includeBox) {
    // Anything entered inside includes the object; the box itself is the
    // one control here that must not do that (unticking it excludes).
    const includeOnInput = (event) => {
      // A descendant's include box being ticked includes this object
      // too; being unticked is an exclusion, not entered data, and must
      // not undo an exclusion of this object made by hand.
      if (event.target === includeBox) return;
      if (isIncludeBox(event.target) && !event.target.checked) return;
      includeBox.checked = true;
    };
    node.addEventListener('input', includeOnInput);
    node.addEventListener('change', includeOnInput);
  }
  const field = {
    node,
    setError(message) {
      errorNode.textContent = message || '';
      errorNode.hidden = !message;
    },
    entered() { return includeBox ? includeBox.checked : children.some((child) => child.entered()); },
    read(errors, present) {
      // Decided before any child is read: the root is always sent, a
      // required object is sent whenever its parent is, and an optional
      // object is sent when included, which is also when its faults start
      // to count.
      const include = opts.path === '' || (opts.required && present) || (includeBox !== null && includeBox.checked);
      if (!include) return undefined;
      // Null prototype: a manifest may name a property __proto__, and on
      // an ordinary object that assignment would go to the prototype
      // setter instead of becoming a key the hub can see.
      const value = Object.create(null);
      children.forEach((child, index) => {
        const item = child.read(errors, true);
        if (item !== undefined) value[keys[index]] = item;
      });
      return value;
    },
  };
  for (const child of children) opts.register(child);
  return field;
}

function nullField(schema, opts) {
  // The only valid value is null, so a required one is sent as null and an
  // optional one is left absent, which is the closest a form can come to
  // "not set" for a key whose value carries no information.
  const wrapped = wrap(opts, el('span', { class: 'muted mono' }, opts.required ? 'null' : 'not sent'), describe(schema));
  return {
    node: wrapped.node, setError: wrapped.setError,
    entered() { return false; },
    read(errors, present) { return opts.required && present ? null : undefined; },
  };
}

function jsonField(schema, opts) {
  const textarea = el('textarea', { name: opts.name, id: nextId('field'), spellcheck: 'false' });
  if (schema.default !== undefined) {
    textarea.value = pretty(schema.default);
  } else if (schema.type === 'object' && enforced(opts)) {
    textarea.value = '{}';
  } else if (schema.type === 'object') {
    textarea.placeholder = '{}';
  }
  const wrapped = wrap(opts, textarea, 'JSON' + (schema.type ? ', ' + schema.type : ''));
  const initial = textarea.value;
  return {
    node: wrapped.node, setError: wrapped.setError,
    entered() { return textarea.value !== initial; },
    read(errors, present) {
      const text = textarea.value.trim();
      if (text === '') {
        requireIf(opts, present, errors);
        return undefined;
      }
      try {
        return JSON.parse(text);
      } catch {
        errors.push({ path: opts.path, message: 'is not valid JSON' });
        return undefined;
      }
    },
  };
}

// buildParamsForm turns an action's params schema into fields, returning the
// container node, a read() that yields the params object, and a map from
// hub fault paths to the fields that own them.
function buildParamsForm(paramsSchema, players) {
  const fieldsByPath = new Map();
  const register = (field) => { fieldsByPath.set(field.node.dataset.path, field); };
  const container = el('div', { class: 'stack', id: 'params' });
  if (!paramsSchema || typeof paramsSchema !== 'object') {
    container.append(el('p', { class: 'muted' }, 'This action takes no parameters.'));
    return { node: container, fieldsByPath, read: () => ({}) };
  }
  const root = buildField(paramsSchema, {
    name: 'params', path: '', label: 'Parameters', required: true, players, register,
  });
  register(root);
  container.append(root.node);
  if (paramsSchema.type && paramsSchema.type !== 'object') {
    container.append(el('p', { class: 'notice' },
      'This schema declares params of type ' + paramsSchema.type + ', but the Admin API only accepts a JSON object; the dispatch will be refused until the plugin fixes its manifest.'));
  }
  return {
    node: container,
    fieldsByPath,
    read(errors) {
      const value = root.read(errors, true);
      return value === undefined ? {} : value;
    },
  };
}

// showFaults places each { path, message } on the field that owns it, falling
// back to the nearest enclosing field for array indices the form does not
// model individually, and returns the ones nothing claimed.
function showFaults(faults, fieldsByPath) {
  for (const field of fieldsByPath.values()) field.setError(null);
  const orphans = [];
  for (const fault of faults) {
    let path = fault.path || '';
    let field = fieldsByPath.get(path);
    while (!field && path !== '') {
      const stripped = path.replace(/(\[\d+\]|\.[^.\[\]]+)$/, '');
      if (stripped === path) break;
      path = stripped;
      field = fieldsByPath.get(path);
    }
    if (field && (fault.path || '') !== '') {
      field.setError(fault.path + ' ' + fault.message);
    } else {
      orphans.push(fault);
    }
  }
  return orphans;
}

// ---------------------------------------------------------------------------
// Action form and dispatch

function targetField(action, contexts, players) {
  const context = action.context || '';
  if (context === '' || context === 'world') return null;
  const builtin = { player: 'Player', vehicle: 'Vehicle id', object: 'Object id' };
  let label = builtin[context];
  let required = Boolean(label);
  if (!label) {
    const declared = contexts.find((entry) => entry && entry.id === context);
    label = (declared && declared.name ? declared.name : context) + ' reference';
    required = false;
  }
  return stringField(
    context === 'player' ? { type: 'string', 'x-vyshka-widget': 'player' } : { type: 'string' },
    { name: 'referenceKey', path: 'referenceKey', label, required, players });
}

async function loadPlayers(serverId) {
  try {
    const snapshot = await api('GET', '/servers/' + encodeURIComponent(serverId) + '/state/players');
    const entries = snapshot && snapshot.snapshot && Array.isArray(snapshot.snapshot.players) ? snapshot.snapshot.players : [];
    return entries
      .filter((entry) => entry && entry.player && entry.player.id)
      .map((entry) => ({ id: entry.player.id, platform: entry.player.platform || '', name: entry.name || '' }));
  } catch (err) {
    if (err instanceof ApiError && (err.status === 404 || err.status === 403)) return [];
    throw err;
  }
}

async function viewAction(app, route, seq) {
  const { server, manifest } = await loadServerAndManifest(route.serverId);
  if (seq !== renderSeq) return;
  setCrumbs([{ label: 'Servers', href: '#/' }, { label: server.name, href: serverHref(server.id) }, { label: route.code }]);
  const body = manifest ? manifest.manifest || {} : {};
  const action = (Array.isArray(body.actions) ? body.actions : []).find((entry) => entry && entry.code === route.code);
  clear(app);
  if (!action) {
    app.append(el('div', { class: 'error', role: 'alert' },
      manifest
        ? 'The server’s manifest (revision ' + manifest.revision + ') does not declare ' + route.code + '.'
        : 'This server has not published a manifest, so nothing can be dispatched.'));
    return;
  }
  const needsPlayers = action.context === 'player' || JSON.stringify(action.params || {}).includes('"player"');
  const players = needsPlayers ? await loadPlayers(server.id) : [];
  if (seq !== renderSeq) return;

  const contexts = Array.isArray(body.contexts) ? body.contexts : [];
  const target = targetField(action, contexts, players);
  const params = buildParamsForm(action.params, players);
  if (target) params.fieldsByPath.set('referenceKey', target);
  // A player picked on the map arrives in the route and lands in the
  // target field, which stays editable: the preselection is a convenience,
  // not a lock.
  if (target && route.player && action.context === 'player') target.set(route.player);

  const danger = action.danger || 'none';
  let confirmBox = null;
  if (danger !== 'none') {
    confirmBox = el('input', { type: 'checkbox', id: 'confirm-danger', required: true });
  }

  const formErrors = el('div', { class: 'error', id: 'form-errors', role: 'alert', hidden: true });
  const submit = el('button', { type: 'submit', class: 'primary', id: 'dispatch' }, 'Dispatch');
  const result = el('div', { id: 'result' });
  // The idempotency key is bound to the exact request it was minted for.
  // Resending that request (the hub accepted it but the answer was lost)
  // reuses the key and gets the same action back, as section 7 intends; a
  // request that differs in any way is a different action and gets a fresh
  // key, so an edit can never be answered with the unedited original.
  let idempotencyKey = randomKey();
  let keyedRequest = null;
  let stopWatching = null;
  teardown = () => { if (stopWatching) stopWatching(); };

  // novalidate: the browser's own constraint check has no notion of which
  // optional objects are included, so a stray value inside an excluded one
  // would block a dispatch that does not send it. Each field's read applies
  // the input's constraints itself, for the fields that are present.
  const form = el('form', {
    class: 'stack card', id: 'action-form', novalidate: true,
    onsubmit: async (event) => {
      event.preventDefault();
      formErrors.hidden = true;
      clear(formErrors);
      const errors = [];
      const referenceKey = target ? target.read(errors, true) : undefined;
      const paramsValue = params.read(errors);
      const orphans = showFaults(errors, params.fieldsByPath);
      if (confirmBox && !confirmBox.checked) {
        orphans.push({ path: '', message: 'tick the confirmation box: this action is marked ' + danger + ' by its plugin' });
      }
      if (errors.length > 0 || orphans.length > 0) {
        if (orphans.length > 0) listFaults(formErrors, 'Fix these before dispatching:', orphans);
        return;
      }
      const request = { code: action.code, params: paramsValue };
      if (action.context) request.context = action.context;
      if (referenceKey !== undefined) request.referenceKey = referenceKey;
      const fingerprint = JSON.stringify(request);
      if (keyedRequest !== null && keyedRequest !== fingerprint) idempotencyKey = randomKey();
      keyedRequest = fingerprint;
      request.idempotencyKey = idempotencyKey;
      submit.disabled = true;
      try {
        const accepted = await api('POST', '/servers/' + encodeURIComponent(server.id) + '/actions', request);
        if (seq !== renderSeq) return;
        // Accepted, so this key is spent: the next dispatch, even of the
        // same values, is a new action and must not replay this one.
        idempotencyKey = randomKey();
        keyedRequest = null;
        if (stopWatching) stopWatching();
        stopWatching = watchAction(result, accepted.actionId, action, request, () => { submit.disabled = false; });
      } catch (err) {
        submit.disabled = false;
        if (seq !== renderSeq) return;
        if (err instanceof ApiError && err.status === 401) {
          signOut('The hub rejected this token.');
          return;
        }
        if (err instanceof ApiError && err.code === 'params_invalid' && err.details && Array.isArray(err.details.errors)) {
          const unplaced = showFaults(err.details.errors, params.fieldsByPath);
          listFaults(formErrors, 'The hub refused the parameters (params_invalid):', unplaced);
          return;
        }
        formErrors.hidden = false;
        formErrors.append(el('strong', {}, err.code ? err.code + ': ' : ''), err.message);
      }
    },
  },
  el('h1', {}, action.name || action.code, ' ',
    action.context ? badge(action.context) : null, ' ',
    danger !== 'none' ? badge(danger, danger) : null),
  el('p', { class: 'muted mono' }, action.code),
  target ? target.node : null,
  params.node,
  confirmBox ? el('label', { class: 'field inline ' + danger }, confirmBox,
    el('span', { class: 'name' }, danger === 'destructive'
      ? 'I understand this action is destructive and cannot be undone'
      : 'I understand this action is marked as a warning by its plugin')) : null,
  formErrors,
  el('div', { class: 'actions-row' }, submit,
    el('span', { class: 'muted' }, 'validated against manifest revision ' + manifest.revision + ' before it is queued')));

  app.append(serverSummaryCompact(server), form, result);
}

function serverSummaryCompact(server) {
  return el('p', { class: 'muted' },
    el('a', { href: serverHref(server.id) }, server.name), ' ',
    badge(server.linkState || 'unknown', server.linkState),
    server.linkState !== 'up' ? ': the link is not up, so a dispatch queues until the plugin polls, or expires at its TTL.' : '');
}

function listFaults(container, heading, faults) {
  container.hidden = false;
  clear(container);
  container.append(el('strong', {}, heading));
  if (faults.length > 0) {
    container.append(el('ul', {}, faults.map((fault) => el('li', {},
      fault.path ? el('span', { class: 'mono' }, fault.path + ' ') : null, fault.message))));
  }
}

// watchAction polls GET /actions/{id} until the action reaches a terminal
// state, drawing the lifecycle as it goes. It returns a function that stops
// the polling, for navigation away and for the next dispatch.
function watchAction(container, actionId, action, request, onSettled) {
  let stopped = false;
  let timer = null;
  const draw = (record, err) => {
    clear(container);
    const states = ['queued', 'delivered', 'running', 'completed'];
    const reachedIndex = record
      ? (TERMINAL_STATES.has(record.state) ? states.length - 1 : states.indexOf(record.state))
      : 0;
    const timeline = el('ol', { class: 'timeline', id: 'timeline' }, states.map((state, index) => {
      let label = state;
      let cls = '';
      if (record && index === states.length - 1 && record.state !== 'completed' && TERMINAL_STATES.has(record.state)) {
        label = record.state;
        cls = 'bad';
      } else if (index <= reachedIndex) {
        cls = 'reached';
      }
      if (index === reachedIndex) cls += ' current';
      return el('li', { class: cls }, label);
    }));
    const card = el('div', { class: 'card', id: 'result-card' },
      el('h2', {}, 'Dispatched ', el('span', { class: 'mono' }, actionId)),
      timeline,
      el('dl', { class: 'kv' },
        el('dt', {}, 'State'), el('dd', { id: 'result-state' }, record ? record.state : 'queued'),
        el('dt', {}, 'Sent'), el('dd', {}, el('pre', {}, pretty({ code: request.code, context: request.context, referenceKey: request.referenceKey, params: request.params }))),
        record && record.createdAt ? [el('dt', {}, 'Created'), el('dd', {}, formatTime(record.createdAt))] : null,
        record && record.deliveredAt ? [el('dt', {}, 'Delivered'), el('dd', {}, formatTime(record.deliveredAt))] : null,
        record && record.runningAt ? [el('dt', {}, 'Running'), el('dd', {}, formatTime(record.runningAt))] : null,
        record && record.finishedAt ? [el('dt', {}, 'Finished'), el('dd', {}, formatTime(record.finishedAt))] : null,
        record && record.durationMs !== null && record.durationMs !== undefined ? [el('dt', {}, 'Took'), el('dd', {}, record.durationMs + ' ms')] : null,
        record && record.expiresAt && !TERMINAL_STATES.has(record.state) ? [el('dt', {}, 'Expires'), el('dd', {}, formatTime(record.expiresAt))] : null,
        record && record.ok !== null && record.ok !== undefined ? [el('dt', {}, 'Outcome'), el('dd', { id: 'result-outcome' }, badge(record.ok ? 'ok' : 'failed', record.ok ? 'completed' : 'failed'))] : null,
        record && record.result !== null && record.result !== undefined ? [el('dt', {}, 'Result'), el('dd', {}, el('pre', { id: 'result-payload' }, pretty(record.result)))] : null,
        record && record.error ? [el('dt', {}, 'Error'), el('dd', { id: 'result-error', class: 'error' }, record.error)] : null),
      err ? el('div', { class: 'error' }, 'Reading the action failed: ', err.message) : null,
      record && !TERMINAL_STATES.has(record.state) ? el('p', { class: 'muted' }, 'Watching for changes every second…') : null);
    container.append(card);
  };
  draw(null, null);
  const tick = async () => {
    if (stopped) return;
    let record = null;
    try {
      record = await api('GET', '/actions/' + encodeURIComponent(actionId));
    } catch (err) {
      if (stopped) return;
      if (err instanceof ApiError && err.status === 401) {
        signOut('The hub rejected this token.');
        return;
      }
      draw(null, err);
      timer = setTimeout(tick, ACTION_POLL_MS * 3);
      return;
    }
    if (stopped) return;
    draw(record, null);
    if (TERMINAL_STATES.has(record.state)) {
      onSettled();
      return;
    }
    timer = setTimeout(tick, ACTION_POLL_MS);
  };
  tick();
  return () => {
    stopped = true;
    if (timer) clearTimeout(timer);
    onSettled();
  };
}

// ---------------------------------------------------------------------------

window.addEventListener('hashchange', () => { render(); });
render();
