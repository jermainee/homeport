import { createExecutionContext, waitOnExecutionContext } from "cloudflare:test";
import { env } from "cloudflare:workers";
import { vi } from "vitest";
import worker from "../src/index";

export const BASE_URL = "https://api.homeport.test";

/** Sends a request to the Worker, optionally with some env values replaced. */
export async function call(
  path: string,
  init?: RequestInit,
  envOverrides: Partial<Env> = {},
): Promise<Response> {
  const ctx = createExecutionContext();
  const response = await worker.fetch(
    new Request(new URL(path, BASE_URL), init),
    { ...env, ...envOverrides },
    ctx,
  );
  await waitOnExecutionContext(ctx);
  return response;
}

export function postJson(
  path: string,
  body: unknown,
  envOverrides: Partial<Env> = {},
): Promise<Response> {
  return call(
    path,
    { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(body) },
    envOverrides,
  );
}

export function postForm(path: string, fields: Record<string, string>): Promise<Response> {
  return call(path, { method: "POST", body: new URLSearchParams(fields) });
}

export function bearer(sessionToken: string): RequestInit {
  return { headers: { authorization: `Bearer ${sessionToken}` } };
}

let counter = 0;
/** A fresh email address per test, so tests never share a member. */
export function uniqueEmail(): string {
  counter += 1;
  return `member-${crypto.randomUUID()}-${counter}@example.com`;
}

/** A UUIDv7 whose timestamp is `ms`, as the apps create for `request_id`. */
export function uuidv7(ms = Date.now()): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  for (let i = 0; i < 6; i++) bytes[i] = Math.floor(ms / 2 ** (8 * (5 - i))) % 256;
  bytes[6] = ((bytes[6] ?? 0) & 0x0f) | 0x70;
  bytes[8] = ((bytes[8] ?? 0) & 0x3f) | 0x80;
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

/** A session token as the app creates it: `hpm_` and 32 random bytes in base64url. */
export function newSessionToken(): string {
  let binary = "";
  for (const byte of crypto.getRandomValues(new Uint8Array(32))) binary += String.fromCharCode(byte);
  return `hpm_${btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "")}`;
}

/** Lowercase hex SHA-256, computed independently of the Worker's code. */
export async function sha256Hex(value: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value));
  return Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, "0")).join("");
}

export interface LoginRequest {
  login_request_id: string;
  match_code: string;
  expires_at: string;
}

export async function loginRequestBody(email: string, sessionToken: string) {
  return {
    request_id: uuidv7(),
    email,
    session_token_sha256: await sha256Hex(sessionToken),
  };
}

/**
 * Starts a sign-in the way the app does and returns the API response, the
 * app's session token and the link from the dev email log.
 */
export async function startLogin(
  email: string,
): Promise<{ request: LoginRequest; sessionToken: string; link: URL }> {
  const sessionToken = newSessionToken();
  const log = vi.spyOn(console, "log");
  try {
    const response = await postJson("/v1/auth/login-requests", await loginRequestBody(email, sessionToken));
    if (response.status !== 201) {
      throw new Error(`login request failed: ${response.status} ${await response.text()}`);
    }
    const request = await response.json<LoginRequest>();
    return { request, sessionToken, link: findLoggedLink(log.mock.calls, email) };
  } finally {
    log.mockRestore();
  }
}

/** Polls a login request the way the app does every few seconds: a GET with its token. */
export function poll(
  loginRequestId: string,
  sessionToken: string,
  envOverrides: Partial<Env> = {},
): Promise<Response> {
  return call(`/v1/auth/login-requests/${loginRequestId}`, bearer(sessionToken), envOverrides);
}

export function confirm(link: URL, matchCode: string): Promise<Response> {
  return postForm("/auth/confirm", { token: tokenFrom(link), match_code: matchCode });
}

/** Signs in end to end through the dev email log and returns the activated session token. */
export async function signIn(email: string): Promise<{ sessionToken: string; memberId: string }> {
  const { request, sessionToken, link } = await startLogin(email);
  const confirmed = await confirm(link, request.match_code);
  if (confirmed.status !== 200) throw new Error(`confirm failed: ${confirmed.status}`);
  const polled = await poll(request.login_request_id, sessionToken);
  if (polled.status !== 200) throw new Error(`poll failed: ${polled.status}`);
  const { member_id } = await polled.json<{ member_id: string }>();
  return { sessionToken, memberId: member_id };
}

export function findLoggedLink(calls: unknown[][], email: string): URL {
  for (const args of calls) {
    for (const arg of args) {
      if (typeof arg !== "string") continue;
      const entry = JSON.parse(arg) as { event?: string; to?: string; link?: string };
      if (entry.event === "magic_link_email" && entry.to === email.toLowerCase() && entry.link) {
        return new URL(entry.link);
      }
    }
  }
  throw new Error(`no magic link logged for ${email}`);
}

export function tokenFrom(link: URL): string {
  const token = link.searchParams.get("token");
  if (!token) throw new Error(`link has no token: ${link.href}`);
  return token;
}

export const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
export const TIMESTAMP = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$/;
