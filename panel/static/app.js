// Vyshka panel: a thin client over the Admin API (/api/v1).
//
// Nothing here knows a game. Servers come from GET /servers, actions from the
// manifest each plugin published, and every action form is generated from the
// params schema in that manifest (protocol section 6.1's JSON Schema subset,
// plus the x-vyshka-widget hint). Dispatching is POST /servers/{id}/actions
// and watching the result is GET /actions/{id}, the same calls curl makes.
//
// The DOM is built with createElement and text nodes only. Manifest labels
// and result payloads are plugin-supplied text and are never treated as
// markup; the Content-Security-Policy the hub sends is the backstop.

const API = '/api/v1';
const TOKEN_KEY = 'vyshka.adminToken';
const SERVER_LIST_REFRESH_MS = 5000;
const ACTION_POLL_MS = 1000;
const TERMINAL_STATES = new Set(['completed', 'failed', 'expired']);

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

function parseRoute() {
  const parts = location.hash.replace(/^#/, '').split('/').filter(Boolean).map(decodeURIComponent);
  if (parts[0] === 'servers' && parts.length === 2) {
    return { view: 'server', serverId: parts[1] };
  }
  if (parts[0] === 'servers' && parts.length === 4 && parts[2] === 'actions') {
    return { view: 'action', serverId: parts[1], code: parts[3] };
  }
  return { view: 'servers' };
}

function serverHref(serverId) {
  return '#/servers/' + encodeURIComponent(serverId);
}

function actionHref(serverId, code) {
  return serverHref(serverId) + '/actions/' + encodeURIComponent(code);
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
  setCrumbs([{ label: 'Servers', href: '#/' }, { label: server.name }]);
  clear(app);
  app.append(serverSummary(server));

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
      class: 'action-item', href: actionHref(server.id, action.code), 'data-action-code': action.code,
    },
    el('span', { class: 'name' }, action.name || action.code),
    el('span', { class: 'code mono' }, action.code),
    el('span', { class: 'spacer' }),
    action.context ? badge(action.context) : null,
    action.danger && action.danger !== 'none' ? badge(action.danger, action.danger) : null))));
  }
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

// enforced says whether a required field's emptiness should stop a submit:
// true for a required field of a present object, false for one inside an
// optional object nobody has touched, which the object itself omits whole.
function enforced(opts) {
  return Boolean(opts.required) && !opts.soft;
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
  return {
    node: wrapped.node, setError: wrapped.setError,
    read() {
      if (select.value === '') return undefined;
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
      read() { return select.value === '' ? undefined : select.value === 'true'; },
    };
  }
  const input = el('input', { type: 'checkbox', name: opts.name, id: nextId('field') });
  if (schema.default === true) input.checked = true;
  const wrapped = wrap(opts, input, describe(schema), true);
  return {
    node: wrapped.node, setError: wrapped.setError,
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
  return {
    node: wrapped.node, setError: wrapped.setError,
    read(errors) {
      const text = input.value.trim();
      if (text === '') {
        if (opts.required) errors.push({ path: opts.path, message: 'is required' });
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
  return {
    node: wrapped.node, setError: wrapped.setError,
    read(errors) {
      const value = input.value;
      if (value === '') {
        if (opts.required) errors.push({ path: opts.path, message: 'is required' });
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
    return {
      node: wrapped.node, setError: wrapped.setError,
      read(errors) {
        const texts = axes.map((axis) => axis.value.trim());
        if (texts.every((text) => text === '')) {
          if (opts.required) errors.push({ path: opts.path, message: 'is required' });
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
  return {
    node: wrapped.node, setError: wrapped.setError,
    read(errors) {
      // String items are taken as typed; other kinds are trimmed before
      // parsing. Only blank lines are dropped, so a line of spaces is a
      // string item and an empty string cannot be expressed here (a
      // limitation of the one-per-line form, not of the protocol).
      const raw = textarea.value.split('\n');
      const lines = (items.type === 'string' ? raw : raw.map((line) => line.trim())).filter((line) => line.trim() !== '');
      // Empty: a required array of a present object is sent empty; a soft
      // one is left to its parent, which omits itself when nothing else in
      // it is set and otherwise lets the hub name the missing key.
      if (lines.length === 0) return enforced(opts) ? [] : undefined;
      const values = lines.map((line, index) => coerceItem(items, line, errors, opts.path + '[' + index + ']'));
      return values.some((value) => value === undefined) ? undefined : values;
    },
  };
}

function objectField(schema, opts) {
  const properties = schema.properties && typeof schema.properties === 'object' ? schema.properties : null;
  if (!properties || Object.keys(properties).length === 0) return jsonField(schema, opts);
  const required = new Set(Array.isArray(schema.required) ? schema.required : []);
  // An optional object left entirely empty is omitted whole, which is valid
  // whatever it requires of its children; so its children's own required
  // marks are soft: shown, and reported only once something in the object
  // is filled in.
  const optional = !opts.required || opts.soft;
  const keys = Object.keys(properties);
  const children = keys.map((key) => buildField(properties[key], {
    name: opts.name + '.' + key,
    path: opts.path ? opts.path + '.' + key : key,
    label: key,
    required: required.has(key),
    soft: optional,
    players: opts.players,
    register: opts.register,
  }));
  const errorNode = el('span', { class: 'field-error', hidden: true });
  const node = el('fieldset', { 'data-path': opts.path },
    el('legend', {}, opts.label, opts.required ? el('span', { class: 'req' }, '*') : null),
    children.map((child) => child.node), errorNode);
  const field = {
    node,
    setError(message) {
      errorNode.textContent = message || '';
      errorNode.hidden = !message;
    },
    read(errors) {
      // Null prototype: a manifest may name a property __proto__, and on
      // an ordinary object that assignment would go to the prototype
      // setter instead of becoming a key the hub can see.
      const value = Object.create(null);
      const own = [];
      let any = false;
      children.forEach((child, index) => {
        const item = child.read(own);
        if (item !== undefined) {
          value[keys[index]] = item;
          any = true;
        }
      });
      if (!any && optional && opts.path !== '') return undefined;
      errors.push(...own);
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
  return { node: wrapped.node, setError: wrapped.setError, read() { return opts.required ? null : undefined; } };
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
  return {
    node: wrapped.node, setError: wrapped.setError,
    read(errors) {
      const text = textarea.value.trim();
      if (text === '') {
        if (opts.required) errors.push({ path: opts.path, message: 'is required' });
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
      const value = root.read(errors);
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

  const form = el('form', {
    class: 'stack card', id: 'action-form',
    onsubmit: async (event) => {
      event.preventDefault();
      formErrors.hidden = true;
      clear(formErrors);
      const errors = [];
      const referenceKey = target ? target.read(errors) : undefined;
      const paramsValue = params.read(errors);
      const orphans = showFaults(errors, params.fieldsByPath);
      if (errors.length > 0) {
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
