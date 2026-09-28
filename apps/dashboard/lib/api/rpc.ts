// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// The one place the dashboard talks Connect to its upstreams.
//
//   cp       CONTROL_PLANE_URL  every ControlPlane/Cost/Logs/Profiles/
//                               Network/Alerts service
//   advisor  ADVISOR_URL        AdvisorService
//
// Unary calls use Connect's JSON codec (protojson: lowerCamelCase keys,
// int64 as strings, zero values omitted) so responses are greppable with
// curl. Server streams use application/connect+json envelopes — see
// connect-stream.ts; openServerStream() only opens them, the SSE route
// handlers decode.
//
// Credentials (never exposed to the browser — this module is
// server-only):
//   · token-mode session  → the signed-in user's own token, so the
//     control plane enforces that user's RBAC;
//   · anonymous session against an open control plane → no header;
//   · otherwise → the shared CONTROL_PLANE_TOKEN / ADVISOR_TOKEN.
//
// Every call has a deadline (Connect-Timeout-Ms + an abort signal), a
// response-size cap, and returns a typed result instead of throwing — the
// pages degrade to labelled demo data on any failure. The one exception
// is an expired/revoked user token: that redirects through
// /api/auth/expired, which clears the cookie and lands on /login.

import "server-only";
import { redirect } from "next/navigation";
import { getSession } from "@/lib/session";
import { encodeEnvelope } from "./connect-stream";

export type Upstream = "cp" | "advisor";

export type ConnectCode =
  | "canceled"
  | "unknown"
  | "invalid_argument"
  | "deadline_exceeded"
  | "not_found"
  | "already_exists"
  | "permission_denied"
  | "resource_exhausted"
  | "failed_precondition"
  | "aborted"
  | "out_of_range"
  | "unimplemented"
  | "internal"
  | "unavailable"
  | "data_loss"
  | "unauthenticated";

export type RpcError = {
  /** A Connect code, or a transport-level failure before any Connect reply. */
  code: ConnectCode | "not_configured" | "network";
  message: string;
  status?: number;
};

export type RpcResult<T> = { ok: true; data: T } | { ok: false; error: RpcError };

export type Credential = {
  header: string | null;
  source: "user" | "shared" | "none";
};

export type CallOptions = {
  timeoutMs?: number;
  signal?: AbortSignal;
  /** Skip session lookup (login probes, tests). */
  credential?: Credential;
  /**
   * What to do when the USER's token is rejected. "redirect" (pages,
   * server actions) sends them through /api/auth/expired; "return"
   * (route handlers that must answer JSON/SSE) just returns the error.
   */
  onExpired?: "redirect" | "return";
};

const ENV: Record<Upstream, { url: string; token: string; tag: string }> = {
  cp: { url: "CONTROL_PLANE_URL", token: "CONTROL_PLANE_TOKEN", tag: "control-plane" },
  advisor: { url: "ADVISOR_URL", token: "ADVISOR_TOKEN", tag: "advisor" },
};

export const DEFAULT_TIMEOUT_MS = 10_000;
/** Responses larger than this are refused (the RPCs all cap results far lower). */
export const MAX_RESPONSE_BYTES = 32 * 1024 * 1024;

export function upstreamBase(u: Upstream): string | null {
  const v = process.env[ENV[u].url]?.trim();
  return v && v.length > 0 ? v.replace(/\/+$/, "") : null;
}

export async function resolveCredential(u: Upstream): Promise<Credential> {
  const session = await getSession();
  if (session?.mode === "token") {
    return session.token
      ? { header: `Bearer ${session.token}`, source: "user" }
      : // Signed in anonymously against a control plane that reported
        // authRequired=false: presenting any token would be rejected.
        { header: null, source: "user" };
  }
  const shared = process.env[ENV[u].token]?.trim();
  return shared ? { header: `Bearer ${shared}`, source: "shared" } : { header: null, source: "none" };
}

function headersFor(cred: Credential, timeoutMs: number, contentType: string): Record<string, string> {
  const h: Record<string, string> = {
    "Content-Type": contentType,
    "Connect-Protocol-Version": "1",
    "Connect-Timeout-Ms": String(timeoutMs),
    "User-Agent": "kubehero-dashboard",
  };
  if (cred.header) h.Authorization = cred.header;
  return h;
}

// HTTP status → Connect code, for errors that arrive without a Connect
// JSON body (proxies, load balancers, 404 on an old control plane).
export function codeFromStatus(status: number): ConnectCode {
  switch (status) {
    case 400:
      return "invalid_argument";
    case 401:
      return "unauthenticated";
    case 403:
      return "permission_denied";
    case 404:
      return "unimplemented";
    case 408:
      return "deadline_exceeded";
    case 409:
      return "aborted";
    case 412:
      return "failed_precondition";
    case 413:
      return "resource_exhausted";
    case 415:
      return "internal";
    case 429:
      return "unavailable";
    case 499:
      return "canceled";
    case 502:
    case 503:
    case 504:
      return "unavailable";
    default:
      return "unknown";
  }
}

const KNOWN_CODES = new Set<string>([
  "canceled", "unknown", "invalid_argument", "deadline_exceeded", "not_found",
  "already_exists", "permission_denied", "resource_exhausted", "failed_precondition",
  "aborted", "out_of_range", "unimplemented", "internal", "unavailable", "data_loss",
  "unauthenticated",
]);

export function parseConnectError(status: number, body: string): RpcError {
  try {
    const v = JSON.parse(body) as { code?: unknown; message?: unknown };
    if (v && typeof v.code === "string" && KNOWN_CODES.has(v.code)) {
      return {
        code: v.code as ConnectCode,
        message: typeof v.message === "string" ? v.message.slice(0, 500) : "",
        status,
      };
    }
  } catch {
    /* not a Connect error body */
  }
  return { code: codeFromStatus(status), message: body.slice(0, 200), status };
}

type FetchLike = {
  ok: boolean;
  status?: number;
  json?: () => Promise<unknown>;
  text?: () => Promise<string>;
  body?: ReadableStream<Uint8Array> | null;
  headers?: Headers;
};

async function readCapped(r: FetchLike): Promise<string> {
  const len = Number(r.headers?.get?.("content-length") ?? "");
  if (Number.isFinite(len) && len > MAX_RESPONSE_BYTES) {
    throw new Error(`response of ${len} bytes exceeds cap`);
  }
  if (r.body && typeof (r.body as ReadableStream).getReader === "function") {
    const reader = r.body.getReader();
    const chunks: Uint8Array[] = [];
    let total = 0;
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.length;
      if (total > MAX_RESPONSE_BYTES) {
        await reader.cancel();
        throw new Error("response exceeds cap");
      }
      chunks.push(value);
    }
    return Buffer.concat(chunks).toString("utf8");
  }
  if (r.text) return r.text();
  return JSON.stringify(r.json ? await r.json() : {});
}

function timeoutSignal(timeoutMs: number, outer?: AbortSignal): AbortSignal {
  const t = AbortSignal.timeout(timeoutMs);
  return outer ? AbortSignal.any([outer, t]) : t;
}

export async function callUnary<Res>(
  u: Upstream,
  service: string,
  method: string,
  req: unknown,
  opts: CallOptions = {},
): Promise<RpcResult<Res>> {
  const base = upstreamBase(u);
  if (!base) {
    return { ok: false, error: { code: "not_configured", message: `${ENV[u].url} is not set` } };
  }
  const cred = opts.credential ?? (await resolveCredential(u));
  const timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;

  let result: RpcResult<Res>;
  try {
    const r = (await fetch(`${base}/${service}/${method}`, {
      method: "POST",
      headers: headersFor(cred, timeoutMs, "application/json"),
      body: JSON.stringify(req ?? {}),
      cache: "no-store",
      signal: timeoutSignal(timeoutMs, opts.signal),
    })) as unknown as FetchLike;
    if (r.ok) {
      const data = (r.json && !r.body ? await r.json() : JSON.parse((await readCapped(r)) || "{}")) as Res;
      result = { ok: true, data: data ?? ({} as Res) };
    } else {
      const body = r.text ? await r.text() : "";
      result = { ok: false, error: parseConnectError(r.status ?? 0, body) };
    }
  } catch (err) {
    const aborted = err instanceof Error && (err.name === "TimeoutError" || err.name === "AbortError");
    result = {
      ok: false,
      error: aborted
        ? { code: "deadline_exceeded", message: `${method} timed out after ${timeoutMs}ms` }
        : { code: "network", message: err instanceof Error ? err.message : String(err) },
    };
  }

  if (!result.ok) {
    // Never log credentials or request bodies — method + code is enough to debug.
    console.error(`[${ENV[u].tag}]`, method, result.error.code, result.error.status ?? "", result.error.message);
    if (
      result.error.code === "unauthenticated" &&
      cred.source === "user" &&
      (opts.onExpired ?? "redirect") === "redirect"
    ) {
      // Outside the try/catch on purpose: redirect() throws NEXT_REDIRECT.
      redirect("/api/auth/expired");
    }
  }
  return result;
}

/** Legacy-shaped helper: the response, or null on any failure. */
export async function callOrNull<Res>(
  u: Upstream,
  service: string,
  method: string,
  req: unknown,
  opts?: CallOptions,
): Promise<Res | null> {
  const r = await callUnary<Res>(u, service, method, req, opts);
  return r.ok ? r.data : null;
}

/**
 * Opens a Connect server stream. Resolves to the raw Response (body =
 * envelopes) or a typed error when the upstream refused before streaming.
 */
export async function openServerStream(
  u: Upstream,
  service: string,
  method: string,
  req: unknown,
  opts: { signal: AbortSignal; credential?: Credential; timeoutMs?: number },
): Promise<{ ok: true; res: Response } | { ok: false; error: RpcError }> {
  const base = upstreamBase(u);
  if (!base) return { ok: false, error: { code: "not_configured", message: `${ENV[u].url} is not set` } };
  const cred = opts.credential ?? (await resolveCredential(u));
  // Streams are long-lived: the deadline only bounds how long the
  // upstream may take to START answering. Once headers arrive the timer
  // is cleared and only the caller's signal (client went away) ends it.
  const connectMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  const headers = headersFor(cred, connectMs, "application/connect+json");
  delete headers["Connect-Timeout-Ms"];

  const ctl = new AbortController();
  const onOuterAbort = () => ctl.abort(opts.signal.reason);
  if (opts.signal.aborted) ctl.abort(opts.signal.reason);
  else opts.signal.addEventListener("abort", onOuterAbort, { once: true });
  let timedOut = false;
  const timer = setTimeout(() => {
    timedOut = true;
    ctl.abort();
  }, connectMs);

  try {
    const res = await fetch(`${base}/${service}/${method}`, {
      method: "POST",
      headers,
      body: encodeEnvelope(req) as unknown as BodyInit,
      cache: "no-store",
      signal: ctl.signal,
    });
    clearTimeout(timer);
    if (!res.ok) {
      const body = await res.text().catch(() => "");
      return { ok: false, error: parseConnectError(res.status, body) };
    }
    return { ok: true, res };
  } catch (err) {
    clearTimeout(timer);
    return {
      ok: false,
      error: timedOut
        ? { code: "deadline_exceeded", message: `${method} did not start within ${connectMs}ms` }
        : { code: "network", message: err instanceof Error ? err.message : String(err) },
    };
  }
}

/** Human copy for an RPC error, for toasts and inline banners. */
export function describeRpcError(e: RpcError): string {
  switch (e.code) {
    case "permission_denied":
      return "Your role does not allow this — an admin token is required.";
    case "unauthenticated":
      return "Your session is no longer valid — sign in again.";
    case "not_configured":
      return "No control plane configured (demo mode).";
    case "invalid_argument":
      return e.message ? `Rejected: ${e.message}` : "The control plane rejected the request.";
    case "failed_precondition":
      return e.message || "The control plane is not ready for this request.";
    case "deadline_exceeded":
      return "The control plane did not answer in time.";
    case "unavailable":
    case "network":
      return "The control plane is unreachable.";
    case "unimplemented":
      return "This control plane version does not support that yet.";
    default:
      return e.message || `Request failed (${e.code}).`;
  }
}
