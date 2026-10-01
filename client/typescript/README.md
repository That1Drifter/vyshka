# @vyshka/admin-client

A TypeScript client for the Admin API of a Vyshka hub, the operator-facing half of the
protocol (`spec/protocol.md`). Its types are generated from `spec/openapi-admin.yaml` by
[openapi-typescript](https://openapi-ts.dev), and its requests go through
[openapi-fetch](https://openapi-ts.dev/openapi-fetch/), so every path, parameter, body, and
answer is checked against the document at compile time. A Discord bot, a dashboard, or a
script is a client like any other; this is the package that makes writing one in
TypeScript cheap.

It runs anywhere `fetch` does: Node.js 20 or later, Deno, Bun, and browsers. Its one
runtime dependency is openapi-fetch.

## Install

The package is released with the hub, under the same version, as a tarball on the hub's
GitHub release:

```
npm install https://github.com/That1Drifter/vyshka/releases/download/hub-v0.3.0/vyshka-admin-client-0.3.0.tgz
```

It is not on the npm registry. From a clone, `npm ci && npm run build` in
`client/typescript` builds it, and `npm install <path to client/typescript>` installs it.

## Use

```ts
import { AdminApiError, createAdminClient, unwrap, waitForAction } from "@vyshka/admin-client";

const hub = createAdminClient({ baseUrl: "https://hub.example.net", token: process.env.VYSHKA_TOKEN! });

// Every request is openapi-fetch's: the path is a literal of the document, and the
// params and body are typed from it.
const { servers } = await unwrap(hub.GET("/api/v1/servers"));

const { actionId } = await unwrap(
  hub.POST("/api/v1/servers/{serverId}/actions", {
    params: { path: { serverId: servers[0]!.id } },
    body: { code: "vyshka.heal", context: "player", referenceKey: "76561198000000000" },
  }),
);
const action = await waitForAction(hub, actionId, { signal: AbortSignal.timeout(60_000) });
console.log(action.state, action.result);

try {
  await unwrap(hub.GET("/api/v1/bans/{banId}", { params: { path: { banId: "nope" } } }));
} catch (err) {
  if (err instanceof AdminApiError && err.code === "not_found") {
    // branch on err.code, the protocol's stable error code (spec section 2.2)
  }
}
```

What the package adds to openapi-fetch:

- `createAdminClient({ baseUrl, token, fetch?, headers? })` refuses a URL that is not
  http(s) or carries a query or fragment, and a token that is empty or holds whitespace
  or a control character. It sends the token on every request and does not follow
  redirects, so a misconfigured URL fails instead of answering from somewhere else.
- `unwrap(request)` resolves to the answer's body (`undefined` for a 204) and throws an
  `AdminApiError` for anything outside 2xx: `status`, the protocol error `code`, and its
  `details`. An answer without the protocol error shape (a proxy's error page, a
  redirect) has an empty `code`. Without `unwrap`, a request resolves to openapi-fetch's
  `{ data, error, response }`.
- A path parameter that is empty, `.`, or `..` is refused before anything is sent. The
  URL parser of every `fetch` removes dot segments, percent-encoded ones included, so the
  request would reach a different route. A player identity that is exactly `.` or `..` is
  legal in the protocol (spec section 8.6) and needs a client that can send `%2e`; the Go
  client is one.
- `waitForAction(client, actionId, { intervalMs?, signal?, onChange? })` reads an action
  until it is completed, failed, or expired.
- Every schema of the document is exported as a type by its own name (`ServerRecord`,
  `ActionRecord`, `KVEntry`, ...; the error body is `ErrorBody`), with `paths`,
  `components`, and `operations` for anything else.

## Versions

The package carries the hub's version and is built from the same tag. Two constants name
what it was generated from: `PROTOCOL_DRAFT`, the draft of `spec/protocol.md`, and
`OPENAPI_VERSION`, the `info.version` of `spec/openapi-admin.yaml`.

A newer hub stays usable. The types are compile-time only: a member the hub added arrives
in the object and is simply not typed, and an enum-like value a later draft added (an
action state, a link state) arrives as the string it is, outside the union the type names.
Code that switches over such a union should keep a default branch.

## Development

```
npm ci
npm run generate         # rewrite src/schema.ts and src/version.ts from the spec
npm run check-generated  # fail when they differ from what the spec generates (CI)
npm run typecheck
npm test
```

`npm test` builds the package and runs `test/`. The unit tests drive a fake `fetch`. The
live test (`test/live.test.ts`) calls every operation the document declares against a
running hub, with the conformance suite's reference plugin driver as the game server, and
grades every answer against the document, including members the document does not
declare; it fails if any operation went uncalled. It needs `VYSHKA_TEST_URL` and
`VYSHKA_TEST_TOKEN` (an `admin` token), and Go on the `PATH` unless `VYSHKA_TEST_DRIVER`
names a built driver. It skips without them, unless `VYSHKA_TEST_LIVE=required`, which CI
sets.

```
go build -o bin/vyshka-hub ./hub/cmd/vyshka-hub && go build -o bin/conformance-driver ./conformance/plugin/driver
VYSHKA_ADMIN_TOKEN=vya_local_test ./bin/vyshka-hub serve -addr 127.0.0.1:8090 -db /tmp/ts.db &
cd client/typescript
VYSHKA_TEST_URL=http://127.0.0.1:8090 VYSHKA_TEST_TOKEN=vya_local_test \
  VYSHKA_TEST_DRIVER=../../bin/conformance-driver npm test
```

A change to `spec/openapi-admin.yaml` is followed by `npm run generate` in the same
commit, or CI fails the freshness check.
