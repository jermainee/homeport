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

let counter = 0;
/** A fresh email address per test, so tests never share a member. */
export function uniqueEmail(): string {
  counter += 1;
  return `member-${crypto.randomUUID()}-${counter}@example.com`;
}

export interface LoginRequest {
  login_request_id: string;
  poll_secret: string;
  match_code: string;
}

/** Starts a sign-in and returns the API response plus the link from the dev email log. */
export async function startLogin(
  email: string,
  extra: Record<string, unknown> = {},
): Promise<{ request: LoginRequest; link: URL }> {
  const log = vi.spyOn(console, "log");
  try {
    const response = await postJson("/v1/auth/login-requests", { email, ...extra });
    if (response.status !== 201) {
      throw new Error(`login request failed: ${response.status} ${await response.text()}`);
    }
    const request = await response.json<LoginRequest>();
    const link = findLoggedLink(log.mock.calls, email);
    return { request, link };
  } finally {
    log.mockRestore();
  }
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

/** Signs in end to end through the dev email log and returns the session token. */
export async function signIn(email: string): Promise<{ request: LoginRequest; sessionToken: string }> {
  const { request, link } = await startLogin(email);
  const confirm = await postForm("/auth/confirm", {
    token: tokenFrom(link),
    match_code: request.match_code,
  });
  if (confirm.status !== 200) throw new Error(`confirm failed: ${confirm.status}`);
  const poll = await postJson(`/v1/auth/login-requests/${request.login_request_id}/poll`, {
    poll_secret: request.poll_secret,
  });
  if (poll.status !== 200) throw new Error(`poll failed: ${poll.status}`);
  const { session_token } = await poll.json<{ session_token: string }>();
  return { request, sessionToken: session_token };
}
