# Panel

The optional web UI of the reference hub, embedded in the `vyshka-hub` binary and served at
`/panel/` (a browser at `/` is redirected there). It is a thin client over the Admin API: it
may not reach into hub internals, and anything it can do must be possible with `curl` against
`/api/v1` alone. Action forms are rendered from the plugin manifest rather than hand-written
per game, which is what keeps the hub game-agnostic.

Tracked in issue #13 (panel v1), #47 (the event feed), and #46 (the live map).

## What it does

- **Sign in** with an Admin API token. The token lives in the tab's `sessionStorage` and
  travels to the hub as a bearer token on every request; closing the tab forgets it, and
  "Sign out" forgets it sooner. The hub's own scopes apply unchanged: a token narrowed to
  `servers:read` sees the server list and nothing dispatches.
- **Server list** with link state, credential state, enrolled plugin, last-seen time, and the
  count of envelopes queued for delivery, refreshed every five seconds.
- **Actions** from the server's stored manifest, grouped by namespace, with context and
  danger badges.
- **Forms generated from the manifest schema** (protocol section 6.1's JSON Schema subset):
  `enum` becomes a select, `boolean` a checkbox (or a not-set/true/false select when
  optional with no default, so the key can stay absent), `integer` and `number` numeric
  inputs with their bounds (an integer's exclusive or fractional bounds rounded inward onto
  the input; a real number's exclusive bound left to the hub), `string` a text input,
  `array` a one-value-per-line textarea, nested `object` a fieldset, and anything without
  `properties` a JSON textarea. `required` is enforced and `default` prefilled. An optional
  field left empty is omitted from `params`, not sent as an empty string. An optional object
  has an include checkbox in its legend: it ticks itself when anything inside is entered, can
  be ticked by hand to send an object whose only values are its initial ones, and left
  unticked omits the object whole whatever it requires of its children. String
  array items are taken as typed, whitespace included; an empty-string item cannot be
  expressed in the one-per-line form.
- **`x-vyshka-widget` hints** shape the input and never its validation: `player` offers
  the identities from the latest `state.players` snapshot as suggestions, `vector` renders
  x, y, z inputs for an array of numbers (z may be left blank for a flat position, and a
  blank coordinate is never a zero), `webhook` a text input with a URL keyboard, `itemlist`
  a text input with a hint. Unknown hints fall back to the field's type, as the protocol
  requires.
- **Targets**: a `player` context action gets a required player field fed by the same
  snapshot; `vehicle` and `object` get an id field; custom contexts get an optional
  reference field (enumeration arrives with custom contexts, milestone M5).
- **Danger** (`warning`, `destructive`) requires an explicit confirmation checkbox before the
  Dispatch button does anything.
- **Dispatch and live result**: one `POST /api/v1/servers/{id}/actions` with an idempotency
  key bound to the exact request (resending the same request after a lost answer reuses it
  and gets the same action back; an edited request gets a fresh key, and so does the next
  dispatch after an accepted one), then `GET /api/v1/actions/{id}` every second until the action
  reaches a terminal state, drawn as a timeline with the result payload or error. A
  `params_invalid` refusal lands on the field the hub named.
- **Event feed** per server, at `#/servers/{id}/events`, over
  `GET /api/v1/servers/{id}/events` (protocol section 8.5): newest first in the hub's own
  order, a type filter in the hub's grammar (exact types, `{namespace}.*` patterns, `*`,
  several at once), paging behind the hub's cursor with a "Load older" button, and a follow
  mode, on by default, that re-reads the first page every five seconds and merges what is
  new into place by id. Following re-reads the page rather than asking for events since the
  newest one because the feed is ordered by the game server's clock, so a late batch can
  land below events already shown; a late event that lands below the newest hundred is
  picked up by a reload or an older page. The filter and the follow switch live in the
  route, so a reload keeps them and the URL can be shared. Each row shows `occurredAt`
  (with `receivedAt` on hover), the type with a core or custom badge and, for a custom
  type the manifest declares, its display name, and the event's `data` as one line of text
  with the full JSON behind a disclosure, serialized when the disclosure is first opened and
  compact rather than indented past 64 levels of nesting (the hub bounds data in bytes, not
  depth, and indentation grows with the square of the depth); a row that cannot be built
  is one row's problem, not the feed's. A filter the hub refuses, or one the token's
  `events:read` scope does not cover, is shown beside the form with the hub's code; an
  empty type term in a shared link is sent as it is, so the hub's refusal shows instead of
  a quietly wider feed. The feed keeps the walk's cursor apart from a "gap" cursor a
  follow tick records when the hub holds more below its first page than the feed has
  walked: when the page ends in an event the feed had not seen (more than a page of new
  events landed), or when the page carries a cursor where the previous read carried none
  (the hub crossed the page boundary). "Load older" continues the walk first and starts
  from the gap once the walk is done. A page that merely shifted by one new event, or that
  is unchanged over a finished walk, records nothing, so a finished walk is not offered
  again on every new event. What this cannot see is a late event landing below the first
  page of a hub already past the boundary; a reload picks it up.
- **Live map** per server, at `#/servers/{id}/map`, over
  `GET /api/v1/servers/{id}/state/players` (protocol section 8.3) re-read every five
  seconds. The latest snapshot's players are listed with identity, position, and extras,
  and plotted on a basemap when the hub has a tileset installed for the server's world
  (below). The world is the one the plugin reported in its latest `core.server.start`
  event; `?world=` in the route overrides it, for a token without `events:read` or to look
  at another map. A snapshot is whole, so every refresh replaces every marker: a player
  absent from the latest snapshot is gone from the map. The snapshot's `capturedAt` and
  `receivedAt` ages are always on the page, because the five-second re-read says nothing
  about how often the plugin publishes (the DayZ plugin on a held long-poll measured 50 s
  between snapshots with `pollTimeout` 25 and a 10 s snapshot interval). Clicking a marker, or a row's Actions link, opens the server's action
  list with that player preselected (`?player=`): player-context actions open with the
  target field filled in, editable. Positions are read in the game's own frame as the
  manifest says (for DayZ `[x, y, z]` with `y` the elevation, so the map plots `x` east and
  `z` north); a position the manifest cannot read, or a player without one, is listed and
  not plotted. Drag or arrow keys pan, wheel and the `+`/`−` buttons zoom (past the
  imagery's native level too), `0` or Fit shows the whole world.

## Map tilesets

The map's imagery is not in the binary: a world's basemap is tens of megabytes and is
game-specific, so the operator generates it and installs it in a directory the hub is
pointed at with `vyshka-hub serve -maps-dir DIR` (env `VYSHKA_MAPS_DIR`). The hub serves it
under `/panel/maps/`:

| Path | What it is |
|---|---|
| `/panel/maps/` | `{ "worlds": [...] }`, the subdirectories holding a `manifest.json` |
| `/panel/maps/{world}/manifest.json` | the dataset contract below |
| `/panel/maps/{world}/tiles/...` | the tiles the manifest's template names |

Nothing else under a world directory is served (a build leaves large intermediates beside
the tiles), directories are never listed, and `{world}` is one path segment of letters,
digits, dots, dashes, and underscores. A world directory, and its `tiles` directory, may be
a symlink or junction to a dataset built elsewhere; each is opened as a root and the file
opened inside it, so nothing beneath `tiles` can reach outside it, and the manifest and a
tile may not themselves be links (a world whose manifest is one is not listed; the check
runs after the open and requires the named and the opened file to be the same file with the
same size and time, so a swap in between refuses). The one accepted limit: on a filesystem
that reports no file identity, a local writer who installs a manifest linked to a build
intermediate and swaps it for a same-sized, same-timed regular file during a request gets
that intermediate served once. Missing and unreachable files are answered in the protocol's
error shape; an unsatisfiable `Range` or a failed precondition gets Go's plain answer. Tiles are answered with `Cache-Control: public,
max-age=3600`; the index and manifests with `no-cache`. The surface is unauthenticated
like the page itself, so install only imagery you are prepared to serve to anyone who can
reach the hub.

A world directory is named by the world id the plugin reports (`chernarusplus`, `enoch`,
`sakhal` on DayZ) and holds a `manifest.json`:

```json
{
  "schemaVersion": 1,
  "world": "chernarusplus",
  "name": "Chernarus",
  "bounds": { "xmin": 0, "xmax": 15360, "zmin": 0, "zmax": 15360 },
  "raster": { "width": 15360, "height": 15360 },
  "tiles": { "size": 256, "minZoom": 0, "maxZoom": 6, "urlTemplate": "tiles/{z}/{x}/{y}.webp" },
  "axes": { "east": { "index": 0, "name": "x" }, "north": { "index": 2, "name": "z" } }
}
```

`bounds` is the world's extent in the game's map frame; `raster` the native image size, with
`(xmin, zmax)` at its top-left corner (north up) and the raster's `maxZoom` level drawn at
one image pixel per raster pixel. Level `z` is the raster scaled by `2^(z - maxZoom)`, cut
into `size`-pixel tiles from the top-left corner, north-origin XYZ rows, partial edge tiles
padded; `urlTemplate` is resolved relative to the manifest. `axes` says which components of
a position are easting and northing and what to call them; the defaults shown are DayZ's
(and Enfusion's). A two-number position is always read as `[east, north]`. `name` is
optional. Extra members (a build's provenance, validation results) are ignored.

`spikes/chernarus-satellite` builds a Chernarus dataset in this shape from the installed DayZ
assets; copy its `manifest.json` and `tiles/` into `DIR/chernarusplus/`. Its README records
what that dataset's registration has and has not been verified against.

## Security posture

The Go side is a file server that adds response headers. Every panel response carries a
`Content-Security-Policy` confining the page to its own origin with no inline script or
style, `frame-ancestors 'none'`, `X-Content-Type-Options: nosniff`, `Referrer-Policy:
no-referrer`, and `Cache-Control: no-cache`. The JavaScript builds every node with
`createElement` and text nodes; there is no `innerHTML` anywhere, and a test fails if one
appears, because manifest labels, result payloads, event data, and player names are
plugin-supplied text. There are no cookies, so there is nothing for cross-site request
forgery to ride on. Map tilesets are served with the same headers and no authentication;
they are operator-installed static files, never anything the hub holds about a server.

Disable the panel with `vyshka-hub serve -panel=false` (env `VYSHKA_PANEL=false`); the hub
then serves nothing at `/panel/` and `/` is an ordinary 404.

## Layout

```
panel/
  panel.go         // http.Handler over the embedded files and the maps directory, plus the security headers
  static/
    index.html     // the shell: header, breadcrumbs, one <main>
    app.js         // routing, the Admin API client, the form builder, dispatch and result, the event feed, the map view
    map.js         // the map widget: tile pyramid on a canvas, markers as buttons, the world frame
    style.css      // one stylesheet, light and dark
  panel_test.go    // the handler: headers, what it serves, what it refuses, the maps surface
  e2e_test.go      // headless Chrome against a real hub, a fake plugin, and a generated tileset
```

No build step: the files are served as written. The hub takes the handler through
`hub.Config.Panel` (`panel.NewHandler(panel.Config{MapsDir: ...})`, or `panel.Handler()`
for no maps) and does not import this package, so an embedder can mount a different panel
or none.

## Tests

`go test ./panel/` runs the handler tests and, when it finds a Chromium-family browser
(Chrome, Chromium, or Edge on Windows), the end-to-end test: it boots a hub with the panel, a
fake plugin that publishes a manifest and a players snapshot, and drives headless Chrome
through sign-in (a bad token first), the server list, the action list, the generated form
(asserting the inputs' bounds, defaults, options, and the player suggestions), a dispatch the
hub refuses with `params_invalid` (the fault must land on its field and the plugin must never
see it), a corrected dispatch that round-trips to `completed` with the plugin's result on
the page, and then the event feed: a batch listed out of order on the wire shown in the
hub's order, the declared name and markup-shaped data as text, the type filter and its
place in the route, follow mode merging a late event below the ones already shown, a paused
feed that does not move while a new event lands in the hub, a 2500-deep payload rendered
bounded, paging through 158 events with no event shown twice, a feed of exactly one page
offering the walk when a late older event gives its unchanged page a cursor, and a burst
of 150 new events on a walked feed joined to its history in two pages. Then the live map,
on a three-level tileset the test generates (a 1024 m world painted in sixteen flat colours
by raster quadrant): the world picked from the plugin's start event, four players listed
with one lacking a position and one in the flat two-number form, three markers placed
within 1.5 px of where the world frame puts them with the canvas pixel under each the
colour of its quadrant (which is what tells a flipped or swapped axis from a right one),
a second snapshot replacing the markers whole, a marker click landing on the action list
with the player preselected and the heal form filled in, and a world with no tileset
listing the players under a notice.

- `VYSHKA_E2E=required` fails instead of skipping when no browser is found (CI sets it).
- `VYSHKA_E2E_BROWSER=/path/to/chrome` names the executable.
- `VYSHKA_E2E_SCREENSHOT=/path/to.png` writes a screenshot of the completed dispatch.

## Trying it

```
go build -o bin/vyshka-hub ./hub/cmd/vyshka-hub
VYSHKA_ADMIN_TOKEN=vya_local_dev_token ./bin/vyshka-hub serve
VYSHKA_ADMIN_TOKEN=vya_local_dev_token scripts/demo-panel.sh   # a fake plugin to click against
```

Then open <http://127.0.0.1:8080/> and sign in with the token. With the DayZ plugin from
`plugins/dayz` enrolled instead of the demo plugin, the same page dispatches `vyshka.heal` to
a live server: pick the server, pick the action, choose or type the player's Steam64 id, or
open the live map and click the player. To see the demo plugin's players on imagery, build
the Chernarus dataset (`spikes/chernarus-satellite`), install it under a maps directory as
described above, and start the hub with `-maps-dir`.
