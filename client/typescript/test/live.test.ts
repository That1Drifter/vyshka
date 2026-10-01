// The package against a live hub, every operation in spec/openapi-admin.yaml
// called through it at least once, and every answer graded against the
// document (openapi-check.ts). The conformance suite's reference plugin
// driver plays the game server, so the manifest, actions, contexts, events,
// state, and the ban list all have something real behind them.
//
// Needs VYSHKA_TEST_URL and VYSHKA_TEST_TOKEN (an `admin` token), and Go on
// the PATH for the driver unless VYSHKA_TEST_DRIVER names a built one. It
// skips without them unless VYSHKA_TEST_LIVE=required, which CI sets so that a
// missing hub fails the job instead of skipping the one test that grades it.

import assert from "node:assert/strict";
import { spawn, type ChildProcess } from "node:child_process";
import { once } from "node:events";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import {
  AdminApiError,
  createAdminClient,
  unwrap,
  waitForAction,
  type AdminClient,
  type WebhookDelivery,
} from "../dist/index.js";
import { OpenAPICheck } from "./openapi-check.ts";

const hubURL = process.env.VYSHKA_TEST_URL ?? "";
const adminToken = process.env.VYSHKA_TEST_TOKEN ?? "";
const required = process.env.VYSHKA_TEST_LIVE === "required";
const repoRoot = fileURLToPath(new URL("../../../", import.meta.url));

const skip = hubURL === "" || adminToken === "" ? "VYSHKA_TEST_URL and VYSHKA_TEST_TOKEN are not set" : false;

test("the hub's answers satisfy the OpenAPI document, every operation", { skip: required ? false : skip, timeout: 240_000 }, async () => {
  assert.ok(hubURL !== "" && adminToken !== "", "VYSHKA_TEST_LIVE=required, but VYSHKA_TEST_URL or VYSHKA_TEST_TOKEN is empty");

  const check = await OpenAPICheck.load(new URL("../../../spec/openapi-admin.yaml", import.meta.url));
  const graded = (client: AdminClient): AdminClient => {
    client.use({
      async onResponse({ request, response, schemaPath }) {
        await check.grade(request.method, schemaPath, response.clone());
      },
    });
    return client;
  };
  const client = graded(createAdminClient({ baseUrl: hubURL, token: adminToken }));
  const run = Date.now().toString(36);

  const receiver = await startReceiver();
  let driver: Driver | undefined;
  try {
    // Servers and enrollment.
    const created = await unwrap(client.POST("/api/v1/servers", { body: { name: `ts-live ${run}`, game: "conformance" } }));
    const serverId = created.server.id;
    const server = { params: { path: { serverId } } };
    const { servers } = await unwrap(client.GET("/api/v1/servers"));
    assert.ok(servers.some((s) => s.id === serverId), "the new server is listed");
    await unwrap(client.GET("/api/v1/servers/{serverId}", server));
    const enrollment = await unwrap(
      client.POST("/api/v1/servers/{serverId}/enrollment-token", { ...server, body: { ttlSeconds: 600 } }),
    );

    // Webhooks registered before the plugin starts, so they observe what it
    // sends: one target answers 204, the other 500.
    const ok = await unwrap(
      client.POST("/api/v1/webhooks", { body: { url: `${receiver.url}/ok`, events: ["*"], serverIds: [serverId] } }),
    );
    const failing = await unwrap(
      client.POST("/api/v1/webhooks", {
        body: {
          url: `${receiver.url}/fail`,
          events: ["conformance-driver.*", "core.*"],
          serverIds: [serverId],
          template: "discord",
          redact: ["position"],
        },
      }),
    );
    assert.ok(ok.secret !== "" && failing.secret !== "", "registration returns a signing secret");

    driver = startDriver(hubURL, enrollment.token);

    // The manifest, once the driver has published it.
    const manifest = await eventually("the driver's manifest", () =>
      unwrap(client.GET("/api/v1/servers/{serverId}/manifest", server)),
    );
    assert.equal(manifest.revision, 1);

    // Context enumeration, from the plugin and then from the cache or fresh.
    const zones = await unwrap(
      client.GET("/api/v1/servers/{serverId}/contexts/{contextId}/entries", {
        params: { path: { serverId, contextId: "driver.zone" } },
      }),
    );
    assert.equal(zones.entries.length, 2);
    await unwrap(
      client.GET("/api/v1/servers/{serverId}/contexts/{contextId}/entries", {
        params: { path: { serverId, contextId: "driver.zone" }, query: { refresh: true } },
      }),
    );

    // An action, dispatched and followed to its outcome.
    const dispatched = await unwrap(
      client.POST("/api/v1/servers/{serverId}/actions", {
        ...server,
        body: { code: "conformance-driver.echo", params: { amount: 5 }, idempotencyKey: `ts-live-${run}` },
      }),
    );
    const action = await waitForAction(client, dispatched.actionId, { intervalMs: 100 });
    assert.equal(action.state, "completed");
    assert.deepEqual(action.result, { echo: { amount: 5 } });

    // The event feed, one event per page so the cursor is exercised.
    const firstPage = await eventually("the driver's events", async () => {
      const page = await unwrap(
        client.GET("/api/v1/servers/{serverId}/events", {
          ...server,
          params: { ...server.params, query: { type: ["core.*", "conformance-driver.*"], limit: 1 } },
        }),
      );
      assert.ok(page.nextCursor, "a second page");
      return page;
    });
    await unwrap(
      client.GET("/api/v1/servers/{serverId}/events", {
        params: { path: { serverId }, query: { limit: 1, cursor: firstPage.nextCursor } },
      }),
    );

    // State snapshots.
    const players = await eventually("the players snapshot", () =>
      unwrap(
        client.GET("/api/v1/servers/{serverId}/state/{stateType}", {
          params: { path: { serverId, stateType: "players" } },
        }),
      ),
    );
    assert.equal(players.type, "players");
    await unwrap(
      client.GET("/api/v1/servers/{serverId}/state/{stateType}/history", {
        params: { path: { serverId, stateType: "players" }, query: { limit: 5 } },
      }),
    );

    // The player profile: the driver's hello names its player, and an action
    // dispatched in the player context against it, so neither feed is empty
    // and their records are graded, not just their envelopes.
    const player = { platform: "conformance", playerId: "driver-1" };
    const playerEvents = await eventually("the player's events", async () => {
      const page = await unwrap(client.GET("/api/v1/players/{platform}/{playerId}/events", { params: { path: player } }));
      assert.ok(page.events.length > 0, "an event refers to the player");
      return page;
    });
    assert.deepEqual(playerEvents.events[0]?.roles, ["player"]);
    const targeted = await unwrap(
      client.POST("/api/v1/servers/{serverId}/actions", {
        ...server,
        body: { code: "conformance-driver.echo", context: "player", referenceKey: "driver-1", params: { amount: 1 } },
      }),
    );
    await waitForAction(client, targeted.actionId, { intervalMs: 100 });
    const playerActions = await unwrap(
      client.GET("/api/v1/players/{platform}/{playerId}/actions", { params: { path: player } }),
    );
    assert.deepEqual(
      playerActions.actions.map((a) => a.id),
      [targeted.actionId],
    );
    const { note } = await unwrap(
      client.POST("/api/v1/players/{platform}/{playerId}/notes", { params: { path: player }, body: { text: `noted ${run}` } }),
    );
    const notes = await unwrap(client.GET("/api/v1/players/{platform}/{playerId}/notes", { params: { path: player } }));
    assert.ok(notes.notes.some((n) => n.id === note.id));
    await unwrap(
      client.DELETE("/api/v1/players/{platform}/{playerId}/notes/{noteId}", {
        params: { path: { ...player, noteId: note.id } },
      }),
    );

    // A raw envelope.
    const queued = await unwrap(
      client.POST("/api/v1/servers/{serverId}/envelopes", { ...server, body: { type: "ts-live.ping", body: { run } } }),
    );
    assert.equal(queued.envelope.type, "ts-live.ping");

    // The key/value store.
    const namespace = `ts-live-${run}`;
    const key = (k: string) => ({ params: { path: { namespace, key: k } } });
    const written = await unwrap(client.PUT("/api/v1/kv/{namespace}/{key}", { ...key("doc"), body: { value: { a: 1 }, ttlSeconds: 600 } }));
    assert.equal(written.revision, 1);
    const read = await unwrap(client.GET("/api/v1/kv/{namespace}/{key}", key("doc")));
    assert.deepEqual(read.value, { a: 1 });
    await unwrap(client.POST("/api/v1/kv/{namespace}/{key}/incr", key("hits")));
    const bumped = await unwrap(client.POST("/api/v1/kv/{namespace}/{key}/incr", { ...key("hits"), body: { delta: 5 } }));
    assert.equal(bumped.value, 6);
    const mismatch = await client.PUT("/api/v1/kv/{namespace}/{key}", { ...key("doc"), body: { value: 2, ifRevision: 9 } });
    assert.equal(mismatch.response.status, 409);
    const keys = await unwrap(client.GET("/api/v1/kv/{namespace}", { params: { path: { namespace }, query: { prefix: "h" } } }));
    assert.deepEqual(
      keys.keys.map((k) => k.key),
      ["hits"],
    );
    const namespaces = await unwrap(client.GET("/api/v1/kv"));
    assert.ok(namespaces.namespaces.some((n) => n.namespace === namespace && n.keys === 2));
    await unwrap(client.DELETE("/api/v1/kv/{namespace}/{key}", key("doc")));
    await unwrap(client.DELETE("/api/v1/kv/{namespace}/{key}", key("hits")));

    // Tokens, and a bound token's view.
    const minted = await unwrap(
      client.POST("/api/v1/tokens", {
        body: { name: `ts-live ${run}`, scopes: ["servers:read"], servers: [serverId], expiresInSeconds: 3600 },
      }),
    );
    const narrow = graded(createAdminClient({ baseUrl: hubURL, token: minted.secret }));
    await unwrap(narrow.GET("/api/v1/servers/{serverId}", server));
    const refused = await narrow.GET("/api/v1/tokens");
    assert.equal(refused.response.status, 403);
    const { tokens } = await unwrap(client.GET("/api/v1/tokens"));
    assert.ok(tokens.some((t) => t.id === minted.token.id));
    await unwrap(client.DELETE("/api/v1/tokens/{tokenId}", { params: { path: { tokenId: minted.token.id } } }));
    const revoked = await narrow.GET("/api/v1/servers/{serverId}", server);
    assert.equal(revoked.response.status, 401);

    // The audit log.
    const audit = await unwrap(client.GET("/api/v1/audit", { params: { query: { serverId, limit: 5 } } }));
    assert.ok(audit.records.length > 0);

    // The ban list.
    const banned = { platform: "conformance", id: `ts-live-${run}` };
    const ban = await unwrap(
      client.POST("/api/v1/bans", { body: { player: banned, reason: "ts-live", durationSeconds: 3600, name: "Live", serverId } }),
    );
    const again = await client.POST("/api/v1/bans", { body: { player: banned, reason: "twice" } });
    assert.equal(again.response.status, 409);
    await unwrap(client.GET("/api/v1/bans"));
    const history = await unwrap(
      client.GET("/api/v1/bans", { params: { query: { state: "all", platform: banned.platform, playerId: banned.id } } }),
    );
    assert.equal(history.bans.length, 1);
    await unwrap(client.GET("/api/v1/bans/{banId}", { params: { path: { banId: ban.ban.id } } }));
    const lifted = await unwrap(client.POST("/api/v1/bans/{banId}/lift", { params: { path: { banId: ban.ban.id } } }));
    assert.equal(lifted.ban.state, "lifted");

    // Webhook deliveries: delivered on one target, failing on the other.
    const deliveries = (webhookId: string) =>
      unwrap(client.GET("/api/v1/webhooks/{webhookId}/deliveries", { params: { path: { webhookId }, query: { limit: 50 } } }));
    await eventually("a delivered delivery", async () => {
      const page = await deliveries(ok.webhook.id);
      assert.ok(page.deliveries.some((d) => d.state === "delivered"));
    });
    const failed: WebhookDelivery = await eventually("a failed attempt", async () => {
      const page = await deliveries(failing.webhook.id);
      const attempted = page.deliveries.find((d) => d.attempts > 0);
      assert.ok(attempted, "an attempted delivery");
      return attempted;
    });
    const replayed = await unwrap(
      client.POST("/api/v1/webhooks/{webhookId}/deliveries/{deliveryId}/replay", {
        params: { path: { webhookId: failing.webhook.id, deliveryId: failed.id } },
      }),
    );
    assert.equal(replayed.delivery.state, "pending");
    const paused = await unwrap(
      client.PATCH("/api/v1/webhooks/{webhookId}", { params: { path: { webhookId: failing.webhook.id } }, body: { paused: true } }),
    );
    assert.ok(paused.webhook.pausedAt);
    const { webhooks } = await unwrap(client.GET("/api/v1/webhooks"));
    assert.ok(webhooks.some((w) => w.id === ok.webhook.id));
    await unwrap(client.DELETE("/api/v1/webhooks/{webhookId}", { params: { path: { webhookId: ok.webhook.id } } }));
    await unwrap(client.DELETE("/api/v1/webhooks/{webhookId}", { params: { path: { webhookId: failing.webhook.id } } }));

    // The server's record with the plugin linked, then its credentials revoked.
    await eventually("the server's ban report", async () => {
      const record = await unwrap(client.GET("/api/v1/servers/{serverId}", server));
      assert.ok(record.bans?.appliedRevision !== null && record.bans?.appliedRevision !== undefined);
    });
    await driver.stop();
    driver = undefined;
    await unwrap(client.DELETE("/api/v1/servers/{serverId}/credentials", server));

    // A refusal comes back as the protocol error, graded like an answer.
    await assert.rejects(unwrap(client.GET("/api/v1/servers/{serverId}", { params: { path: { serverId: `missing-${run}` } } })), (err) => {
      assert.ok(err instanceof AdminApiError);
      assert.equal(err.code, "not_found");
      assert.equal(err.status, 404);
      return true;
    });
  } catch (err) {
    if (driver) {
      console.error(`driver log:\n${driver.log()}`);
    }
    throw err;
  } finally {
    await driver?.stop();
    await receiver.close();
  }

  assert.deepEqual(check.failures, [], "every answer satisfies the document");
  assert.deepEqual(check.unexercised(), [], "every operation in the document was called and answered 2xx");
});

/** Retries fn until it resolves, for up to 60 s. */
async function eventually<T>(what: string, fn: () => Promise<T>): Promise<T> {
  const deadline = Date.now() + 60_000;
  for (;;) {
    try {
      return await fn();
    } catch (err) {
      if (Date.now() > deadline) {
        throw new Error(`gave up waiting for ${what}`, { cause: err });
      }
      await new Promise((resolve) => setTimeout(resolve, 250));
    }
  }
}

interface Receiver {
  url: string;
  close(): Promise<void>;
}

/** A webhook target: /ok answers 204, anything else 500. */
async function startReceiver(): Promise<Receiver> {
  const server = http.createServer((request, response) => {
    request.resume();
    request.on("end", () => {
      response.statusCode = request.url === "/ok" ? 204 : 500;
      response.end();
    });
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const { port } = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${port}`,
    close: () =>
      new Promise((resolve) => {
        server.closeAllConnections();
        server.close(() => resolve());
      }),
  };
}

interface Driver {
  log(): string;
  stop(): Promise<void>;
}

/** The reference plugin driver, enrolled with token; it exits when its stdin closes. */
function startDriver(url: string, token: string): Driver {
  const command = process.env.VYSHKA_TEST_DRIVER;
  const child: ChildProcess = command
    ? spawn(command, ["-url", url, "-token", token], { stdio: ["pipe", "pipe", "pipe"] })
    : spawn("go", ["run", "./conformance/plugin/driver", "-url", url, "-token", token], {
        cwd: repoRoot,
        stdio: ["pipe", "pipe", "pipe"],
      });
  let output = "";
  child.stdout?.on("data", (chunk) => (output += chunk));
  child.stderr?.on("data", (chunk) => (output += chunk));
  let stopped: Promise<void> | undefined;
  return {
    log: () => output,
    stop() {
      stopped ??= (async () => {
        if (child.exitCode !== null || child.signalCode !== null) {
          return;
        }
        const exited = once(child, "exit");
        child.stdin?.end();
        const timer = setTimeout(() => child.kill(), 5_000);
        await exited;
        clearTimeout(timer);
      })();
      return stopped;
    },
  };
}
