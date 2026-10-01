// A TypeScript client for the Admin API of a Vyshka hub, the operator-facing
// half of the protocol (spec/protocol.md, with spec/openapi-admin.yaml as its
// machine-readable companion).
//
// The types in ./schema.ts are generated from spec/openapi-admin.yaml; the
// requests go through openapi-fetch, so every path, parameter, body, and
// answer is checked against the document at compile time. This module adds
// what a generator cannot: a constructor that refuses a URL or token that
// cannot work, path parameters that cannot resolve to a different route, the
// protocol error as a thrown error, and a wait for an action's outcome.

import createClient, { type Client, type PathSerializer } from "openapi-fetch";
import type { ActionRecord, paths } from "./schema.js";

export type * from "./schema.js";
export type { Error as ErrorBody } from "./schema.js";
export { OPENAPI_VERSION, PROTOCOL_DRAFT } from "./version.js";

/** A client of one hub with one token: openapi-fetch over the Admin API paths. */
export type AdminClient = Client<paths>;

export interface AdminClientOptions {
  /**
   * The hub's base URL, such as http://127.0.0.1:8080, or a path prefix
   * behind a reverse proxy. http or https, with no query or fragment.
   */
  baseUrl: string;
  /** An Admin API token (spec section 10). Never logged by this package. */
  token: string;
  /** A fetch to use instead of globalThis.fetch. */
  fetch?: (input: Request) => Promise<Response>;
  /** Extra headers on every request. Authorization is always the token's. */
  headers?: Record<string, string>;
}

/**
 * Returns a client for the hub at options.baseUrl that authenticates with
 * options.token. Throws a TypeError when the URL is not an http(s) URL
 * without a query or fragment, or when the token is empty or carries
 * whitespace or a control character, which could not travel in a header.
 *
 * Redirects are not followed: an API client that follows one turns a POST
 * into a GET and a misconfigured URL into an answer from somewhere else, so a
 * 3xx comes back as the hub's answer and unwrap() reports it.
 */
export function createAdminClient(options: AdminClientOptions): AdminClient {
  const baseUrl = checkBaseUrl(options.baseUrl);
  if (typeof options.token !== "string" || options.token === "") {
    throw new TypeError("vyshka: the token is empty");
  }
  if (/[\s\p{Cc}]/u.test(options.token)) {
    throw new TypeError("vyshka: the token contains whitespace or a control character");
  }
  return createClient<paths>({
    baseUrl,
    fetch: options.fetch,
    headers: {
      ...options.headers,
      Accept: "application/json",
      Authorization: `Bearer ${options.token}`,
    },
    pathSerializer: strictPathSerializer,
    redirect: "manual",
  });
}

function checkBaseUrl(raw: string): string {
  const trimmed = typeof raw === "string" ? raw.trim().replace(/\/+$/, "") : "";
  if (trimmed === "") {
    throw new TypeError("vyshka: the hub URL is empty");
  }
  let parsed: URL;
  try {
    parsed = new URL(trimmed);
  } catch {
    throw new TypeError("vyshka: the hub URL does not parse");
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
    throw new TypeError(`vyshka: the hub URL must start with http:// or https://, not ${parsed.protocol}`);
  }
  if (parsed.search !== "" || parsed.hash !== "" || trimmed.includes("?") || trimmed.includes("#")) {
    throw new TypeError("vyshka: the hub URL must not carry a query or a fragment");
  }
  return trimmed;
}

/**
 * Fills the path template, one percent-encoded segment per parameter, and
 * refuses what would not stay one segment of this route. An empty value
 * would join its neighbours; "." and ".." are dot segments, which the URL
 * parser of every fetch removes before the request leaves (percent-encoded
 * too, under the WHATWG URL standard), so they would reach a different
 * route. A player identity that is exactly "." or ".." is legal in the
 * protocol (spec section 8.6) and needs an HTTP client that can send %2e;
 * the Go client in this repository is one.
 */
const strictPathSerializer: PathSerializer = (pathname, pathParams) =>
  pathname.replace(/\{([^{}]+)\}/g, (_, name: string) => {
    const value = pathParams?.[name];
    if (typeof value !== "string" && !(typeof value === "number" && Number.isFinite(value))) {
      throw new TypeError(`vyshka: the path parameter ${name} is missing`);
    }
    const segment = String(value);
    if (segment === "") {
      throw new TypeError(`vyshka: the path parameter ${name} is empty`);
    }
    if (segment === "." || segment === "..") {
      throw new TypeError(
        `vyshka: the path parameter ${name} is a dot segment, which fetch cannot send; use a client that sends %2e`,
      );
    }
    return encodeURIComponent(segment);
  });

/**
 * A refusal from the hub: any answer outside 2xx. code is the protocol error
 * code (spec section 2.2), stable and meant to be branched on; message is for
 * people. An answer that did not carry the protocol error shape (a reverse
 * proxy's error page, a redirect) has an empty code.
 */
export class AdminApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly details: Record<string, unknown> | undefined;

  constructor(status: number, code: string, message: string, details?: Record<string, unknown>) {
    super(code === "" ? `${message} (${status})` : `${code} (${status}): ${message}`);
    this.name = "AdminApiError";
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

/** What every openapi-fetch request resolves to. */
export type Outcome = { data?: unknown; error?: unknown; response: Response };

/** The body of an outcome's 2xx variant; undefined for an answer without one. */
export type Answer<R extends Outcome> = [Exclude<R, { error: unknown }>["data"]] extends [never]
  ? undefined
  : Exclude<R, { error: unknown }>["data"];

/**
 * Resolves to the answer's body, or throws an AdminApiError for any answer
 * outside 2xx. A 204 resolves to undefined.
 *
 *     const { servers } = await unwrap(client.GET("/api/v1/servers"));
 */
export async function unwrap<R extends Outcome>(outcome: R | Promise<R>): Promise<Answer<R>> {
  const { data, error, response } = await outcome;
  if (response.ok) {
    return data as Answer<R>;
  }
  throw refusal(response, error);
}

function refusal(response: Response, body: unknown): AdminApiError {
  const shaped = (body as { error?: { code?: unknown; message?: unknown; details?: unknown } } | undefined)?.error;
  if (shaped && typeof shaped.code === "string" && shaped.code !== "") {
    const details =
      shaped.details && typeof shaped.details === "object" ? (shaped.details as Record<string, unknown>) : undefined;
    return new AdminApiError(response.status, shaped.code, String(shaped.message ?? ""), details);
  }
  // A browser hands a manual redirect over as an opaque response, status 0.
  if (response.type === "opaqueredirect" || (response.status >= 300 && response.status < 400)) {
    const location = response.headers.get("Location");
    const message = location
      ? `the hub redirected to ${location}, which this client does not follow`
      : "the hub redirected, which this client does not follow";
    return new AdminApiError(response.status, "", message);
  }
  return new AdminApiError(response.status, "", response.statusText || "unexpected HTTP status");
}

/** The states an action never leaves: completed, failed, and expired. */
export function isTerminal(state: string): boolean {
  return state === "completed" || state === "failed" || state === "expired";
}

export interface WaitOptions {
  /** Milliseconds between reads; 500 when absent. */
  intervalMs?: number;
  /** Stops the wait; the promise rejects with the signal's reason. */
  signal?: AbortSignal;
  /** Called with the record on the first read and on every change of state. */
  onChange?: (action: ActionRecord) => void;
}

/**
 * Reads an action until it reaches a terminal state and resolves to that
 * record. A refusal rejects with an AdminApiError; options.signal ending the
 * wait rejects with its reason, and onChange has seen how far the action got.
 */
export async function waitForAction(
  client: AdminClient,
  actionId: string,
  options: WaitOptions = {},
): Promise<ActionRecord> {
  const interval = options.intervalMs !== undefined && options.intervalMs > 0 ? options.intervalMs : 500;
  const signal = options.signal;
  let last: string | undefined;
  for (;;) {
    signal?.throwIfAborted();
    const action = await unwrap(client.GET("/api/v1/actions/{actionId}", { params: { path: { actionId } }, signal }));
    if (options.onChange && action.state !== last) {
      options.onChange(action);
    }
    last = action.state;
    if (isTerminal(action.state)) {
      return action;
    }
    await sleep(interval, signal);
  }
}

function sleep(ms: number, signal: AbortSignal | undefined): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(signal.reason);
      return;
    }
    const onAbort = () => {
      clearTimeout(timer);
      reject(signal?.reason);
    };
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}
