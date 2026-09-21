// Vyshka panel: the management views.
//
// Servers and their credentials, Admin API tokens, webhooks and their
// deliveries, the audit log, and the key/value store, each a thin client over
// /api/v1 and nothing else: every call a control here makes is one an
// operator could make with curl, in the same order, with the same token.
//
// Everything is built with createElement and text nodes, the same rule app.js
// follows, because a webhook URL, a token name, an audit detail, and a stored
// value are all text some other program wrote.
//
// Two surfaces this module calls arrive with the hub lanes of issue #64: the
// webhook PATCH (edit and pause) and the delivery replay of section 11, and
// the key listing of section 12. Until those land, a hub answers them 404 or
// 405, and the views show that answer where they show any other refusal.

import {
  ApiError, actionHref, api, append, attempt, auditHref, badge, clear, confirmation, disclosure,
  el, eventPayloadText, forbidden, formatTime, go, holdSecret, isForbidden, kvHref,
  kvNamespaceHref, linesOf, mountSecret, problemBox, setCrumbs, setTeardown,
  signOut, stale, statusKind, summarizeEventData, token, webhookHref, webhooksHref,
} from './lib.js';

// The deliveries table refreshes on the same cadence as the event feed and
// the server list, with the same teardown discipline.
const DELIVERY_REFRESH_MS = 5000;
const DELIVERY_PAGE_SIZE = 500;
const AUDIT_PAGE_SIZE = 100;
const KV_PAGE_SIZE = 100;

// The webhook templates section 11.2 defines. A hub that grows another
// refuses an unknown one with bad_request, which the form shows.
const WEBHOOK_TEMPLATES = ['generic-json', 'discord'];

// ---------------------------------------------------------------------------
// Shared bits

function fieldRow(id, label, control, hint) {
  return el('label', { class: 'field', for: id },
    el('span', { class: 'name' }, label),
    control,
    hint ? el('span', { class: 'hint' }, hint) : null);
}

function textInput(id, attrs = {}) {
  return el('input', Object.assign({
    type: 'text', id, name: id, spellcheck: 'false', autocomplete: 'off',
  }, attrs));
}

function numberInput(id, attrs = {}) {
  return el('input', Object.assign({ type: 'number', id, name: id, step: '1' }, attrs));
}

function textArea(id, attrs = {}) {
  return el('textarea', Object.assign({ id, name: id, spellcheck: 'false' }, attrs));
}

// scopeBadges draws one badge per scope, so a long grant list stays readable
// and a narrowed scope is visibly narrower than an unnarrowed one.
function scopeBadges(scopes) {
  const list = Array.isArray(scopes) ? scopes : [];
  if (list.length === 0) return el('span', { class: 'muted' }, 'none');
  return el('span', { class: 'badges' }, list.map((scope) => badge(scope, scope === 'admin' ? 'destructive' : '')));
}

// eventBadges renders a webhook's filter: an empty filter is every type, and
// saying so is not the same as showing nothing.
function eventBadges(events) {
  const list = Array.isArray(events) ? events : [];
  if (list.length === 0) return el('span', { class: 'muted' }, 'every type');
  return el('span', { class: 'badges' }, list.map((type) => badge(type)));
}

function serverNames(ids, names) {
  const list = Array.isArray(ids) ? ids : [];
  if (list.length === 0) return el('span', { class: 'muted' }, 'every server');
  return el('span', { class: 'badges' }, list.map((id) => badge(names.get(id) || id)));
}

// loadServerNames reads the server list for the views that only want labels.
// A token with webhooks:manage but no servers:read cannot have it, which is
// not an error: those views fall back to ids.
async function loadServerNames() {
  try {
    const data = await api('GET', '/servers');
    const servers = Array.isArray(data.servers) ? data.servers : [];
    return { allowed: true, servers, names: new Map(servers.map((server) => [server.id, server.name])) };
  } catch (err) {
    if (isForbidden(err) || (err instanceof ApiError && err.status === 404)) {
      return { allowed: false, servers: [], names: new Map() };
    }
    throw err;
  }
}

// ---------------------------------------------------------------------------
// Servers: registration and credentials
//
// Both forms hand back a one-time secret the hub will not show again, so both
// draw it through mountSecret and leave it on the page until the operator
// navigates away. An answer that arrives after the page has already gone is
// held instead (lib.js), because the server exists on the hub whatever the
// browser was doing when the answer landed; an answer that arrives after the
// session has gone is dropped, which is why every request here records the
// bearer it went out under.

// registerServerForm is the "Register a server" card on the server list. It
// takes the render sequence its page began with, so a late answer or a late
// refusal lands on that page or on nothing at all. onCreated is called after
// a successful POST so the list can refresh.
export function registerServerForm(seq, onCreated) {
  const name = textInput('server-name', { required: true, placeholder: 'Chernarus #1' });
  const game = textInput('server-game', { placeholder: 'dayz' });
  const ttl = numberInput('server-ttl', { min: '1', placeholder: 'hub default' });
  const problem = problemBox('register-server-error');
  const secretSlot = el('div', { id: 'enrollment-token-slot' });
  const submit = el('button', { type: 'submit', class: 'primary', id: 'register-server-submit' }, 'Register');
  const form = el('form', {
    class: 'stack card', id: 'register-server', novalidate: true,
    onsubmit: async (event) => {
      event.preventDefault();
      problem.hide();
      const request = { name: name.value.trim() };
      if (request.name === '') {
        problem.show(new ApiError(0, 'bad_request', 'a name is required'));
        return;
      }
      if (game.value.trim() !== '') request.game = game.value.trim();
      if (ttl.value.trim() !== '') request.enrollmentTokenTtlSeconds = Number(ttl.value);
      // The bearer this request goes out under, read before the await: the
      // answer belongs to this session and to no session that replaces it.
      const owner = token();
      submit.disabled = true;
      try {
        const created = await api('POST', '/servers', request);
        const shown = {
          kind: 'enrollment',
          owner,
          id: 'enrollment-token',
          label: 'Enrollment token for ' + created.server.name,
          secret: created.enrollment.token,
          expiresAt: created.enrollment.expiresAt,
          note: 'Shown once. Give it to the plugin: it enrolls with it, and the hub will not show it again. A fresh one can be minted on the server page.',
        };
        if (stale(seq)) {
          // The page these nodes belong to has gone, and appending to it
          // would show nobody anything. The server is registered and this
          // token is the only copy, so it goes to the held tray instead.
          holdSecret(shown);
          return;
        }
        // The slot can be off the page with the sequence unmoved: the first
        // load of the server list failing clears #app under this form.
        if (!mountSecret(secretSlot, shown)) return;
        name.value = '';
        game.value = '';
        ttl.value = '';
        if (onCreated) onCreated(created.server);
      } catch (err) {
        // A refusal of a request this page made must not sign out the
        // session that replaced it: a 401 answering a token already
        // discarded says nothing about the one signed in now.
        if (stale(seq)) return;
        if (err instanceof ApiError && err.status === 401) {
          signOut('The hub rejected this token.');
          return;
        }
        problem.show(err);
      } finally {
        submit.disabled = false;
      }
    },
  },
  el('h2', {}, 'Register a server'),
  el('p', { class: 'muted' },
    'One POST /api/v1/servers (protocol section 5.1). The answer carries a one-time enrollment token, which is the only time the hub shows it.'),
  fieldRow('server-name', 'Name', name, 'required'),
  fieldRow('server-game', 'Game', game, 'optional: the hub refuses an enrollment that declares another game'),
  fieldRow('server-ttl', 'Enrollment token lifetime (seconds)', ttl, 'optional: the hub decides when this is empty, and may clamp what it is given'),
  problem.node,
  el('div', { class: 'actions-row' }, submit));
  return el('div', {}, form, secretSlot);
}

// serverCredentials is the credential card on a server page: a fresh
// enrollment token, and revocation of the secret the plugin holds.
export function serverCredentials(server, seq, reload) {
  const ttl = numberInput('enrollment-ttl', { min: '1', placeholder: 'hub default' });
  const problem = problemBox('credentials-error');
  const secretSlot = el('div', { id: 'enrollment-token-slot' });
  const issue = el('button', {
    type: 'button', class: 'primary', id: 'new-enrollment-token',
    onclick: async () => {
      problem.hide();
      const owner = token();
      issue.disabled = true;
      try {
        const body = ttl.value.trim() === '' ? {} : { ttlSeconds: Number(ttl.value) };
        const minted = await api('POST', '/servers/' + encodeURIComponent(server.id) + '/enrollment-token', body);
        const shown = {
          kind: 'enrollment',
          owner,
          id: 'enrollment-token',
          label: 'Enrollment token for ' + server.name,
          secret: minted.token,
          expiresAt: minted.expiresAt,
          note: 'Shown once, and any unused earlier token for this server has stopped working (protocol section 5.1).',
        };
        if (stale(seq)) {
          holdSecret(shown);
          return;
        }
        mountSecret(secretSlot, shown);
        // The page is deliberately not reloaded here: a redraw would take
        // this token with it, and it exists nowhere else. Nothing on the
        // record changes until a plugin enrolls with it anyway.
      } catch (err) {
        if (stale(seq)) return;
        if (err instanceof ApiError && err.status === 401) {
          signOut('The hub rejected this token.');
          return;
        }
        problem.show(err);
      } finally {
        issue.disabled = false;
      }
    },
  }, 'New enrollment token');
  const confirm = confirmation('revoke-credentials-confirm',
    'I understand this ends the plugin’s sessions at once and it cannot reconnect without a new enrollment token');
  const revoke = el('button', {
    type: 'button', class: 'danger', id: 'revoke-credentials',
    onclick: async () => {
      problem.hide();
      if (!confirm.checked) {
        problem.show(new ApiError(0, 'confirm', 'tick the confirmation box: revoking credentials disconnects the plugin'));
        return;
      }
      revoke.disabled = true;
      try {
        await api('DELETE', '/servers/' + encodeURIComponent(server.id) + '/credentials');
        // A reload started from a page already replaced would draw over
        // whatever the operator navigated to; the revocation stands either
        // way, and the next read of the record shows it.
        if (stale(seq)) return;
        confirm.box.checked = false;
        if (reload) reload();
      } catch (err) {
        if (stale(seq)) return;
        if (err instanceof ApiError && err.status === 401) {
          signOut('The hub rejected this token.');
          return;
        }
        problem.show(err);
      } finally {
        revoke.disabled = false;
      }
    },
  }, 'Revoke credentials');
  return el('div', {},
    el('div', { class: 'card stack', id: 'credentials' },
      el('h2', {}, 'Credentials'),
      el('p', { class: 'muted' },
        'Credential state is ', badge(server.credentialState || '', server.credentialState),
        '. A new enrollment token invalidates any unused earlier one; revocation kills the server secret and every live session (protocol sections 5.1 and 5.4).'),
      fieldRow('enrollment-ttl', 'Enrollment token lifetime (seconds)', ttl, 'optional'),
      el('div', { class: 'actions-row' }, issue),
      confirm.node,
      el('div', { class: 'actions-row' }, revoke),
      problem.node),
    secretSlot);
}

// ---------------------------------------------------------------------------
// Pinned quick actions
//
// Pins are this browser's, not the hub's: an operator's shortlist on the
// machine they work from, kept in localStorage under vyshka.pins.{serverId}
// as an array of action codes. Nothing about them travels to the hub, so a
// pin is not a grant and does not survive a move to another browser. The
// presets slice (issue #76) may move them into the store.

const PIN_PREFIX = 'vyshka.pins.';

function readPins(serverId) {
  try {
    const raw = localStorage.getItem(PIN_PREFIX + serverId);
    const parsed = raw ? JSON.parse(raw) : [];
    return Array.isArray(parsed) ? parsed.filter((code) => typeof code === 'string' && code !== '') : [];
  } catch {
    // A browser with storage disabled simply has no pins; the page works.
    return [];
  }
}

function writePins(serverId, codes) {
  try {
    localStorage.setItem(PIN_PREFIX + serverId, JSON.stringify(codes));
  } catch {
    // Storage refused (private mode, quota): the pin does not stick, and
    // the page must not break over a convenience.
  }
}

// actionAnchor is the action item itself, the same link the action list has
// always drawn.
function actionAnchor(server, action, player, vehicle, pinnedCopy) {
  return el('a', {
    class: 'action-item',
    href: actionHref(server.id, action.code,
      action.context === 'player' ? player : '',
      action.context === 'vehicle' ? vehicle : ''),
    'data-action-code': action.code,
    'data-pinned': pinnedCopy ? 'true' : null,
  },
  el('span', { class: 'name' }, action.name || action.code),
  el('span', { class: 'code mono' }, action.code),
  el('span', { class: 'spacer' }),
  action.context ? badge(action.context) : null,
  action.danger && action.danger !== 'none' ? badge(action.danger, action.danger) : null);
}

// actionsSection draws the manifest's actions, grouped by namespace, with the
// pinned shortlist above them and a pin toggle on every item.
export function actionsSection(server, manifest, player, vehicle = '') {
  const body = manifest.manifest || {};
  const actions = Array.isArray(body.actions) ? body.actions : [];
  const byCode = new Map();
  for (const action of actions) {
    if (action && typeof action.code === 'string') byCode.set(action.code, action);
  }
  // The action list's own pin buttons are built once and restated on every
  // draw; the pinned section's are built with its rows, which are rebuilt
  // whole, so they are never registered here and never go stale.
  const listButtons = new Map();
  const pinnedList = el('div', { class: 'action-list', id: 'pinned-list' });
  const pinnedSection = el('section', { id: 'pinned', hidden: true },
    el('h2', {}, 'Pinned'),
    el('p', { class: 'muted' },
      'Pinned in this browser only (localStorage), not on the hub, so another browser sees its own shortlist.'),
    pinnedList);

  const toggle = (code) => {
    const pins = readPins(server.id);
    const at = pins.indexOf(code);
    if (at === -1) {
      pins.push(code);
    } else {
      pins.splice(at, 1);
    }
    writePins(server.id, pins);
    draw();
  };
  const setPinState = (button, on) => {
    button.setAttribute('aria-pressed', on ? 'true' : 'false');
    button.textContent = on ? 'Unpin' : 'Pin';
    button.title = on
      ? 'Remove this action from the pinned list in this browser'
      : 'Pin this action to the top of this page, in this browser';
  };
  const pinButton = (code) => el('button', {
    type: 'button', class: 'small pin', 'data-pin': code, 'aria-pressed': 'false',
    onclick: () => toggle(code),
  }, 'Pin');
  const listRow = (action) => {
    const button = pinButton(action.code);
    if (!listButtons.has(action.code)) listButtons.set(action.code, []);
    listButtons.get(action.code).push(button);
    return el('div', { class: 'action-row', 'data-action-row': action.code },
      actionAnchor(server, action, player, vehicle, false), button);
  };

  const draw = () => {
    const pins = readPins(server.id);
    const held = new Set(pins);
    for (const [code, group] of listButtons) {
      for (const button of group) setPinState(button, held.has(code));
    }
    clear(pinnedList);
    for (const code of pins) {
      const button = pinButton(code);
      setPinState(button, true);
      const action = byCode.get(code);
      if (action) {
        pinnedList.append(el('div', { class: 'action-row', 'data-pinned-row': code },
          actionAnchor(server, action, player, vehicle, true), button));
        continue;
      }
      // A pin the manifest no longer declares stays visible rather than
      // vanishing: the operator pinned it, and the reason it cannot be
      // dispatched is the manifest's revision, which the row says.
      pinnedList.append(el('div', { class: 'action-row missing', 'data-pin-missing': code },
        el('span', { class: 'action-item muted' },
          el('span', { class: 'name mono' }, code),
          el('span', { class: 'spacer' }),
          badge('not in manifest revision ' + manifest.revision, 'expired')),
        button));
    }
    pinnedSection.hidden = pins.length === 0;
  };

  const nodes = [pinnedSection,
    el('h2', {}, 'Actions'),
    el('p', { class: 'muted' },
      'Manifest revision ' + manifest.revision + ', published ' + formatTime(manifest.publishedAt),
      body.plugin && body.plugin.name ? ' by ' + body.plugin.name + ' ' + (body.plugin.version || '') : '')];
  if (actions.length === 0) {
    nodes.push(el('p', { class: 'notice' }, 'The manifest declares no actions.'));
    draw();
    return nodes;
  }
  const byNamespace = new Map();
  for (const action of actions) {
    const namespace = action.namespace || '';
    if (!byNamespace.has(namespace)) byNamespace.set(namespace, []);
    byNamespace.get(namespace).push(action);
  }
  for (const [namespace, group] of byNamespace) {
    if (byNamespace.size > 1 || namespace) nodes.push(el('h3', {}, namespace || 'no namespace'));
    nodes.push(el('div', { class: 'action-list' }, group.map((action) => listRow(action))));
  }
  draw();
  return nodes;
}

// ---------------------------------------------------------------------------
// Tokens
//
// The whole view needs `admin` (protocol section 10.4), so a refusal is one
// notice naming that scope rather than one per call.

const EXPIRY_CHOICES = [
  { value: '', label: 'never' },
  { value: '86400', label: '1 day' },
  { value: '604800', label: '7 days' },
  { value: '2592000', label: '30 days' },
  { value: '7776000', label: '90 days' },
  { value: 'custom', label: 'custom (seconds)' },
];

const BUNDLES = [
  { value: '', label: 'custom (edit the scopes below)' },
  { value: 'owner', label: 'Owner' },
  { value: 'moderator', label: 'Moderator' },
  { value: 'event-host', label: 'Event host' },
];

function tokenState(record) {
  if (record.revokedAt) return 'revoked';
  if (record.expiresAt && Date.parse(record.expiresAt) <= Date.now()) return 'expired';
  return 'live';
}

// dispatchPrefix is the scope prefix one manifest action narrows to, so that
// actions:dispatch:{prefix}.* covers that action and no more than it.
//
// A scope pattern is matched against the code, and section 6.1 gives an
// action a `namespace` member for display and token scoping, so that member
// is the prefix whenever the code actually sits under it. Without it, the
// prefix is the code with its last segment removed, not the first segment:
// a code family.child.heal narrowed to family.* would hand out every action
// of every sibling namespace under family. A code with no dot narrows
// nothing, and contributes nothing rather than a prefix matching everything.
function dispatchPrefix(action) {
  const code = typeof action.code === 'string' ? action.code : '';
  if (code === '') return '';
  if (typeof action.namespace === 'string' && action.namespace !== ''
      && code.startsWith(action.namespace + '.')) {
    return action.namespace;
  }
  const dot = code.lastIndexOf('.');
  return dot > 0 ? code.slice(0, dot) : '';
}

// manifestNamespaces reads every stored manifest and collects what a role
// bundle needs to narrow itself: the action prefixes the hub could dispatch,
// and the KV namespaces the plugins declared (section 6.6).
async function manifestNamespaces() {
  const data = await api('GET', '/servers');
  const servers = Array.isArray(data.servers) ? data.servers : [];
  const actions = new Set();
  const kv = new Set();
  for (const server of servers) {
    const manifest = await api('GET', '/servers/' + encodeURIComponent(server.id) + '/manifest')
      .catch((err) => {
        // No manifest yet is the ordinary case for a server nobody has
        // enrolled; it narrows nothing and is not a failure.
        if (err instanceof ApiError && err.status === 404) return null;
        throw err;
      });
    if (!manifest) continue;
    const body = manifest.manifest || {};
    for (const action of Array.isArray(body.actions) ? body.actions : []) {
      if (!action || typeof action.code !== 'string') continue;
      const prefix = dispatchPrefix(action);
      if (prefix !== '') actions.add(prefix);
    }
    for (const namespace of Array.isArray(body.kvNamespaces) ? body.kvNamespaces : []) {
      if (typeof namespace === 'string' && namespace !== '') kv.add(namespace);
    }
  }
  return { actions: [...actions].sort(), kv: [...kv].sort() };
}

// bundleScopes expands a role bundle. Bundles are a panel convenience and no
// part of the protocol: the hub sees only the scope list the operator sent,
// and the operator sees that list before it is sent.
function bundleScopes(bundle, namespaces) {
  if (bundle === 'owner') return ['admin'];
  if (bundle !== 'moderator' && bundle !== 'event-host') return null;
  const scopes = ['servers:read', 'events:read'];
  if (namespaces.actions.length === 0) {
    scopes.push('actions:dispatch');
  } else {
    for (const namespace of namespaces.actions) scopes.push('actions:dispatch:' + namespace + '.*');
  }
  if (bundle === 'event-host') {
    for (const namespace of namespaces.kv) scopes.push('kv:rw:' + namespace);
  }
  return scopes;
}

export async function viewTokens(app, route, seq) {
  setCrumbs([{ label: 'Tokens' }]);
  let data;
  try {
    data = await api('GET', '/tokens');
  } catch (err) {
    if (stale(seq)) return;
    if (isForbidden(err)) {
      forbidden(app, 'admin', err, 'GET /api/v1/tokens');
      return;
    }
    throw err;
  }
  if (stale(seq)) return;
  // The server list names the binding column and fills the picker; a token
  // with admin can always read it, so a failure here is a real one.
  const serverList = await loadServerNames();
  if (stale(seq)) return;
  clear(app);
  app.append(el('h1', {}, 'Admin API tokens'));

  const problem = problemBox('tokens-error');
  const secretSlot = el('div', { id: 'token-secret-slot' });
  const tbody = el('tbody', {});
  const table = el('table', { id: 'tokens' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Name'), el('th', {}, 'Scopes'), el('th', {}, 'Servers'), el('th', {}, 'State'),
      el('th', {}, 'Created'), el('th', {}, 'Expires'), el('th', {}, 'Id'), el('th', {}, ''))),
    tbody);
  const empty = el('p', { class: 'notice', id: 'tokens-empty', hidden: true },
    'No minted tokens. The hub is being reached with its configured bootstrap credential (protocol section 10.6), which has no record and cannot be revoked through the API.');

  const reload = async () => {
    const latest = await api('GET', '/tokens');
    if (stale(seq)) return;
    drawTokens(latest.tokens || []);
  };
  // guarded is the one place a control's failure is turned into a notice: a
  // rejected token signs out, anything else lands beside the controls that
  // produced it, and a view already replaced draws nothing at all.
  const guarded = (run) => async (...args) => {
    problem.hide();
    try {
      await run(...args);
    } catch (err) {
      if (stale(seq)) return;
      if (err instanceof ApiError && err.status === 401) {
        signOut('The hub rejected this token.');
        return;
      }
      problem.show(err);
    }
  };

  function revokeCell(record) {
    if (tokenState(record) === 'revoked') return el('span', { class: 'muted' }, 'revoked ' + formatTime(record.revokedAt));
    const cell = el('span', { class: 'revoke-cell' });
    const ask = el('button', {
      type: 'button', class: 'small danger', 'data-revoke-token': record.id,
      onclick: () => {
        clear(cell);
        cell.append(
          el('button', {
            type: 'button', class: 'small danger', 'data-revoke-confirm': record.id,
            onclick: guarded(async () => {
              await api('DELETE', '/tokens/' + encodeURIComponent(record.id));
              await reload();
            }),
          }, 'Confirm'),
          ' ',
          el('button', {
            type: 'button', class: 'small', 'data-revoke-cancel': record.id,
            onclick: () => { clear(cell); cell.append(ask); },
          }, 'Cancel'));
      },
    }, 'Revoke');
    cell.append(ask);
    return cell;
  }

  function drawTokens(records) {
    clear(tbody);
    for (const record of records) {
      const state = tokenState(record);
      tbody.append(el('tr', { 'data-token-id': record.id, 'data-token-state': state, class: state === 'live' ? '' : 'dim' },
        el('td', {}, record.name || el('span', { class: 'muted' }, 'unnamed')),
        el('td', {}, scopeBadges(record.scopes)),
        el('td', {}, serverNames(record.servers, serverList.names)),
        el('td', {}, badge(state, state === 'live' ? 'up' : state)),
        el('td', {}, formatTime(record.createdAt)),
        el('td', {}, record.expiresAt ? formatTime(record.expiresAt) : el('span', { class: 'muted' }, 'never')),
        el('td', { class: 'mono' }, record.id),
        el('td', { class: 'row-actions' }, revokeCell(record))));
    }
    table.hidden = records.length === 0;
    empty.hidden = records.length > 0;
  }

  // The mint form.
  const name = textInput('token-name', { required: true, placeholder: 'panel' });
  const bundle = el('select', { id: 'token-bundle', name: 'bundle' },
    BUNDLES.map((choice) => el('option', { value: choice.value }, choice.label)));
  const scopes = textArea('token-scopes', { placeholder: 'servers:read\nevents:read' });
  const expiry = el('select', { id: 'token-expiry', name: 'expiry' },
    EXPIRY_CHOICES.map((choice) => el('option', { value: choice.value }, choice.label)));
  const customExpiry = numberInput('token-expiry-seconds', { min: '1', placeholder: 'seconds', hidden: true });
  const binding = serverPicker('token-servers', serverList.servers, serverList.allowed, [],
    'none ticked mints an unbound token, whose grants apply to every server; ticked servers bind every grant to those servers (protocol section 10.1). A bound token cannot carry admin or webhooks:manage, and a kv:rw grant stays installation-wide');
  const warning = el('p', { class: 'notice danger', id: 'dispatch-warning', hidden: true },
    'This list holds an unnarrowed actions:dispatch, which can dispatch anything any plugin declares, on every server (protocol section 10.1 asks a UI to warn). Narrow it to {namespace}.* unless you mean it.');
  const bundleNote = el('p', { class: 'muted', id: 'bundle-note' },
    'A bundle fills the scopes below; they stay editable, and the panel sends exactly what is there.');
  const submit = el('button', { type: 'submit', class: 'primary', id: 'mint-token-submit' }, 'Mint token');

  let namespaces = null;
  // scopesEdited says whether the operator has typed in the scope list since
  // the bundle last changed. Enumerating the manifests takes as many calls as
  // there are servers, and an operator who gives up on a slow expansion,
  // picks custom, and types a list must not have it overwritten when that
  // enumeration finally lands.
  let scopesEdited = false;
  // Every enumeration takes a serial, and only the one started last may write
  // the cache or the scope list. An enumeration is as many calls as there are
  // servers and any one of them can stall, so a later enumeration can finish
  // first, fill the cache with newer manifests, and have that cache expanded
  // into the textarea the operator is reading. The stalled one landing after
  // that carries the older, wider list, and "is this still the chosen bundle"
  // cannot tell the two apart when the choice has come back round to the same
  // one. An enumeration overtaken by a later one is dropped whole.
  let enumeration = 0;
  const applyBundle = async () => {
    const chosen = bundle.value;
    if (chosen === '') {
      bundleNote.textContent = 'Custom: type the scopes to grant, one per line.';
      return;
    }
    if (chosen !== 'owner' && namespaces === null) {
      bundleNote.textContent = 'Reading the stored manifests to narrow the dispatch and key/value scopes…';
      enumeration++;
      const mine = enumeration;
      const read = await manifestNamespaces();
      if (stale(seq)) return;
      if (mine !== enumeration) return;
      // The read is kept whatever became of the choice that started it: it
      // is the hub's answer, not this expansion's, and the next bundle
      // change spends it without asking again.
      namespaces = read;
      if (bundle.value !== chosen || scopesEdited) return;
    }
    const expanded = bundleScopes(chosen, namespaces || { actions: [], kv: [] });
    scopes.value = expanded.join('\n');
    warning.hidden = !expanded.includes('actions:dispatch');
    bundleNote.textContent = chosen === 'owner'
      ? 'Owner is the admin scope, which implies every other scope, token management included (protocol section 10.1).'
      : 'Expanded from the stored manifests: ' +
        (namespaces.actions.length === 0 ? 'no manifest declares an action namespace' : namespaces.actions.join(', ')) +
        (chosen === 'event-host'
          ? '; key/value namespaces: ' + (namespaces.kv.length === 0 ? 'none declared' : namespaces.kv.join(', '))
          : '') +
        '. Edit the list before minting; what is sent is what is shown.';
  };
  bundle.addEventListener('change', guarded(async () => {
    // Choosing a bundle is the operator asking for its list, so whatever
    // they had typed before is theirs to lose from here.
    scopesEdited = false;
    await applyBundle();
  }));
  expiry.addEventListener('change', () => { customExpiry.hidden = expiry.value !== 'custom'; });
  scopes.addEventListener('input', () => {
    // Only a human typing raises this: applyBundle assigns to value, which
    // fires no input event.
    scopesEdited = true;
    warning.hidden = !linesOf(scopes).includes('actions:dispatch');
  });

  const form = el('form', {
    class: 'stack card', id: 'mint-token', novalidate: true,
    onsubmit: guarded(async (event) => {
      event.preventDefault();
      const request = { name: name.value.trim(), scopes: linesOf(scopes) };
      const servers = binding.read();
      if (servers.length > 0) request.servers = servers;
      if (request.name === '') {
        problem.show(new ApiError(0, 'bad_request', 'a name is required'));
        return;
      }
      if (request.scopes.length === 0) {
        problem.show(new ApiError(0, 'bad_request', 'scopes must not be empty (protocol section 10.4)'));
        return;
      }
      if (expiry.value === 'custom') {
        if (customExpiry.value.trim() === '') {
          problem.show(new ApiError(0, 'bad_request', 'type the lifetime in seconds, or choose another expiry'));
          return;
        }
        request.expiresInSeconds = Number(customExpiry.value);
      } else if (expiry.value !== '') {
        request.expiresInSeconds = Number(expiry.value);
      }
      const owner = token();
      submit.disabled = true;
      try {
        const minted = await api('POST', '/tokens', request);
        const shown = {
          kind: 'token',
          owner,
          id: 'token-secret',
          label: 'Secret for ' + (minted.token.name || 'the new token'),
          secret: minted.secret,
          expiresAt: minted.token.expiresAt,
          note: 'Shown once. The hub stores a digest, so no later call can retrieve it (protocol section 10.4). Scopes: ' +
            (minted.token.scopes || []).join(', ') + '. Servers: ' +
            ((minted.token.servers || []).length === 0 ? 'every server (unbound)' : (minted.token.servers || []).map((id) => serverList.names.get(id) || id).join(', ')) + '.',
        };
        if (stale(seq)) {
          // The token is minted and live. Dropping this answer with the
          // page would leave a credential nobody can use and nobody saw.
          holdSecret(shown);
          return;
        }
        if (!mountSecret(secretSlot, shown)) return;
        name.value = '';
        await reload();
      } finally {
        submit.disabled = false;
      }
    }),
  },
  el('h2', {}, 'Mint a token'),
  fieldRow('token-name', 'Name', name, 'required: it is what the audit log records for every mutation this token makes'),
  fieldRow('token-bundle', 'Role bundle', bundle, 'a panel convenience, not protocol: it only fills the scope list'),
  bundleNote,
  fieldRow('token-scopes', 'Scopes', scopes, 'one per line, in the grammar of protocol section 10.1'),
  warning,
  binding.node,
  fieldRow('token-expiry', 'Expires', expiry, 'a hub may clamp a very short lifetime up to its floor'),
  customExpiry,
  el('div', { class: 'actions-row' }, submit));

  app.append(
    el('p', { class: 'muted' },
      'Every Admin API credential and what it may do (protocol section 10.4). Revoked and expired records stay listed, because the audit log points at them.'),
    problem.node, secretSlot, table, empty, form);
  drawTokens(data.tokens || []);
}

// ---------------------------------------------------------------------------
// Webhooks

// serverPicker is the server list as checkboxes, shared by the webhook filter
// and the token binding; the hint says what an empty pick means for each.
function serverPicker(id, servers, allowed, selected, hint) {
  const explain = hint || 'none ticked means every server, now and later; a non-empty list needs servers:read on the registering token (protocol section 11.2)';
  if (!allowed) {
    const area = textArea(id, { placeholder: 'one server id per line, empty for every server' });
    area.value = (selected || []).join('\n');
    return {
      node: fieldRow(id, 'Server ids', area,
        'this token cannot read the server list (servers:read), so ids are typed; ' + explain),
      read: () => linesOf(area),
    };
  }
  const held = new Set(selected || []);
  const boxes = servers.map((server) => {
    const box = el('input', { type: 'checkbox', 'data-server-id': server.id });
    box.checked = held.has(server.id);
    return { box, server };
  });
  const node = el('div', { class: 'field', id: id },
    el('span', { class: 'name' }, 'Servers'),
    el('div', { class: 'checklist' }, boxes.map(({ box, server }) => el('label', { class: 'check' },
      box, ' ', server.name, ' ', el('span', { class: 'mono muted' }, server.id)))),
    el('span', { class: 'hint' }, explain));
  return { node, read: () => boxes.filter(({ box }) => box.checked).map(({ server }) => server.id) };
}

function templateSelect(id, value) {
  const select = el('select', { id, name: id }, WEBHOOK_TEMPLATES.map((name) => el('option', { value: name }, name)));
  select.value = WEBHOOK_TEMPLATES.includes(value) ? value : 'generic-json';
  return select;
}

export async function viewWebhooks(app, route, seq) {
  setCrumbs([{ label: 'Webhooks' }]);
  let data;
  try {
    data = await api('GET', '/webhooks');
  } catch (err) {
    if (stale(seq)) return;
    if (isForbidden(err)) {
      forbidden(app, 'webhooks:manage', err, 'GET /api/v1/webhooks');
      return;
    }
    throw err;
  }
  const servers = await loadServerNames();
  if (stale(seq)) return;
  clear(app);
  app.append(el('h1', {}, 'Webhooks'));

  const problem = problemBox('webhooks-error');
  const secretSlot = el('div', { id: 'webhook-secret-slot' });
  const tbody = el('tbody', {});
  const table = el('table', { id: 'webhooks' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'URL'), el('th', {}, 'Events'), el('th', {}, 'Servers'),
      el('th', {}, 'Template'), el('th', {}, 'State'), el('th', {}, 'Created'))),
    tbody);
  const empty = el('p', { class: 'notice', id: 'webhooks-empty', hidden: true },
    'No webhooks. A webhook exports what its filter matches as signed POSTs, and only what lands after it is registered (protocol section 11.2).');

  const drawWebhooks = (records) => {
    clear(tbody);
    for (const record of records) {
      tbody.append(el('tr', {
        class: 'row-link', 'data-webhook-id': record.id, 'data-paused': record.pausedAt ? 'true' : 'false',
        onclick: () => { go(webhookHref(record.id)); },
      },
      el('td', {}, el('a', { href: webhookHref(record.id) }, record.url)),
      el('td', {}, eventBadges(record.events)),
      el('td', {}, serverNames(record.serverIds, servers.names)),
      el('td', {}, record.template || 'generic-json'),
      el('td', {}, record.pausedAt ? badge('paused', 'expired') : badge('active', 'up')),
      el('td', {}, formatTime(record.createdAt))));
    }
    table.hidden = records.length === 0;
    empty.hidden = records.length > 0;
  };

  const url = textInput('webhook-url', { required: true, placeholder: 'https://example.net/hooks/vyshka', inputmode: 'url' });
  const events = textArea('webhook-events', { placeholder: 'core.player.*\naction.completed' });
  const picker = serverPicker('webhook-servers', servers.servers, servers.allowed, []);
  const template = templateSelect('webhook-template', 'generic-json');
  const submit = el('button', { type: 'submit', class: 'primary', id: 'register-webhook-submit' }, 'Register');
  const form = el('form', {
    class: 'stack card', id: 'register-webhook', novalidate: true,
    onsubmit: async (event) => {
      event.preventDefault();
      problem.hide();
      if (url.value.trim() === '') {
        problem.show(new ApiError(0, 'bad_request', 'a url is required, http or https'));
        return;
      }
      const owner = token();
      submit.disabled = true;
      try {
        const created = await api('POST', '/webhooks', {
          url: url.value.trim(),
          events: linesOf(events),
          serverIds: picker.read(),
          template: template.value,
        });
        const shown = {
          kind: 'webhook',
          owner,
          id: 'webhook-secret',
          label: 'Signing secret for ' + created.webhook.url,
          secret: created.secret,
          note: 'Shown once. The hub must keep the secret itself to sign with it, so read access to its database is read access to this value (protocol section 11.2).',
        };
        if (stale(seq)) {
          // The webhook is registered and already matching. Without this
          // value nothing can verify a delivery's signature.
          holdSecret(shown);
          return;
        }
        if (!mountSecret(secretSlot, shown)) return;
        url.value = '';
        events.value = '';
        const latest = await api('GET', '/webhooks');
        if (stale(seq)) return;
        drawWebhooks(latest.webhooks || []);
      } catch (err) {
        if (stale(seq)) return;
        if (err instanceof ApiError && err.status === 401) {
          signOut('The hub rejected this token.');
          return;
        }
        problem.show(err);
      } finally {
        submit.disabled = false;
      }
    },
  },
  el('h2', {}, 'Register a webhook'),
  el('p', { class: 'muted' },
    'Registration needs grants covering what the filter subscribes to, or webhooks:manage would quietly be an installation-wide read grant (protocol section 11.2).'),
  fieldRow('webhook-url', 'URL', url, 'required, http or https'),
  fieldRow('webhook-events', 'Events', events, 'one pattern per line, empty for every type; patterns follow protocol section 10.1'),
  picker.node,
  fieldRow('webhook-template', 'Template', template, 'generic-json is the shape of protocol section 11.3; discord renders the Discord shape'),
  el('div', { class: 'actions-row' }, submit));

  app.append(problem.node, secretSlot, table, empty, form);
  drawWebhooks(data.webhooks || []);
}

function deliveryStateBadge(state) {
  if (state === 'delivered') return badge('delivered', 'completed');
  if (state === 'dead') return badge('dead', 'failed');
  return badge(state || 'pending', 'pending');
}

export async function viewWebhook(app, route, seq) {
  let data;
  try {
    data = await api('GET', '/webhooks');
  } catch (err) {
    if (stale(seq)) return;
    if (isForbidden(err)) {
      forbidden(app, 'webhooks:manage', err, 'GET /api/v1/webhooks');
      return;
    }
    throw err;
  }
  if (stale(seq)) return;
  const record = (data.webhooks || []).find((entry) => entry && entry.id === route.webhookId);
  setCrumbs([{ label: 'Webhooks', href: webhooksHref() }, { label: record ? record.url : route.webhookId }]);
  clear(app);
  if (!record) {
    // The Admin API has no read of a single webhook (protocol section 11.2
    // lists the collection, the delete, and the deliveries), so the page
    // finds its record in the list, and a missing one is simply gone.
    app.append(el('div', { class: 'error', id: 'webhook-missing', role: 'alert' },
      'No webhook with id ', el('span', { class: 'mono' }, route.webhookId),
      ' is registered on this hub.'));
    return;
  }
  const servers = await loadServerNames();
  if (stale(seq)) return;

  const problem = problemBox('webhook-error');
  const summary = el('dl', { class: 'kv', id: 'webhook-summary' });
  const drawSummary = (current) => {
    clear(summary);
    append(summary, [
      el('dt', {}, 'URL'), el('dd', {}, current.url),
      el('dt', {}, 'Events'), el('dd', {}, eventBadges(current.events)),
      el('dt', {}, 'Servers'), el('dd', {}, serverNames(current.serverIds, servers.names)),
      el('dt', {}, 'Template'), el('dd', {}, current.template || 'generic-json'),
      el('dt', {}, 'State'), el('dd', { id: 'webhook-state' }, current.pausedAt
        ? [badge('paused', 'expired'), ' since ' + formatTime(current.pausedAt)]
        : badge('active', 'up')),
      el('dt', {}, 'Created'), el('dd', {}, formatTime(current.createdAt)),
      el('dt', {}, 'Webhook id'), el('dd', { class: 'mono' }, current.id),
    ]);
  };

  let current = record;
  const refreshRecord = async () => {
    const latest = await api('GET', '/webhooks');
    if (stale(seq)) return;
    const found = (latest.webhooks || []).find((entry) => entry && entry.id === current.id);
    if (found) {
      current = found;
      drawSummary(current);
      pause.textContent = current.pausedAt ? 'Resume' : 'Pause';
      pause.setAttribute('aria-pressed', current.pausedAt ? 'true' : 'false');
    }
  };
  const guarded = (run) => async (...args) => {
    problem.hide();
    try {
      await run(...args);
    } catch (err) {
      if (stale(seq)) return;
      if (err instanceof ApiError && err.status === 401) {
        signOut('The hub rejected this token.');
        return;
      }
      problem.show(err);
    }
  };

  const pause = el('button', {
    type: 'button', class: 'primary', id: 'toggle-pause', 'aria-pressed': current.pausedAt ? 'true' : 'false',
    onclick: guarded(async () => {
      pause.disabled = true;
      try {
        await api('PATCH', '/webhooks/' + encodeURIComponent(current.id), { paused: !current.pausedAt });
        await refreshRecord();
      } finally {
        pause.disabled = false;
      }
    }),
  }, current.pausedAt ? 'Resume' : 'Pause');

  const editUrl = textInput('edit-webhook-url', { required: true });
  editUrl.value = current.url;
  const editEvents = textArea('edit-webhook-events');
  editEvents.value = (current.events || []).join('\n');
  const editPicker = serverPicker('edit-webhook-servers', servers.servers, servers.allowed, current.serverIds || []);
  const editTemplate = templateSelect('edit-webhook-template', current.template);
  const editSubmit = el('button', { type: 'submit', class: 'primary', id: 'edit-webhook-submit' }, 'Save changes');
  const editForm = el('form', {
    class: 'stack card', id: 'edit-webhook', novalidate: true,
    onsubmit: guarded(async (event) => {
      event.preventDefault();
      editSubmit.disabled = true;
      try {
        await api('PATCH', '/webhooks/' + encodeURIComponent(current.id), {
          url: editUrl.value.trim(),
          events: linesOf(editEvents),
          serverIds: editPicker.read(),
          template: editTemplate.value,
        });
        await refreshRecord();
      } finally {
        editSubmit.disabled = false;
      }
    }),
  },
  el('h2', {}, 'Edit'),
  el('p', { class: 'muted' },
    'A present member replaces that field whole. The secret is never rotated by an edit. A delivery already queued keeps the body it was rendered with, and goes to the new URL on its next attempt.'),
  fieldRow('edit-webhook-url', 'URL', editUrl),
  fieldRow('edit-webhook-events', 'Events', editEvents, 'one pattern per line, empty for every type'),
  editPicker.node,
  fieldRow('edit-webhook-template', 'Template', editTemplate),
  el('div', { class: 'actions-row' }, editSubmit));

  const deleteConfirm = confirmation('delete-webhook-confirm',
    'I understand this deletes the webhook and abandons its pending deliveries');
  const deleteButton = el('button', {
    type: 'button', class: 'danger', id: 'delete-webhook',
    onclick: guarded(async () => {
      if (!deleteConfirm.checked) {
        problem.show(new ApiError(0, 'confirm', 'tick the confirmation box: deleting a webhook abandons its pending deliveries'));
        return;
      }
      deleteButton.disabled = true;
      try {
        await api('DELETE', '/webhooks/' + encodeURIComponent(current.id));
        go(webhooksHref());
      } finally {
        deleteButton.disabled = false;
      }
    }),
  }, 'Delete webhook');

  // Deliveries.
  const deadOnly = el('input', { type: 'checkbox', id: 'dead-only' });
  const status = el('p', { class: 'muted', id: 'deliveries-status' }, 'Loading…');
  const tbody = el('tbody', {});
  const table = el('table', { id: 'deliveries', hidden: true },
    el('thead', {}, el('tr', {},
      el('th', {}, 'State'), el('th', {}, 'Type'), el('th', {}, 'Server'), el('th', {}, 'Attempts'),
      el('th', {}, 'Last result'), el('th', {}, 'Created'), el('th', {}, 'Next attempt'),
      el('th', {}, 'Delivered'), el('th', {}, ''))),
    tbody);
  const empty = el('p', { class: 'notice', id: 'deliveries-empty', hidden: true },
    'No deliveries yet. A webhook observes only what lands after it is registered.');

  let deliveries = [];
  const drawDeliveries = () => {
    const shown = deadOnly.checked ? deliveries.filter((entry) => entry.state === 'dead') : deliveries;
    clear(tbody);
    for (const delivery of shown) {
      const replay = el('button', {
        type: 'button', class: 'small', 'data-replay': delivery.id,
        onclick: guarded(async () => {
          replay.disabled = true;
          try {
            await api('POST', '/webhooks/' + encodeURIComponent(current.id) +
              '/deliveries/' + encodeURIComponent(delivery.id) + '/replay');
            await loadDeliveries();
          } finally {
            replay.disabled = false;
          }
        }),
      }, 'Replay');
      tbody.append(el('tr', { 'data-delivery-id': delivery.id, 'data-delivery-state': delivery.state },
        el('td', {}, deliveryStateBadge(delivery.state)),
        el('td', { class: 'mono' }, delivery.type || ''),
        el('td', {}, delivery.serverId
          ? (servers.names.get(delivery.serverId) || el('span', { class: 'mono' }, delivery.serverId))
          : el('span', { class: 'muted' }, 'none')),
        el('td', {}, String(delivery.attempts === undefined ? 0 : delivery.attempts)),
        el('td', { class: 'last-result' },
          delivery.lastStatus === undefined || delivery.lastStatus === null
            ? el('span', { class: 'muted' }, 'no status')
            : badge(String(delivery.lastStatus), statusKind(delivery.lastStatus)),
          delivery.lastError ? el('span', { class: 'muted' }, ' ' + delivery.lastError) : null),
        el('td', {}, formatTime(delivery.createdAt)),
        el('td', {}, delivery.nextAttemptAt ? formatTime(delivery.nextAttemptAt) : el('span', { class: 'muted' }, 'none')),
        el('td', {}, delivery.deliveredAt ? formatTime(delivery.deliveredAt) : el('span', { class: 'muted' }, 'never')),
        el('td', { class: 'row-actions' }, replay)));
    }
    table.hidden = shown.length === 0;
    empty.hidden = shown.length > 0;
    const dead = deliveries.filter((entry) => entry.state === 'dead').length;
    status.textContent = deliveries.length + ' deliver' + (deliveries.length === 1 ? 'y' : 'ies') +
      ' held, ' + dead + ' dead' + (deadOnly.checked ? ', showing dead only' : '') +
      '. Newest first, refreshed every ' + (DELIVERY_REFRESH_MS / 1000) + ' s.';
  };
  const loadDeliveries = async () => {
    const page = await api('GET', '/webhooks/' + encodeURIComponent(current.id) +
      '/deliveries?limit=' + DELIVERY_PAGE_SIZE);
    if (stale(seq)) return;
    deliveries = Array.isArray(page.deliveries) ? page.deliveries : [];
    problem.hide();
    drawDeliveries();
  };
  deadOnly.addEventListener('change', drawDeliveries);

  drawSummary(current);
  app.append(
    el('h1', {}, 'Webhook ', el('span', { class: 'muted title-tail' }, current.url)),
    el('div', { class: 'card' }, summary, el('div', { class: 'actions-row' }, pause)),
    problem.node,
    editForm,
    el('div', { class: 'card stack', id: 'delete-webhook-card' },
      el('h2', {}, 'Delete'), deleteConfirm.node, el('div', { class: 'actions-row' }, deleteButton)),
    el('h2', {}, 'Deliveries'),
    el('label', { class: 'field inline', id: 'dead-only-field' }, deadOnly,
      el('span', { class: 'name' }, 'Dead letter only')),
    status, empty, table);

  try {
    await loadDeliveries();
  } catch (err) {
    if (stale(seq)) return;
    if (err instanceof ApiError && err.status === 401) throw err;
    status.textContent = 'The deliveries could not be read.';
    problem.show(err);
  }
  if (stale(seq)) return;
  let inFlight = false;
  const timer = setInterval(async () => {
    if (inFlight || document.hidden) return;
    inFlight = true;
    try {
      await loadDeliveries();
    } catch (err) {
      if (stale(seq)) return;
      if (err instanceof ApiError && err.status === 401) {
        signOut('The hub rejected this token.');
        return;
      }
      problem.show(err);
    } finally {
      inFlight = false;
    }
  }, DELIVERY_REFRESH_MS);
  setTeardown(() => clearInterval(timer));
}

// ---------------------------------------------------------------------------
// Audit
//
// Every authenticated Admin API mutation, refusals included (protocol section
// 10.5). The filters live in the route, so a shared link carries them.

// toLocalInput puts an instant from the route into a datetime-local field.
// The field's seconds are shown (step 1 below), so a boundary the operator
// can see is a boundary they can edit; milliseconds have nowhere to go, which
// is why an untouched field is never re-encoded (boundaryFor below).
function toLocalInput(iso) {
  if (!iso) return '';
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '';
  const pad = (value) => String(value).padStart(2, '0');
  return date.getFullYear() + '-' + pad(date.getMonth() + 1) + '-' + pad(date.getDate()) +
    'T' + pad(date.getHours()) + ':' + pad(date.getMinutes()) + ':' + pad(date.getSeconds());
}

function fromLocalInput(value) {
  if (!value) return '';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '';
  return date.toISOString();
}

// boundaryFor decides what one timestamp filter sends when Apply is clicked.
// A field the operator did not touch goes back into the route exactly as it
// arrived: the field cannot hold milliseconds, and a route naming
// 12:00:30.500Z re-encoded from an untouched field would quietly become
// 12:00:30.000Z and move the boundary the operator was looking at. Only a
// field they changed is read back out of the control. The comparison is
// against what the control actually held after being populated, because the
// browser normalises the value it is given.
//
// A route value the browser could not parse populates nothing, so it survives
// Apply unchanged and the hub keeps refusing it where the refusal is visible;
// Clear is what drops it.
function boundaryFor(input, populated, original) {
  return input.value === populated ? (original || '') : fromLocalInput(input.value);
}

export async function viewAudit(app, route, seq) {
  setCrumbs([{ label: 'Audit' }]);
  const filters = route.filters;
  const query = new URLSearchParams();
  for (const key of ['tokenId', 'serverId', 'since', 'until']) {
    if (filters[key]) query.set(key, filters[key]);
  }
  query.set('limit', String(AUDIT_PAGE_SIZE));

  let page;
  try {
    page = await api('GET', '/audit?' + query.toString());
  } catch (err) {
    if (stale(seq)) return;
    if (isForbidden(err)) {
      forbidden(app, 'admin', err, 'GET /api/v1/audit');
      return;
    }
    if (!(err instanceof ApiError) || err.status === 401 || err.status === 0) throw err;
    // A malformed since/until in a shared link is the hub's to refuse, and
    // the filter form must stay on the page so it can be corrected.
    page = { records: [], problem: err };
  }
  if (stale(seq)) return;
  const [tokens, servers] = await Promise.all([
    api('GET', '/tokens').catch((err) => (isForbidden(err) ? { tokens: [] } : Promise.reject(err))),
    loadServerNames(),
  ]);
  if (stale(seq)) return;
  clear(app);

  const problem = problemBox('audit-error');
  const tokenNames = new Map((tokens.tokens || []).map((record) => [record.id, record.name]));
  const tokenInput = textInput('audit-token', { list: 'audit-token-list', placeholder: 'token id' });
  tokenInput.value = filters.tokenId || '';
  const tokenList = el('datalist', { id: 'audit-token-list' },
    (tokens.tokens || []).map((record) => el('option', { value: record.id }, record.name || record.id)));
  const serverSelect = el('select', { id: 'audit-server', name: 'serverId' },
    el('option', { value: '' }, 'every server'),
    servers.servers.map((server) => el('option', { value: server.id }, server.name)));
  serverSelect.value = filters.serverId || '';
  // A route can name a server this list does not hold: one deleted, or one
  // this token cannot read. Assigning it to a select with no matching option
  // leaves the select empty, and Apply would then silently widen the filter
  // the operator is looking at from one server to every server. The id gets
  // an option of its own so it round-trips untouched, the same discipline the
  // timestamp boundaries keep.
  if (filters.serverId && serverSelect.value !== filters.serverId) {
    serverSelect.append(el('option', {
      value: filters.serverId, class: 'mono', 'data-unlisted-server': filters.serverId,
    }, filters.serverId));
    serverSelect.value = filters.serverId;
  }
  const since = el('input', { type: 'datetime-local', id: 'audit-since', name: 'since', step: '1' });
  since.value = toLocalInput(filters.since);
  // What the control kept, not what it was given: an untouched field is one
  // whose value still reads back as this.
  const sincePopulated = since.value;
  const until = el('input', { type: 'datetime-local', id: 'audit-until', name: 'until', step: '1' });
  until.value = toLocalInput(filters.until);
  const untilPopulated = until.value;
  const apply = el('button', { type: 'submit', class: 'small primary', id: 'audit-apply' }, 'Apply');
  const reset = el('button', {
    type: 'button', class: 'small', id: 'audit-clear',
    onclick: () => { go(auditHref({})); },
  }, 'Clear');
  const filterForm = el('form', {
    class: 'card feed-controls', id: 'audit-filters', novalidate: true,
    onsubmit: (event) => {
      event.preventDefault();
      go(auditHref({
        tokenId: tokenInput.value.trim(),
        serverId: serverSelect.value,
        since: boundaryFor(since, sincePopulated, filters.since),
        until: boundaryFor(until, untilPopulated, filters.until),
      }));
    },
  },
  el('div', { class: 'filter-grid' },
    fieldRow('audit-token', 'Token id', el('span', {}, tokenInput, tokenList), 'exact id; the list suggests the tokens this hub holds'),
    fieldRow('audit-server', 'Server', serverSelect, 'records the hub attributed to one server'),
    fieldRow('audit-since', 'Since', since, 'inclusive, sent as UTC; a boundary left untouched is sent back exactly as the link carried it'),
    fieldRow('audit-until', 'Until', until, 'exclusive, sent as UTC; a boundary left untouched is sent back exactly as the link carried it')),
  el('div', { class: 'actions-row' }, apply, reset));

  const status = el('p', { class: 'muted', id: 'audit-status' }, 'Loading…');
  const tbody = el('tbody', {});
  const table = el('table', { id: 'audit', hidden: true },
    el('thead', {}, el('tr', {},
      el('th', {}, 'At'), el('th', {}, 'Token'), el('th', {}, 'Request'), el('th', {}, 'Status'),
      el('th', {}, 'Source'), el('th', {}, 'Server'), el('th', {}, 'Detail'))),
    tbody);
  const empty = el('p', { class: 'notice', id: 'audit-empty', hidden: true },
    'No records match. Reads are never audited: only mutations are, and only authenticated ones (protocol section 10.5).');
  const older = el('button', { type: 'button', id: 'audit-older', hidden: true }, 'Load older');

  let cursor = null;
  let shown = 0;
  const drawRecords = (records) => {
    for (const record of records) {
      const detail = record.detail && typeof record.detail === 'object' ? record.detail : {};
      const line = attempt(() => summarizeEventData(detail), 'The detail is nested too deeply to summarize.');
      const hasDetail = Object.keys(detail).length > 0;
      tbody.append(el('tr', { 'data-audit-id': record.id, 'data-audit-status': String(record.status) },
        el('td', { class: 'when' }, formatTime(record.at)),
        el('td', { title: record.tokenId || 'a bootstrap credential has no token record' },
          record.tokenName || el('span', { class: 'muted' }, 'unnamed'),
          record.tokenId ? null : el('span', { class: 'muted' }, ' (no token record)')),
        el('td', {}, el('span', { class: 'mono' }, record.method + ' ' + record.path)),
        el('td', {}, badge(String(record.status), statusKind(record.status))),
        el('td', { class: 'mono' }, record.sourceIp || ''),
        el('td', {}, record.serverId
          ? (servers.names.get(record.serverId) || el('span', { class: 'mono' }, record.serverId))
          : el('span', { class: 'muted' }, 'none')),
        el('td', { class: 'data' }, hasDetail ? disclosure(line, detail) : el('span', { class: 'muted' }, 'no detail'))));
      shown++;
    }
    table.hidden = shown === 0;
    empty.hidden = shown > 0;
    older.hidden = !cursor;
    status.textContent = (shown === 0 ? 'No records shown' : shown + ' record' + (shown === 1 ? '' : 's') + ' shown, newest first') +
      (cursor ? '; older records are available' : '');
  };

  let loading = false;
  older.addEventListener('click', async () => {
    if (loading || !cursor) return;
    loading = true;
    older.disabled = true;
    try {
      const next = new URLSearchParams(query);
      next.set('cursor', cursor);
      const answer = await api('GET', '/audit?' + next.toString());
      if (stale(seq)) return;
      cursor = answer.nextCursor || null;
      problem.hide();
      drawRecords(answer.records || []);
    } catch (err) {
      if (stale(seq)) return;
      if (err instanceof ApiError && err.status === 401) {
        signOut('The hub rejected this token.');
        return;
      }
      problem.show(err);
    } finally {
      loading = false;
      older.disabled = false;
    }
  });

  app.append(
    el('h1', {}, 'Audit log'),
    el('p', { class: 'muted' },
      'Every authenticated mutation, whether the hub performed it or refused it. Reads are not recorded.'),
    filterForm, problem.node, status, empty, table,
    el('div', { class: 'actions-row' }, older));
  if (page.problem) {
    status.textContent = 'The log could not be read with these filters.';
    problem.show(page.problem);
    return;
  }
  cursor = page.nextCursor || null;
  drawRecords(page.records || []);
}

// ---------------------------------------------------------------------------
// Key/value
//
// Read-only in this slice. The listing endpoints arrive with the hub lane of
// issue #64; a hub without them answers 404, which the view shows.

export async function viewKVNamespaces(app, route, seq) {
  setCrumbs([{ label: 'Key/value' }]);
  let data = null;
  let listProblem = null;
  try {
    data = await api('GET', '/kv');
  } catch (err) {
    if (stale(seq)) return;
    if (isForbidden(err)) {
      forbidden(app, 'kv:rw', err, 'GET /api/v1/kv');
      return;
    }
    if (err instanceof ApiError && err.status === 401) throw err;
    listProblem = err;
  }
  if (stale(seq)) return;
  clear(app);

  const open = textInput('kv-namespace-input', { placeholder: 'example-mod' });
  const openForm = el('form', {
    class: 'card feed-controls', id: 'kv-open', novalidate: true,
    onsubmit: (event) => {
      event.preventDefault();
      const namespace = open.value.trim();
      if (namespace === '') return;
      go(kvNamespaceHref(namespace));
    },
  },
  fieldRow('kv-namespace-input', 'Open a namespace', el('span', { class: 'filter-row' },
    open, el('button', { type: 'submit', class: 'small', id: 'kv-open-submit' }, 'Open')),
  'a namespace this token may read but that holds no live key yet is not listed above; type it here'));

  const tbody = el('tbody', {});
  const table = el('table', { id: 'kv-namespaces', hidden: true },
    el('thead', {}, el('tr', {}, el('th', {}, 'Namespace'), el('th', {}, 'Live keys'))),
    tbody);
  const empty = el('p', { class: 'notice', id: 'kv-empty', hidden: true },
    'No namespace this token may read holds a live key. A namespace the token cannot read is never listed.');
  const problem = problemBox('kv-error');

  app.append(
    el('h1', {}, 'Key/value'),
    el('p', { class: 'muted' },
      'Per-mod persistence (protocol section 12). Only namespaces this token’s grants cover are listed, and only those holding at least one live key.'),
    problem.node, table, empty, openForm);

  if (listProblem) {
    problem.show(listProblem);
    return;
  }
  const namespaces = Array.isArray(data.namespaces) ? data.namespaces : [];
  for (const entry of namespaces) {
    tbody.append(el('tr', {
      class: 'row-link', 'data-namespace': entry.namespace,
      onclick: () => { go(kvNamespaceHref(entry.namespace)); },
    },
    el('td', {}, el('a', { href: kvNamespaceHref(entry.namespace) }, entry.namespace)),
    el('td', {}, String(entry.keys === undefined ? 0 : entry.keys))));
  }
  table.hidden = namespaces.length === 0;
  empty.hidden = namespaces.length > 0;
}

export async function viewKVKeys(app, route, seq) {
  const namespace = route.namespace;
  setCrumbs([{ label: 'Key/value', href: kvHref() }, { label: namespace }]);
  const base = '/kv/' + encodeURIComponent(namespace);
  const query = new URLSearchParams();
  if (route.prefix) query.set('prefix', route.prefix);
  query.set('limit', String(KV_PAGE_SIZE));

  let page = null;
  let listProblem = null;
  try {
    page = await api('GET', base + '?' + query.toString());
  } catch (err) {
    if (stale(seq)) return;
    if (isForbidden(err)) {
      forbidden(app, 'kv:rw:' + namespace, err, 'GET /api/v1' + base);
      return;
    }
    if (err instanceof ApiError && err.status === 401) throw err;
    listProblem = err;
  }
  if (stale(seq)) return;
  clear(app);

  const problem = problemBox('kv-keys-error');
  const prefix = textInput('kv-prefix', { placeholder: 'balance.' });
  prefix.value = route.prefix || '';
  const prefixForm = el('form', {
    class: 'card feed-controls', id: 'kv-prefix-form', novalidate: true,
    onsubmit: (event) => {
      event.preventDefault();
      go(kvNamespaceHref(namespace, prefix.value.trim()));
    },
  },
  fieldRow('kv-prefix', 'Key prefix', el('span', { class: 'filter-row' },
    prefix,
    el('button', { type: 'submit', class: 'small', id: 'kv-prefix-apply' }, 'Apply')),
  'a plain string prefix, not a pattern; it lives in the route, so a reload keeps it'));

  const value = el('div', { id: 'kv-value', hidden: true });
  const status = el('p', { class: 'muted', id: 'kv-status' }, 'Loading…');
  const tbody = el('tbody', {});
  const table = el('table', { id: 'kv-keys', hidden: true },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Key'), el('th', {}, 'Revision'), el('th', {}, 'Expires'), el('th', {}, ''))),
    tbody);
  const empty = el('p', { class: 'notice', id: 'kv-keys-empty', hidden: true },
    'No live key here. Expired keys are never listed.');
  const more = el('button', { type: 'button', id: 'kv-more', hidden: true }, 'Load more');

  let cursor = null;
  let shown = 0;
  const openKey = async (key) => {
    problem.hide();
    try {
      const record = await api('GET', base + '/' + encodeURIComponent(key));
      if (stale(seq)) return;
      clear(value);
      value.hidden = false;
      value.append(el('div', { class: 'card', id: 'kv-value-card' },
        el('h2', {}, 'Value of ', el('span', { class: 'mono' }, key)),
        el('dl', { class: 'kv' },
          el('dt', {}, 'Revision'), el('dd', { id: 'kv-value-revision' }, String(record.revision)),
          el('dt', {}, 'Expires'), el('dd', { id: 'kv-value-expires' },
            record.expiresAt ? formatTime(record.expiresAt) : 'never'),
          // A stored value is bounded in bytes by the hub, not in depth
          // (section 12.2), and indenting a deeply nested one multiplies its
          // size by its depth: 16 KiB nested 4000 deep is tens of millions
          // of characters of whitespace. The same bounded helper the event
          // feed uses goes compact past 64 levels, and attempt keeps a value
          // that cannot be serialized to this one line.
          el('dt', {}, 'Value'), el('dd', {}, el('pre', { id: 'kv-value-json' },
            attempt(() => eventPayloadText(record.value), 'The value could not be serialized.'))))));
    } catch (err) {
      if (stale(seq)) return;
      if (err instanceof ApiError && err.status === 401) {
        signOut('The hub rejected this token.');
        return;
      }
      problem.show(err);
    }
  };
  const drawKeys = (keys) => {
    for (const entry of keys) {
      tbody.append(el('tr', { 'data-key': entry.key },
        el('td', { class: 'mono' }, entry.key),
        el('td', {}, String(entry.revision === undefined ? '' : entry.revision)),
        el('td', {}, entry.expiresAt ? formatTime(entry.expiresAt) : el('span', { class: 'muted' }, 'never')),
        el('td', { class: 'row-actions' }, el('button', {
          type: 'button', class: 'small', 'data-open-key': entry.key,
          onclick: () => { openKey(entry.key); },
        }, 'Open'))));
      shown++;
    }
    table.hidden = shown === 0;
    empty.hidden = shown > 0;
    more.hidden = !cursor;
    status.textContent = (shown === 0 ? 'No keys shown' : shown + ' key' + (shown === 1 ? '' : 's') + ' shown, key ascending') +
      (cursor ? '; more are available' : '');
  };

  let loading = false;
  more.addEventListener('click', async () => {
    if (loading || !cursor) return;
    loading = true;
    more.disabled = true;
    try {
      const next = new URLSearchParams(query);
      next.set('cursor', cursor);
      const answer = await api('GET', base + '?' + next.toString());
      if (stale(seq)) return;
      cursor = answer.nextCursor || null;
      problem.hide();
      drawKeys(answer.keys || []);
    } catch (err) {
      if (stale(seq)) return;
      if (err instanceof ApiError && err.status === 401) {
        signOut('The hub rejected this token.');
        return;
      }
      problem.show(err);
    } finally {
      loading = false;
      more.disabled = false;
    }
  });

  app.append(
    el('h1', {}, 'Key/value ', el('span', { class: 'muted title-tail' }, namespace)),
    el('p', { class: 'notice', id: 'kv-readonly' },
      'Read-only in this slice: the panel lists keys and opens one value at a time. Editing arrives with presets, issue #76; until then a value is written with PUT /api/v1/kv/{namespace}/{key} (protocol section 12.2).'),
    prefixForm, problem.node, status, empty, table,
    el('div', { class: 'actions-row' }, more), value);

  if (listProblem) {
    status.textContent = 'The keys could not be listed.';
    problem.show(listProblem);
    return;
  }
  cursor = page.nextCursor || null;
  drawKeys(page.keys || []);
}
