// Vyshka panel: player profiles (protocol section 8.6).
//
// A profile is one identity across every server of the installation: the
// events that refer to it, the actions dispatched against it, and the notes
// operators keep on it, each from its own Admin API read and each shown or
// refused on its own, because each carries its own grant. The events are as
// deep as the hub's retention and no deeper, and the page says so rather than
// letting an empty table pass for a clean record.
//
// Like every other view, the DOM is built from createElement and text nodes:
// names, chat lines, and notes are text other people wrote.

import {
  ApiError, api, attempt, badge, clear, confirmation, disclosure, el, formatTime, go, identityLinks,
  isForbidden, playerHref, playersHref, problemBox, serverHref, setCrumbs, signOut, stale,
  summarizeEventData, token,
} from './lib.js';

const PROFILE_PAGE_SIZE = 50;
const NOTE_MAX = 4000;

// The reference retention of protocol section 8.4, quoted rather than known:
// retention is hub configuration, and the Admin API does not report it.
const RETENTION_NOTE = 'Events stay as long as the hub keeps them (reference: 30 days, chat 90), so this is a window onto the recent past, never a lifetime record. Notes are kept until someone deletes them.';

async function serverNames() {
  try {
    const data = await api('GET', '/servers');
    const servers = Array.isArray(data.servers) ? data.servers : [];
    return new Map(servers.map((server) => [server.id, server.name]));
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) throw err;
    return new Map();
  }
}

function serverLabel(names, serverId) {
  const name = names.get(serverId);
  return name ? el('a', { href: serverHref(serverId) }, name) : el('span', { class: 'mono' }, serverId);
}

// A section that the hub refused for scope says which grant it needs, in
// place, and the rest of the page carries on.
function refusedNotice(id, scope, err) {
  return el('p', { class: 'notice', id },
    'This token cannot read this part of the profile: the hub answered ',
    el('span', { class: 'mono' }, (err && err.code) || 'forbidden'), '. It needs ', el('span', { class: 'mono' }, scope), '.');
}

function onUnauthorized(err) {
  if (err instanceof ApiError && err.status === 401) {
    signOut('The hub rejected this token.');
    return true;
  }
  return false;
}

// pagedSection draws one newest-first list read with the hub's cursor: the
// first page on load, older pages behind a button. load(cursor) answers one
// page; rowOf(item) draws one row.
function pagedSection(options) {
  const { id, heading, columns, emptyText, scope, load, itemsOf, rowOf, seq } = options;
  const tbody = el('tbody', {});
  const table = el('table', { id, hidden: true },
    el('thead', {}, el('tr', {}, columns.map((name) => el('th', {}, name)))), tbody);
  const empty = el('p', { class: 'notice', id: id + '-empty', hidden: true }, emptyText);
  const problem = problemBox(id + '-error');
  const older = el('button', { type: 'button', id: id + '-older', hidden: true }, 'Load older');
  const body = el('div', {}, problem.node, empty, table, el('div', { class: 'actions-row' }, older));
  const section = el('section', { class: 'card', id: id + '-section' }, el('h2', {}, heading), body);

  let cursor = null;
  let count = 0;
  const fetchPage = async (from) => {
    older.disabled = true;
    try {
      const page = await load(from);
      if (stale(seq)) return;
      for (const item of itemsOf(page)) {
        tbody.append(attempt(() => rowOf(item), el('tr', {}, el('td', { colspan: String(columns.length) }, 'This row could not be drawn.'))));
        count++;
      }
      cursor = page.nextCursor || null;
      table.hidden = count === 0;
      empty.hidden = count > 0;
      older.hidden = !cursor;
      problem.hide();
    } catch (err) {
      if (stale(seq) || onUnauthorized(err)) return;
      if (isForbidden(err)) {
        clear(body);
        body.append(refusedNotice(id + '-forbidden', scope, err));
        return;
      }
      problem.show(err);
    } finally {
      older.disabled = false;
    }
  };
  older.addEventListener('click', () => { if (cursor) fetchPage(cursor); });
  fetchPage(null);
  return section;
}

function identityPath(platform, id, tail) {
  return '/players/' + encodeURIComponent(platform) + '/' + encodeURIComponent(id) + tail;
}

function query(parameters) {
  const encoded = new URLSearchParams(parameters).toString();
  return encoded ? '?' + encoded : '';
}

// latestName is the name the newest event gives the identity in the player
// role, the one place a payload names the person rather than someone else.
function latestName(events) {
  for (const event of events) {
    const data = event && event.data && typeof event.data === 'object' ? event.data : {};
    if (Array.isArray(event.roles) && event.roles.includes('player') && typeof data.name === 'string' && data.name !== '') {
      return data.name;
    }
  }
  return '';
}

function notesSection(platform, id, seq) {
  const list = el('ul', { class: 'notes', id: 'notes' });
  const empty = el('p', { class: 'muted', id: 'notes-empty', hidden: true }, 'No notes yet.');
  // The problem box and the saved notice live outside the list, so a token
  // that may write notes but not read them still sees what its writes did.
  const problem = problemBox('notes-error');
  const saved = el('p', { class: 'notice', id: 'note-saved', hidden: true });
  const older = el('button', { type: 'button', id: 'notes-older', hidden: true }, 'Load older');
  // No maxlength: the browser counts UTF-16 units there, and the hub's bound
  // is 4000 code points, so an emoji would count twice. The submit counts.
  const text = el('textarea', { id: 'note-text', name: 'note-text', rows: '3', placeholder: 'What the next moderator should know about this player' });
  const add = el('button', { type: 'submit', class: 'primary', id: 'note-add' }, 'Add note');
  const listArea = el('div', {}, empty, list, el('div', { class: 'actions-row' }, older));
  const body = el('div', {}, problem.node, saved, listArea);
  let cursor = null;
  // rendered holds the row of every note on the page by id. A read and a
  // write can answer in either order, and a note may arrive through both (a
  // read that ran after the write committed), so both place notes through
  // place(), which draws each id once.
  const rendered = new Map();
  // deleted holds the id of every note deleted from this view, for the
  // view's lifetime: a read or a write answering after the delete may still
  // carry the note, and must not draw it back.
  const deleted = new Set();
  // The list's read state: pending until the first read settles, then
  // readable or forbidden. A write while it is readable or pending is placed
  // in the list at once; a refusal clears the list, and the saved notice,
  // which lives outside it, then confirms the writes still standing.
  let readState = 'pending';
  const written = new Set();
  const showSaved = () => {
    if (written.size === 0) {
      saved.hidden = true;
      return;
    }
    saved.textContent = 'Saved. This token cannot read notes (notes:read), so ' +
      (written.size === 1 ? 'the note is' : 'the ' + written.size + ' notes are') + ' not listed here.';
    saved.hidden = false;
  };
  const place = (note, atTop) => {
    const key = String(note.id);
    if (rendered.has(key) || deleted.has(key)) return;
    const item = noteItem(note);
    rendered.set(key, item);
    if (atTop) list.prepend(item); else list.append(item);
    empty.hidden = true;
  };

  const noteItem = (note) => {
    const author = note.createdBy && typeof note.createdBy === 'object' ? note.createdBy : {};
    const remove = confirmation('note-confirm-' + note.id, 'delete');
    const button = el('button', { type: 'button', class: 'small danger', 'data-note-delete': note.id }, 'Delete');
    const item = el('li', { class: 'note', 'data-note-id': note.id },
      el('p', { class: 'note-text' }, typeof note.text === 'string' ? note.text : ''),
      el('p', { class: 'muted note-meta' },
        (author.tokenName || 'an unnamed credential') + ', ' + formatTime(note.createdAt), ' ', remove.node, ' ', button));
    button.addEventListener('click', async () => {
      if (!remove.checked) {
        problem.show(new ApiError(0, 'confirm', 'tick delete beside the note first: a deleted note is gone, and only the audit log remembers it was there'));
        return;
      }
      button.disabled = true;
      try {
        await api('DELETE', identityPath(platform, id, '/notes/' + encodeURIComponent(note.id)));
        if (stale(seq)) return;
        item.remove();
        rendered.delete(String(note.id));
        deleted.add(String(note.id));
        written.delete(String(note.id));
        empty.hidden = rendered.size > 0;
        problem.hide();
      } catch (err) {
        if (stale(seq) || onUnauthorized(err)) return;
        problem.show(err);
      } finally {
        button.disabled = false;
      }
    });
    return item;
  };

  const fetchPage = async (from) => {
    older.disabled = true;
    try {
      const page = await api('GET', identityPath(platform, id, '/notes' + query(from ? { cursor: from } : {})));
      if (stale(seq)) return;
      readState = 'readable';
      for (const note of Array.isArray(page.notes) ? page.notes : []) {
        if (note && note.id !== undefined) place(note, false);
      }
      cursor = page.nextCursor || null;
      older.hidden = !cursor;
      empty.hidden = rendered.size > 0;
    } catch (err) {
      if (stale(seq) || onUnauthorized(err)) return;
      if (isForbidden(err)) {
        readState = 'forbidden';
        rendered.clear();
        clear(listArea);
        listArea.append(refusedNotice('notes-forbidden', 'notes:read', err));
        // A write that landed while the read was out was drawn in the list
        // this refusal just cleared; the notice keeps its confirmation.
        showSaved();
        return;
      }
      problem.show(err);
    } finally {
      older.disabled = false;
    }
  };
  older.addEventListener('click', () => { if (cursor) fetchPage(cursor); });

  const form = el('form', {
    class: 'stack', id: 'note-form', novalidate: true,
    onsubmit: async (event) => {
      event.preventDefault();
      problem.hide();
      saved.hidden = true;
      if (text.value.trim() === '') {
        problem.show(new ApiError(0, 'bad_request', 'a note needs some text'));
        return;
      }
      if ([...text.value].length > NOTE_MAX) {
        problem.show(new ApiError(0, 'bad_request', 'a note holds at most ' + NOTE_MAX + ' characters; this one has ' + [...text.value].length));
        return;
      }
      const owner = token();
      add.disabled = true;
      try {
        const created = await api('POST', identityPath(platform, id, '/notes'), { text: text.value });
        if (stale(seq) || token() !== owner) return;
        written.add(String(created.note.id));
        if (readState === 'forbidden') {
          showSaved();
        } else {
          place(created.note, true);
        }
        text.value = '';
      } catch (err) {
        if (stale(seq) || onUnauthorized(err)) return;
        problem.show(err);
      } finally {
        add.disabled = false;
      }
    },
  },
  el('label', { class: 'field', for: 'note-text' }, el('span', { class: 'name' }, 'New note'), text,
    el('span', { class: 'hint' }, 'Credited to the token signed in here, under its current name. Needs notes:write.')),
  el('div', { class: 'actions-row' }, add));

  fetchPage(null);
  return el('section', { class: 'card', id: 'notes-section' }, el('h2', {}, 'Notes'), body, form);
}

// A path segment of exactly "." or ".." is a dot segment, and the browser
// removes it from a fetch URL, its %2e form included, so this page cannot
// reach the profile of an identity with one; the hub can be asked by a
// client that sends %2e as written (protocol section 8.6).
function unaddressable(member) {
  return member === '.' || member === '..';
}

export async function viewPlayer(app, route, seq) {
  const { platform, id } = route;
  const identity = platform + ':' + id;
  setCrumbs([{ label: 'Players', href: playersHref() }, { label: identity }]);
  if (unaddressable(platform) || unaddressable(id)) {
    clear(app);
    app.append(el('p', { class: 'notice', id: 'player-unaddressable' },
      'The identity ', el('span', { class: 'mono' }, identity),
      ' has a member that is a dot segment ("." or ".."). Browsers remove such a segment from a URL before sending it, so this page cannot ask the hub for the profile; a client that sends the dots percent-encoded (%2e) can (protocol section 8.6).'));
    return;
  }
  const names = await serverNames();
  if (stale(seq)) return;
  clear(app);

  const title = el('h1', { id: 'player-title' }, identity);
  app.append(el('div', { class: 'card', id: 'player-summary' },
    title,
    el('dl', { class: 'kv' },
      el('dt', {}, 'Platform'), el('dd', { class: 'mono', id: 'player-platform' }, platform),
      el('dt', {}, 'Id'), el('dd', { class: 'mono', id: 'player-id' }, id)),
    el('p', { class: 'muted', id: 'player-window' }, RETENTION_NOTE)));

  app.append(notesSection(platform, id, seq));

  let named = false;
  app.append(pagedSection({
    id: 'player-events', heading: 'Events on every server', scope: 'events:read', seq,
    columns: ['When', 'Server', 'Type', 'Roles', 'Data'],
    emptyText: 'No event the hub still holds refers to this identity.',
    load: (cursor) => api('GET', identityPath(platform, id, '/events' + query(
      Object.assign({ limit: String(PROFILE_PAGE_SIZE) }, cursor ? { cursor } : {})))),
    itemsOf: (page) => {
      const events = Array.isArray(page.events) ? page.events : [];
      if (!named) {
        const name = latestName(events);
        if (name) {
          named = true;
          clear(title);
          title.append(name, ' ', el('span', { class: 'muted title-tail mono' }, identity));
        }
      }
      return events;
    },
    rowOf: (event) => {
      const data = event.data && typeof event.data === 'object' && !Array.isArray(event.data) ? event.data : {};
      const roles = Array.isArray(event.roles) ? event.roles : [];
      return el('tr', { 'data-event-id': event.id, 'data-event-type': String(event.type) },
        el('td', { class: 'when', title: 'received ' + formatTime(event.receivedAt) }, formatTime(event.occurredAt)),
        el('td', {}, serverLabel(names, event.serverId)),
        el('td', {}, el('span', { class: 'mono type' }, String(event.type))),
        el('td', {}, el('span', { class: 'badges' }, roles.map((role) => badge(role)))),
        el('td', { class: 'data' },
          Object.keys(data).length > 0
            ? disclosure(attempt(() => summarizeEventData(data), 'The data is nested too deeply to summarize.'), data)
            : el('span', { class: 'muted' }, 'no data'),
          identityLinks(data)));
    },
  }));

  app.append(pagedSection({
    id: 'player-actions', heading: 'Actions against this player', scope: 'actions:read', seq,
    columns: ['Created', 'Server', 'Action', 'State', 'Result'],
    emptyText: 'No player action names this id. Actions match on the id alone, since a player action’s target carries no platform.',
    load: (cursor) => api('GET', identityPath(platform, id, '/actions' + query(
      Object.assign({ limit: String(PROFILE_PAGE_SIZE) }, cursor ? { cursor } : {})))),
    itemsOf: (page) => (Array.isArray(page.actions) ? page.actions : []),
    rowOf: (action) => {
      const outcome = action.error
        ? el('span', {}, String(action.error))
        : action.result !== null && action.result !== undefined
          ? disclosure(attempt(() => summarizeEventData(action.result) || 'result', 'result'), action.result)
          : el('span', { class: 'muted' }, 'none yet');
      return el('tr', { 'data-action-id': action.id },
        el('td', {}, formatTime(action.createdAt)),
        el('td', {}, serverLabel(names, action.serverId)),
        el('td', {}, el('span', { class: 'mono' }, String(action.code))),
        el('td', {}, badge(String(action.state), String(action.state))),
        el('td', { class: 'data' }, outcome));
    },
  }));
}

// viewPlayers is the way into a profile: an identity typed in, or picked
// from who is online on each server the token can read right now.
export async function viewPlayers(app, route, seq) {
  setCrumbs([{ label: 'Players' }]);
  clear(app);
  const platform = el('input', { type: 'text', id: 'lookup-platform', name: 'lookup-platform', value: 'steam', spellcheck: 'false', autocomplete: 'off' });
  const id = el('input', { type: 'text', id: 'lookup-id', name: 'lookup-id', spellcheck: 'false', autocomplete: 'off', placeholder: '76561198000000000' });
  const problem = problemBox('lookup-error');
  const form = el('form', {
    class: 'stack card', id: 'player-lookup', novalidate: true,
    onsubmit: (event) => {
      event.preventDefault();
      if (platform.value.trim() === '' || id.value.trim() === '') {
        problem.show(new ApiError(0, 'bad_request', 'both the platform and the id are needed'));
        return;
      }
      go(playerHref(platform.value.trim(), id.value.trim()));
    },
  },
  el('h1', {}, 'Players'),
  el('p', { class: 'muted' }, 'A profile is one identity across every server: the events that name it, the actions against it, and the notes kept on it (protocol section 8.6).'),
  problem.node,
  el('label', { class: 'field', for: 'lookup-platform' }, el('span', { class: 'name' }, 'Platform'), platform),
  el('label', { class: 'field', for: 'lookup-id' }, el('span', { class: 'name' }, 'Id'), id),
  el('div', { class: 'actions-row' }, el('button', { type: 'submit', class: 'primary', id: 'lookup-open' }, 'Open profile')));
  const online = el('section', { class: 'card', id: 'players-online' }, el('h2', {}, 'Online now'),
    el('p', { class: 'muted', id: 'players-online-status' }, 'Reading each server’s latest player list…'));
  app.append(form, online);

  let servers = [];
  try {
    const data = await api('GET', '/servers');
    servers = Array.isArray(data.servers) ? data.servers : [];
  } catch (err) {
    if (stale(seq) || onUnauthorized(err)) return;
    clear(online);
    online.append(el('h2', {}, 'Online now'), refusedNotice('players-online-forbidden', 'servers:read', err));
    return;
  }
  const rows = [];
  await Promise.all(servers.map(async (server) => {
    try {
      const snapshot = await api('GET', '/servers/' + encodeURIComponent(server.id) + '/state/players');
      const entries = snapshot && snapshot.snapshot && Array.isArray(snapshot.snapshot.players) ? snapshot.snapshot.players : [];
      for (const entry of entries) {
        if (!entry || !entry.player || typeof entry.player.id !== 'string' || typeof entry.player.platform !== 'string') continue;
        rows.push({ server, platform: entry.player.platform, id: entry.player.id, name: typeof entry.name === 'string' ? entry.name : '' });
      }
    } catch (err) {
      // A read from a view that has gone, or under a token since replaced,
      // must not sign out whoever is signed in now.
      if (!stale(seq)) onUnauthorized(err);
    }
  }));
  if (stale(seq)) return;
  clear(online);
  online.append(el('h2', {}, 'Online now'));
  if (rows.length === 0) {
    online.append(el('p', { class: 'muted', id: 'players-online-empty' }, 'No server this token can read reports anyone online.'));
    return;
  }
  rows.sort((a, b) => (a.name || a.id).localeCompare(b.name || b.id));
  online.append(el('table', { id: 'players-online-table' },
    el('thead', {}, el('tr', {}, el('th', {}, 'Name'), el('th', {}, 'Identity'), el('th', {}, 'Server'))),
    el('tbody', {}, rows.map((row) => el('tr', { 'data-player-id': row.id },
      el('td', {}, el('a', { href: playerHref(row.platform, row.id) }, row.name || row.id)),
      el('td', { class: 'mono' }, row.platform + ':' + row.id),
      el('td', {}, el('a', { href: serverHref(row.server.id) }, row.server.name)))))));
}
