// The package's own behavior against a fake fetch: what it sends, what it
// refuses to send, and how it reads an answer.

import assert from "node:assert/strict";
import { test } from "node:test";
import {
  AdminApiError,
  OPENAPI_VERSION,
  PROTOCOL_DRAFT,
  createAdminClient,
  isTerminal,
  unwrap,
  waitForAction,
  type ActionRecord,
} from "../dist/index.js";

const token = "vya_TSCLIENTTESTTOKEN";

interface Seen {
  method: string;
  url: string;
  redirect: RequestRedirect;
  headers: Headers;
  body: string;
}

/** A fetch that records each request and answers with respond's answer. */
function fakeFetch(respond: (request: Request) => Response | Promise<Response>) {
  const seen: Seen[] = [];
  const fetch = async (request: Request): Promise<Response> => {
    seen.push({
      method: request.method,
      url: request.url,
      redirect: request.redirect,
      headers: request.headers,
      body: await request.clone().text(),
    });
    return respond(request);
  };
  return { seen, fetch };
}

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

test("the constructor refuses a URL or token that cannot work", () => {
  const cases: [string, string, RegExp][] = [
    ["", token, /URL is empty/],
    ["ftp://hub.example", token, /http:\/\/ or https:\/\//],
    ["http://hub.example/?x=1", token, /query or a fragment/],
    ["http://hub.example/#top", token, /query or a fragment/],
    ["not a url", token, /does not parse/],
    ["http://hub.example", "", /token is empty/],
    ["http://hub.example", "vya_abc def", /whitespace or a control character/],
    ["http://hub.example", "vya_abc\n", /whitespace or a control character/],
  ];
  for (const [baseUrl, t, message] of cases) {
    assert.throws(() => createAdminClient({ baseUrl, token: t }), message, `${baseUrl} / ${JSON.stringify(t)}`);
  }
});

test("a request carries the token, asks for JSON, keeps the base prefix, and does not follow redirects", async () => {
  const { seen, fetch } = fakeFetch(() => json(200, { servers: [] }));
  const client = createAdminClient({ baseUrl: "http://hub.example/tenant/", token, fetch });
  await unwrap(client.GET("/api/v1/servers"));
  const [request] = seen;
  assert.ok(request);
  assert.equal(request.url, "http://hub.example/tenant/api/v1/servers");
  assert.equal(request.headers.get("Authorization"), `Bearer ${token}`);
  assert.equal(request.headers.get("Accept"), "application/json");
  assert.equal(request.redirect, "manual");
});

test("the caller's headers cannot replace the token", async () => {
  const { seen, fetch } = fakeFetch(() => json(200, { servers: [] }));
  const client = createAdminClient({
    baseUrl: "http://hub.example",
    token,
    fetch,
    headers: { Authorization: "Bearer someone-else", authorization: "Bearer another", "X-Trace": "1" },
  });
  await unwrap(client.GET("/api/v1/servers"));
  assert.equal(seen[0]?.headers.get("Authorization"), `Bearer ${token}`);
  assert.equal(seen[0]?.headers.get("X-Trace"), "1");
});

test("path parameters travel as one encoded segment each", async () => {
  const { seen, fetch } = fakeFetch(() => json(200, { notes: [] }));
  const client = createAdminClient({ baseUrl: "http://hub.example", token, fetch });
  await unwrap(
    client.GET("/api/v1/players/{platform}/{playerId}/notes", {
      params: { path: { platform: "steam", playerId: "a/b?c#d e" } },
    }),
  );
  assert.equal(seen[0]?.url, "http://hub.example/api/v1/players/steam/a%2Fb%3Fc%23d%20e/notes");
});

test("a path parameter that would not stay one segment is refused before anything is sent", async () => {
  const { seen, fetch } = fakeFetch(() => json(200, {}));
  const client = createAdminClient({ baseUrl: "http://hub.example", token, fetch });
  for (const playerId of [".", "..", ""]) {
    await assert.rejects(
      client.GET("/api/v1/players/{platform}/{playerId}/events", { params: { path: { platform: "steam", playerId } } }),
      TypeError,
      JSON.stringify(playerId),
    );
  }
  await assert.rejects(
    // A caller outside TypeScript can leave a parameter out.
    client.GET("/api/v1/servers/{serverId}", { params: { path: {} as { serverId: string } } }),
    /serverId is missing/,
  );
  assert.equal(seen.length, 0, "nothing reached the wire");
});

test("query arrays repeat the parameter, one term each", async () => {
  const { seen, fetch } = fakeFetch(() => json(200, { events: [] }));
  const client = createAdminClient({ baseUrl: "http://hub.example", token, fetch });
  await unwrap(
    client.GET("/api/v1/servers/{serverId}/events", {
      params: { path: { serverId: "s1" }, query: { type: ["core.*", "mod.raid"], limit: 2 } },
    }),
  );
  assert.equal(seen[0]?.url, "http://hub.example/api/v1/servers/s1/events?type=core.*&type=mod.raid&limit=2");
});

test("a request body travels as JSON", async () => {
  const { seen, fetch } = fakeFetch(() => json(201, { server: {}, enrollment: {} }));
  const client = createAdminClient({ baseUrl: "http://hub.example", token, fetch });
  await unwrap(client.POST("/api/v1/servers", { body: { name: "Chernarus #1", game: "dayz" } }));
  assert.equal(seen[0]?.method, "POST");
  assert.equal(seen[0]?.headers.get("Content-Type"), "application/json");
  assert.deepEqual(JSON.parse(seen[0]?.body ?? ""), { name: "Chernarus #1", game: "dayz" });
});

test("unwrap throws the protocol error with its code and details", async () => {
  const { fetch } = fakeFetch(() =>
    json(409, { error: { code: "revision_mismatch", message: "the key moved", details: { revision: 4 } } }),
  );
  const client = createAdminClient({ baseUrl: "http://hub.example", token, fetch });
  await assert.rejects(
    unwrap(client.PUT("/api/v1/kv/{namespace}/{key}", { params: { path: { namespace: "m", key: "k" } }, body: { value: 1, ifRevision: 3 } })),
    (err) => {
      assert.ok(err instanceof AdminApiError);
      assert.equal(err.status, 409);
      assert.equal(err.code, "revision_mismatch");
      assert.equal(err.message, "revision_mismatch (409): the key moved");
      assert.deepEqual(err.details, { revision: 4 });
      return true;
    },
  );
});

test("an answer without the protocol error shape has an empty code", async () => {
  const { fetch } = fakeFetch(() => new Response("<html>bad gateway</html>", { status: 502, statusText: "Bad Gateway" }));
  const client = createAdminClient({ baseUrl: "http://hub.example", token, fetch });
  await assert.rejects(unwrap(client.GET("/api/v1/servers")), (err) => {
    assert.ok(err instanceof AdminApiError);
    assert.equal(err.status, 502);
    assert.equal(err.code, "");
    assert.equal(err.message, "Bad Gateway (502)");
    return true;
  });
});

test("a redirect is reported, not followed", async () => {
  const { seen, fetch } = fakeFetch(
    () => new Response(null, { status: 308, headers: { Location: "https://hub.example/api/v1/servers" } }),
  );
  const client = createAdminClient({ baseUrl: "http://hub.example", token, fetch });
  await assert.rejects(unwrap(client.GET("/api/v1/servers")), (err) => {
    assert.ok(err instanceof AdminApiError);
    assert.equal(err.status, 308);
    assert.match(err.message, /redirected to https:\/\/hub\.example\/api\/v1\/servers, which this client does not follow/);
    return true;
  });
  assert.equal(seen.length, 1);
});

test("a 204 unwraps to undefined", async () => {
  const { fetch } = fakeFetch(() => new Response(null, { status: 204 }));
  const client = createAdminClient({ baseUrl: "http://hub.example", token, fetch });
  const answer = await unwrap(client.DELETE("/api/v1/tokens/{tokenId}", { params: { path: { tokenId: "t1" } } }));
  assert.equal(answer, undefined);
});

function actionRecord(state: ActionRecord["state"]): ActionRecord {
  return {
    id: "a1",
    serverId: "s1",
    code: "mod.heal",
    params: {},
    state,
    createdAt: "2026-09-30T00:00:00Z",
    expiresAt: "2026-09-30T00:02:00Z",
  };
}

test("waitForAction follows the action to a terminal state and reports each change once", async () => {
  const states: ActionRecord["state"][] = ["queued", "queued", "delivered", "running", "running", "completed"];
  let reads = 0;
  const { fetch } = fakeFetch(() => json(200, actionRecord(states[Math.min(reads++, states.length - 1)]!)));
  const client = createAdminClient({ baseUrl: "http://hub.example", token, fetch });
  const seen: string[] = [];
  const done = await waitForAction(client, "a1", { intervalMs: 1, onChange: (a) => seen.push(a.state) });
  assert.equal(done.state, "completed");
  assert.deepEqual(seen, ["queued", "delivered", "running", "completed"]);
  assert.equal(reads, states.length);
});

test("waitForAction stops when its signal ends and rejects with the signal's reason", async () => {
  const { fetch } = fakeFetch(() => json(200, actionRecord("running")));
  const client = createAdminClient({ baseUrl: "http://hub.example", token, fetch });
  const signal = AbortSignal.timeout(50);
  await assert.rejects(waitForAction(client, "a1", { intervalMs: 5, signal }), (err) => {
    assert.equal((err as Error).name, "TimeoutError");
    return true;
  });
});

test("waitForAction surfaces a refusal", async () => {
  const { fetch } = fakeFetch(() => json(403, { error: { code: "forbidden", message: "no actions:read" } }));
  const client = createAdminClient({ baseUrl: "http://hub.example", token, fetch });
  await assert.rejects(waitForAction(client, "a1", { intervalMs: 1 }), (err) => err instanceof AdminApiError && err.code === "forbidden");
});

test("terminal states are completed, failed, and expired", () => {
  assert.deepEqual(
    ["queued", "delivered", "running", "completed", "failed", "expired", "paused"].filter(isTerminal),
    ["completed", "failed", "expired"],
  );
});

test("the package names the documents it was generated from", () => {
  assert.match(PROTOCOL_DRAFT, /^\d+\.\d+$/);
  assert.match(OPENAPI_VERSION, /^\d+\.\d+\.\d+$/);
});
