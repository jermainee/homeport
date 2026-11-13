import { describe, expect, it, vi } from "vitest";
import {
  BASE_URL,
  call,
  findLoggedLink,
  postForm,
  postJson,
  startLogin,
  tokenFrom,
  uniqueEmail,
} from "./helpers";

describe("POST /v1/auth/login-requests", () => {
  it("returns a login request and, in dev email mode, logs the sign-in link", async () => {
    const email = uniqueEmail();
    const log = vi.spyOn(console, "log");

    const response = await postJson("/v1/auth/login-requests", { email });

    expect(response.status).toBe(201);
    const body = await response.json<Record<string, unknown>>();
    expect(body).toMatchObject({
      login_request_id: expect.any(String),
      poll_secret: expect.any(String),
      match_code: expect.stringMatching(/^\d{6}$/),
    });

    const link = findLoggedLink(log.mock.calls, email);
    expect(link.origin).toBe(BASE_URL);
    expect(link.pathname).toBe("/auth/confirm");
    expect(link.searchParams.get("token")).toMatch(/^[A-Za-z0-9_-]{43}$/);
    log.mockRestore();
  });
});

describe("GET /auth/confirm", () => {
  it("shows the match code and a Confirm button without signing anyone in", async () => {
    const email = uniqueEmail();
    const { request, link } = await startLogin(email);

    const response = await call(link.pathname + link.search);

    expect(response.status).toBe(200);
    expect(response.headers.get("content-type")).toMatch(/^text\/html/);
    expect(response.headers.get("cache-control")).toBe("no-store");
    expect(response.headers.get("referrer-policy")).toBe("no-referrer");
    const page = await response.text();
    expect(page).toContain(request.match_code);
    expect(page).toContain(email);
    expect(page).toMatch(/<form method="post" action="\/auth\/confirm">/);
    expect(page).toMatch(/<button[^>]*>Confirm<\/button>/);

    const poll = await postJson(`/v1/auth/login-requests/${request.login_request_id}/poll`, {
      poll_secret: request.poll_secret,
    });
    expect(poll.status).toBe(202);
    expect(await poll.json()).toEqual({ status: "pending" });
  });
});

describe("signing in", () => {
  it("confirming the link lets the app's poll collect a session that /v1/me accepts", async () => {
    const email = uniqueEmail();
    const { request, link } = await startLogin(email);

    const confirm = await postForm("/auth/confirm", {
      token: tokenFrom(link),
      match_code: request.match_code,
    });
    expect(confirm.status).toBe(200);
    expect(await confirm.text()).toContain("You are signed in");

    const poll = await postJson(`/v1/auth/login-requests/${request.login_request_id}/poll`, {
      poll_secret: request.poll_secret,
    });
    expect(poll.status).toBe(200);
    const session = await poll.json<{ status: string; session_token: string; expires_at: string }>();
    expect(session).toEqual({
      status: "confirmed",
      session_token: expect.stringMatching(/^[A-Za-z0-9_-]{43}$/),
      expires_at: expect.any(String),
    });

    const me = await call("/v1/me", { headers: { authorization: `Bearer ${session.session_token}` } });
    expect(me.status).toBe(200);
    expect(await me.json()).toEqual({
      id: expect.any(String),
      email,
      status: "active",
      identity_status: "unverified",
      created_at: expect.any(String),
    });
  });
});

describe("login request validation", () => {
  it.each([
    ["a missing email", {}],
    ["a non-string email", { email: 42 }],
    ["an email without @", { email: "member.example.com" }],
    ["an email over 254 characters", { email: `${"a".repeat(250)}@example.com` }],
  ])("rejects %s", async (_case, body) => {
    const response = await postJson("/v1/auth/login-requests", body);

    expect(response.status).toBe(400);
    expect(await response.json()).toEqual({ error: "invalid_email" });
  });

  it("rejects a body that is not JSON", async () => {
    const response = await call("/v1/auth/login-requests", { method: "POST", body: "email=x" });

    expect(response.status).toBe(400);
  });

  it.each([
    ["a non-string install_id", 7],
    ["an install_id with spaces", "my laptop"],
    ["an install_id over 64 characters", "a".repeat(65)],
  ])("rejects %s", async (_case, installId) => {
    const response = await postJson("/v1/auth/login-requests", {
      email: uniqueEmail(),
      install_id: installId,
    });

    expect(response.status).toBe(400);
    expect(await response.json()).toEqual({ error: "invalid_install_id" });
  });
});
