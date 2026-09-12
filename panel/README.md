# Panel

The optional web UI of the reference hub, embedded in the `vyshka-hub` binary and served at
`/panel/` (a browser at `/` is redirected there). It is a thin client over the Admin API: it
may not reach into hub internals, and anything it can do must be possible with `curl` against
`/api/v1` alone. Action forms are rendered from the plugin manifest rather than hand-written
per game, which is what keeps the hub game-agnostic.

Tracked in issue #13 (panel v1) and #47 (the event feed). The live map view is a follow-up
ticket (#46).

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
  with the full JSON behind a disclosure. A filter the hub refuses, or one the token's
  `events:read` scope does not cover, is shown beside the form with the hub's code.

## Security posture

The Go side is a file server that adds response headers. Every panel response carries a
`Content-Security-Policy` confining the page to its own origin with no inline script or
style, `frame-ancestors 'none'`, `X-Content-Type-Options: nosniff`, `Referrer-Policy:
no-referrer`, and `Cache-Control: no-cache`. The JavaScript builds every node with
`createElement` and text nodes; there is no `innerHTML` anywhere, and a test fails if one
appears, because manifest labels, result payloads, and event data are plugin-supplied text.
There are no cookies, so there is nothing for cross-site request forgery to ride on.

Disable the panel with `vyshka-hub serve -panel=false` (env `VYSHKA_PANEL=false`); the hub
then serves nothing at `/panel/` and `/` is an ordinary 404.

## Layout

```
panel/
  panel.go         // http.Handler over the embedded files, plus the security headers
  static/
    index.html     // the shell: header, breadcrumbs, one <main>
    app.js         // routing, the Admin API client, the form builder, dispatch and result, the event feed
    style.css      // one stylesheet, light and dark
  panel_test.go    // the handler: headers, what it serves, what it refuses
  e2e_test.go      // headless Chrome against a real hub and a fake plugin
```

No build step: the files are served as written. The hub takes the handler through
`hub.Config.Panel` and does not import this package, so an embedder can mount a different
panel or none.

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
feed that does not move while a new event lands in the hub, and paging through 157 events
with no event shown twice.

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
a live server: pick the server, pick the action, choose or type the player's Steam64 id.
