// Vyshka panel: the installation ban list (protocol section 13).
//
// One list for every server of the installation, held by the hub and pulled
// by every plugin that declares the bans capability (section 6.7). This
// module is the list's view at #/bans, the ban form that view shares with a
// player's profile, the list of one identity's bans the profile draws, and
// the line on a server page saying whether the server enforces the list and
// at which revision. Every call is one of section 13.2's, the same an
// operator could make with curl.
//
// A reason and a name are operator-typed text that travels to every game
// server; like every other view, the DOM is built from createElement and text
// nodes, and neither is ever treated as markup.

import {
  ApiError, ago, api, append, attempt, badge, bansHref, clear, el, forbidden, formatTime, go, isForbidden,
  playerHref, problemBox, serverHref, setCrumbs, signOut, stale,
} from './lib.js';

// The hub's reference default page (section 13.2: default 100, cap 500).
const BAN_PAGE_SIZE = 100;
// The bounds of section 13.1, in code points, which is what the hub counts.
const REASON_MAX = 200;
const NAME_MAX = 200;
// The longest ban section 13.2 accepts: ten years.
const DURATION_MAX = 315360000;

const DURATION_CHOICES = [
  { value: '', label: 'permanent' },
  { value: '3600', label: '1 hour' },
  { value: '86400', label: '1 day' },
  { value: '604800', label: '7 days' },
  { value: '2592000', label: '30 days' },
  { value: 'custom', label: 'custom' },
];

const DURATION_UNITS = [
  { value: '3600', label: 'hours' },
  { value: '86400', label: 'days' },
];

function query(parameters) {
  const encoded = new URLSearchParams(parameters).toString();
  return encoded ? '?' + encoded : '';
}

function fieldRow(id, label, control, hint) {
  return el('label', { class: 'field', for: id },
    el('span', { class: 'name' }, label),
    control,
    hint ? el('span', { class: 'hint' }, hint) : null);
}

// A read from a view that has gone, or under a token since replaced, must not
// sign out whoever is signed in now; the callers check stale() first.
function onUnauthorized(err) {
  if (err instanceof ApiError && err.status === 401) {
    signOut('The hub rejected this token.');
    return true;
  }
  return false;
}

// loadServers reads the server list for the provenance picker and the server
// column. A token with bans:read but no servers:read cannot have it, which is
// not an error: the picker is left out and the column shows ids.
export async function loadServers() {
  try {
    const data = await api('GET', '/servers');
    const servers = Array.isArray(data.servers) ? data.servers : [];
    return { allowed: true, servers, names: new Map(servers.map((server) => [server.id, server.name])) };
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) throw err;
    return { allowed: false, servers: [], names: new Map() };
  }
}

function identityOf(ban) {
  const player = ban && ban.player && typeof ban.player === 'object' ? ban.player : {};
  return {
    platform: typeof player.platform === 'string' ? player.platform : '',
    id: typeof player.id === 'string' ? player.id : '',
  };
}

function credentialName(credential) {
  const record = credential && typeof credential === 'object' ? credential : {};
  return record.tokenName || 'an unnamed credential';
}

function stateBadge(state) {
  if (state === 'active') return badge('active', 'destructive');
  if (state === 'expired') return badge('expired', 'expired');
  return badge(String(state || 'unknown'));
}

// ---------------------------------------------------------------------------
// The list
//
// banList draws one newest-first list of bans read with the hub's cursor, the
// first page and then older ones behind a button, with a Lift on each active
// ban behind a second click, the way a token is revoked. A lift redraws the
// list from its first page, since a lifted ban leaves a list of active ones.
//
// options: id (the table's), seq, names (server id to name), showPlayer,
// filters (the query besides limit and cursor), emptyText, problem (the box
// failures land in), onRevision (called with every revision the hub
// reports), and onForbidden (called when a read is refused for scope).

export function banList(options) {
  const { id, seq, names, showPlayer, filters, emptyText, problem, onRevision, onForbidden } = options;
  const columns = [showPlayer ? 'Player' : null, 'Name', 'Reason', 'Expires', 'Placed', 'Server', 'State', '']
    .filter((column) => column !== null);
  const tbody = el('tbody', {});
  const table = el('table', { id, class: 'bans', hidden: true },
    el('thead', {}, el('tr', {}, columns.map((name) => el('th', {}, name)))), tbody);
  const empty = el('p', { class: 'notice', id: id + '-empty', hidden: true }, emptyText);
  const status = el('p', { class: 'muted', id: id + '-status' });
  const older = el('button', { type: 'button', id: id + '-older', hidden: true }, 'Load older');

  let cursor = null;
  let shown = 0;
  // Every walk from the first page takes a number, and a page answering for
  // a walk since restarted (a lift, a ban placed) is dropped, so an older
  // page of the previous list is never appended to the new one.
  let walk = 0;
  const path = (from) => '/bans' + query(Object.assign({}, filters,
    { limit: String(BAN_PAGE_SIZE) }, from ? { cursor: from } : {}));

  const readFailed = (err) => {
    if (stale(seq) || onUnauthorized(err)) return;
    if (isForbidden(err) && onForbidden) {
      onForbidden(err);
      return;
    }
    problem.show(err);
  };

  const lift = async (ban, button) => {
    problem.hide();
    button.disabled = true;
    try {
      const change = await api('POST', '/bans/' + encodeURIComponent(ban.id) + '/lift');
      if (stale(seq)) return;
      if (onRevision) onRevision(change.revision);
      await reload();
    } catch (err) {
      // A refused lift is the lift's problem, never the list's: a token that
      // reads the list without bans:manage keeps its list.
      if (stale(seq) || onUnauthorized(err)) return;
      problem.show(err);
    } finally {
      button.disabled = false;
    }
  };

  const liftCell = (ban) => {
    if (ban.state !== 'active') return null;
    const cell = el('span', { class: 'revoke-cell' });
    const ask = el('button', { type: 'button', class: 'small danger', 'data-lift-ban': ban.id }, 'Lift');
    const confirm = el('button', {
      type: 'button', class: 'small danger', 'data-lift-confirm': ban.id,
      title: 'Takes the ban off the list every server enforces',
    }, 'Confirm lift');
    const cancel = el('button', { type: 'button', class: 'small', 'data-lift-cancel': ban.id }, 'Cancel');
    ask.addEventListener('click', () => { clear(cell); cell.append(confirm, ' ', cancel); });
    cancel.addEventListener('click', () => { clear(cell); cell.append(ask); });
    confirm.addEventListener('click', () => { lift(ban, confirm); });
    cell.append(ask);
    return cell;
  };

  const rowOf = (ban) => {
    const who = identityOf(ban);
    const key = who.platform + ':' + who.id;
    const state = String(ban.state || '');
    const serverId = typeof ban.serverId === 'string' && ban.serverId !== '' ? ban.serverId : '';
    const serverName = serverId ? names.get(serverId) : '';
    return el('tr', { 'data-ban-id': ban.id, 'data-ban-state': state, class: state === 'active' ? '' : 'dim' },
      showPlayer ? el('td', {}, el('a', { class: 'mono', href: playerHref(who.platform, who.id), 'data-profile': key }, key)) : null,
      el('td', {}, ban.name ? String(ban.name) : el('span', { class: 'muted' }, 'none')),
      el('td', { class: 'reason' }, String(ban.reason || '')),
      el('td', {}, ban.expiresAt ? formatTime(ban.expiresAt) : el('span', { class: 'muted' }, 'permanent')),
      el('td', { title: ban.createdBy && ban.createdBy.tokenId ? ban.createdBy.tokenId : 'a bootstrap credential has no token record' },
        credentialName(ban.createdBy), el('br'), el('span', { class: 'muted' }, formatTime(ban.createdAt))),
      el('td', {}, !serverId
        ? el('span', { class: 'muted' }, 'none')
        : serverName ? el('a', { href: serverHref(serverId) }, serverName) : el('span', { class: 'mono' }, serverId)),
      el('td', {}, stateBadge(state),
        state === 'lifted'
          ? el('span', { class: 'muted' }, ' by ' + credentialName(ban.liftedBy) + ', ' + formatTime(ban.liftedAt))
          : null),
      el('td', { class: 'row-actions' }, liftCell(ban)));
  };

  const draw = (page) => {
    if (onRevision) onRevision(page.revision);
    for (const ban of Array.isArray(page.bans) ? page.bans : []) {
      if (!ban || typeof ban !== 'object') continue;
      tbody.append(attempt(() => rowOf(ban),
        el('tr', {}, el('td', { colspan: String(columns.length) }, 'This ban could not be drawn.'))));
      shown++;
    }
    cursor = page.nextCursor || null;
    table.hidden = shown === 0;
    empty.hidden = shown > 0;
    older.hidden = !cursor;
    status.textContent = (shown === 0 ? 'No bans shown' : shown + ' ban' + (shown === 1 ? '' : 's') + ' shown, newest first') +
      (cursor ? '; older bans are available' : '');
  };

  // show starts a walk from a first page the caller already holds.
  const show = (page) => {
    walk++;
    clear(tbody);
    shown = 0;
    draw(page);
  };

  const reload = async () => {
    const mine = ++walk;
    try {
      const page = await api('GET', path(null));
      if (stale(seq) || mine !== walk) return;
      clear(tbody);
      shown = 0;
      draw(page);
    } catch (err) {
      if (mine === walk) readFailed(err);
    }
  };

  older.addEventListener('click', async () => {
    if (!cursor) return;
    const mine = walk;
    older.disabled = true;
    try {
      const page = await api('GET', path(cursor));
      if (stale(seq) || mine !== walk) return;
      draw(page);
    } catch (err) {
      if (mine === walk) readFailed(err);
    } finally {
      older.disabled = false;
    }
  });

  return { node: el('div', {}, status, empty, table, el('div', { class: 'actions-row' }, older)), show, reload };
}

// ---------------------------------------------------------------------------
// The ban form
//
// banForm places one ban over POST /bans. With an identity it is the compact
// form a profile carries, the identity fixed; without one it asks for the
// platform and the id. A second ban of an identity already banned is the
// hub's conflict, which names the ban standing, and the form says so rather
// than showing the bare code: lifting and banning again are two acts.
//
// options: prefix (every id in the form starts with it), identity (or null),
// servers (the provenance picker's choices, empty to leave it out), seq, and
// onPlaced (called with the hub's answer).

function durationSeconds(choice, amount, unit) {
  if (choice === '') return undefined;
  if (choice !== 'custom') return Number(choice);
  const count = Number(amount.trim());
  if (amount.trim() === '' || !Number.isInteger(count) || count < 1) {
    throw new ApiError(0, 'bad_request', 'type a whole number of ' + (unit === '86400' ? 'days' : 'hours') + ', at least 1, or choose another duration');
  }
  const seconds = count * Number(unit);
  if (seconds > DURATION_MAX) {
    throw new ApiError(0, 'bad_request', 'a ban lasts at most ten years (' + DURATION_MAX + ' s, protocol section 13.2); choose permanent for longer');
  }
  return seconds;
}

export function banForm(options) {
  const { prefix, identity, servers, seq, onPlaced } = options;
  const id = (name) => prefix + '-' + name;
  const platform = el('input', { type: 'text', id: id('platform'), name: id('platform'), value: 'steam', spellcheck: 'false', autocomplete: 'off' });
  const player = el('input', { type: 'text', id: id('player-id'), name: id('player-id'), spellcheck: 'false', autocomplete: 'off', placeholder: '76561198000000000' });
  // No maxlength on either text field: the browser counts UTF-16 units there,
  // and the hub's bounds are code points. The submit counts.
  const reason = el('input', { type: 'text', id: id('reason'), name: id('reason'), autocomplete: 'off', placeholder: 'What every server will say it is for' });
  const name = el('input', { type: 'text', id: id('name'), name: id('name'), autocomplete: 'off', placeholder: 'the name the player went by' });
  const duration = el('select', { id: id('duration'), name: id('duration') },
    DURATION_CHOICES.map((choice) => el('option', { value: choice.value }, choice.label)));
  const amount = el('input', { type: 'number', id: id('duration-amount'), name: id('duration-amount'), min: '1', step: '1', placeholder: 'how many' });
  const unit = el('select', { id: id('duration-unit'), name: id('duration-unit') },
    DURATION_UNITS.map((choice) => el('option', { value: choice.value }, choice.label)));
  const custom = el('span', { class: 'filter-row', hidden: true }, amount, unit);
  duration.addEventListener('change', () => { custom.hidden = duration.value !== 'custom'; });
  const pickable = Array.isArray(servers) ? servers : [];
  const server = pickable.length === 0 ? null : el('select', { id: id('server'), name: id('server') },
    el('option', { value: '' }, 'none'),
    pickable.map((entry) => el('option', { value: entry.id }, entry.name || entry.id)));
  const problem = problemBox(id('error'));
  const conflict = el('div', { class: 'error', id: id('conflict'), role: 'alert', hidden: true });
  const placed = el('p', { class: 'notice', id: id('placed'), hidden: true });
  const submit = el('button', { type: 'submit', class: 'danger', id: id('submit') }, 'Ban on every server');

  // showConflict names the ban standing, and then fills in what it was for
  // with one read of it, which bans:manage (implying bans:read) allows. A
  // read that fails leaves the ban named by its id, which is enough to find it.
  const showConflict = async (who, banId) => {
    clear(conflict);
    const key = who.platform + ':' + who.id;
    append(conflict, [el('strong', {}, 'conflict: '), key + ' is already banned',
      banId ? [' (ban ', el('span', { class: 'mono', 'data-conflict-ban': banId }, banId), ')'] : null,
      '. Lift that ban first to ban again with another reason or duration; lifting and banning are two acts, each audited.',
      identity ? null : [' ', el('a', { href: playerHref(who.platform, who.id), id: id('conflict-profile') }, 'See this player’s bans'), '.']]);
    conflict.hidden = false;
    if (!banId) return;
    try {
      const answer = await api('GET', '/bans/' + encodeURIComponent(banId));
      if (stale(seq) || conflict.hidden) return;
      const standing = answer && answer.ban ? answer.ban : null;
      if (!standing) return;
      conflict.append(el('span', { class: 'conflict-detail' },
        ' It was placed by ' + credentialName(standing.createdBy) + ', ' + formatTime(standing.createdAt) +
        ', for: “' + String(standing.reason || '') + '”, ' +
        (standing.expiresAt ? 'until ' + formatTime(standing.expiresAt) : 'permanently') + '.'));
    } catch (err) {
      if (!stale(seq)) onUnauthorized(err);
    }
  };

  const form = el('form', {
    class: 'stack card', id: prefix, novalidate: true,
    onsubmit: async (event) => {
      event.preventDefault();
      problem.hide();
      conflict.hidden = true;
      placed.hidden = true;
      const who = identity
        ? { platform: identity.platform, id: identity.id }
        : { platform: platform.value.trim(), id: player.value.trim() };
      const request = { player: who, reason: reason.value.trim() };
      const label = name.value.trim();
      try {
        if (who.platform === '' || who.id === '') {
          throw new ApiError(0, 'bad_request', 'both the platform and the player id are needed');
        }
        if (request.reason === '') {
          throw new ApiError(0, 'bad_request', 'a reason is required: it travels to every server, and is what a refused player may be shown');
        }
        if ([...request.reason].length > REASON_MAX) {
          throw new ApiError(0, 'bad_request', 'a reason holds at most ' + REASON_MAX + ' characters; this one has ' + [...request.reason].length);
        }
        if ([...label].length > NAME_MAX) {
          throw new ApiError(0, 'bad_request', 'a name holds at most ' + NAME_MAX + ' characters; this one has ' + [...label].length);
        }
        const seconds = durationSeconds(duration.value, amount.value, unit.value);
        if (seconds !== undefined) request.durationSeconds = seconds;
      } catch (err) {
        problem.show(err);
        return;
      }
      if (label !== '') request.name = label;
      if (server && server.value !== '') request.serverId = server.value;
      submit.disabled = true;
      try {
        const change = await api('POST', '/bans', request);
        if (stale(seq)) return;
        placed.textContent = 'Banned ' + who.platform + ':' + who.id + ' on every server; the list is at revision ' +
          change.revision + '. Each server enforces it once its plugin has pulled that revision.';
        placed.hidden = false;
        reason.value = '';
        name.value = '';
        if (!identity) player.value = '';
        if (onPlaced) onPlaced(change);
      } catch (err) {
        if (stale(seq) || onUnauthorized(err)) return;
        if (err instanceof ApiError && err.code === 'conflict') {
          showConflict(who, err.details && typeof err.details.banId === 'string' ? err.details.banId : '');
          return;
        }
        problem.show(err);
      } finally {
        submit.disabled = false;
      }
    },
  },
  el('h2', {}, identity ? 'Ban this player' : 'Ban a player'),
  el('p', { class: 'muted' },
    'One POST /api/v1/bans (protocol section 13.2). The ban goes on the installation list, which every server whose plugin enforces it refuses at connect; a player online there is disconnected once the plugin pulls it. Needs bans:manage.'),
  identity ? null : [
    fieldRow(id('platform'), 'Platform', platform, 'required'),
    fieldRow(id('player-id'), 'Player id', player, 'required: the platform id, such as a Steam64 id'),
  ],
  fieldRow(id('reason'), 'Reason', reason, 'required, at most ' + REASON_MAX + ' characters; every server gets it, and a refused player may be shown it'),
  fieldRow(id('name'), 'Name', name, 'optional: a label for whoever reads the list, stored as typed'),
  fieldRow(id('duration'), 'Duration', el('span', { class: 'filter-row' }, duration, custom),
    'counted by the hub from when it places the ban; permanent lasts until someone lifts it'),
  server ? fieldRow(id('server'), 'Server it arose on', server, 'optional provenance for the audit log’s per-server view; the ban applies on every server whatever this says') : null,
  problem.node,
  conflict,
  placed,
  el('div', { class: 'actions-row' }, submit));
  return { node: form };
}

// ---------------------------------------------------------------------------
// The view

export async function viewBans(app, route, seq) {
  setCrumbs([{ label: 'Bans' }]);
  const all = Boolean(route.all);
  const filters = { state: all ? 'all' : 'active' };
  let first;
  try {
    first = await api('GET', '/bans' + query(Object.assign({}, filters, { limit: String(BAN_PAGE_SIZE) })));
  } catch (err) {
    if (stale(seq)) return;
    if (isForbidden(err)) {
      forbidden(app, 'bans:read', err, 'GET /api/v1/bans');
      return;
    }
    throw err;
  }
  if (stale(seq)) return;
  const servers = await loadServers();
  if (stale(seq)) return;
  clear(app);

  const revision = el('strong', { id: 'bans-revision' });
  const setRevision = (value) => {
    if (Number.isInteger(value)) revision.textContent = 'List revision ' + value;
  };
  const problem = problemBox('bans-error');
  const list = banList({
    id: 'bans', seq, names: servers.names, showPlayer: true, filters, problem, onRevision: setRevision,
    emptyText: all
      ? 'No ban has been placed on this installation yet.'
      : 'No ban is active. Tick the box above to see the lifted and expired ones.',
  });
  const everyState = el('input', { type: 'checkbox', id: 'bans-all' });
  everyState.checked = all;
  everyState.addEventListener('change', () => { go(bansHref(everyState.checked)); });
  const form = banForm({
    prefix: 'ban-form', identity: null, servers: servers.allowed ? servers.servers : [], seq,
    onPlaced: (change) => {
      setRevision(change.revision);
      list.reload();
    },
  });

  app.append(
    el('h1', {}, 'Installation bans'),
    el('p', { class: 'muted' },
      'One list for every server of this installation (protocol section 13). Each server whose plugin declares the bans capability pulls it and reports the revision it enforces, which its page shows. A server’s own ban list is apart from this one and untouched by it.'),
    el('p', {}, revision, el('span', { class: 'muted' },
      '. It moves each time a ban is placed, lifted, or expires off the list.')),
    el('label', { class: 'field inline', id: 'bans-all-field' }, everyState,
      el('span', { class: 'name' }, 'Include lifted and expired bans')),
    problem.node,
    list.node,
    form.node);
  list.show(first);
}

// ---------------------------------------------------------------------------
// The server page's line

// serverBansDetail is the "Installation bans" entry of a server page: whether
// the server's plugin enforces the list (its manifest declares the bans
// capability) and the revision it last reported enforcing. When this token
// may read the list, the current revision is read once to say whether the
// server has caught up; a refusal leaves the comparison out.
export function serverBansDetail(server, seq) {
  const bans = server && server.bans && typeof server.bans === 'object' ? server.bans : {};
  const node = el('dd', { id: 'server-bans' });
  if (!bans.supported) {
    node.append(el('span', { class: 'muted' },
      'not supported by this server’s plugin (its manifest does not declare the bans capability, protocol section 6.7)'));
    return node;
  }
  const applied = Number.isInteger(bans.appliedRevision) ? bans.appliedRevision : null;
  node.append(applied === null
    ? 'supported; no report yet'
    : el('span', { title: 'reported ' + formatTime(bans.appliedAt) },
      'enforcing revision ' + applied + ', reported ' + ago(bans.appliedAt)));
  (async () => {
    let current;
    try {
      const page = await api('GET', '/bans' + query({ limit: '1' }));
      current = page && page.revision;
    } catch (err) {
      if (!stale(seq)) onUnauthorized(err);
      return;
    }
    if (stale(seq) || !Number.isInteger(current)) return;
    let sync;
    if (applied === null) {
      sync = el('span', { class: 'muted', id: 'server-bans-sync', 'data-sync': 'unreported' },
        '; the list is at revision ' + current);
    } else if (applied === current) {
      sync = el('span', { id: 'server-bans-sync', 'data-sync': 'current' }, ' ', badge('up to date', 'up'));
    } else {
      // Differs, not only trails: a hub restored from a backup can be behind
      // the server, and the plugin walks either way (section 13.4).
      sync = el('span', { id: 'server-bans-sync', 'data-sync': applied < current ? 'behind' : 'ahead' }, ' ',
        badge(applied < current ? 'behind' : 'out of step', 'warning'),
        el('span', { class: 'muted' }, ' the list is at revision ' + current));
    }
    node.append(sync);
  })();
  return node;
}
