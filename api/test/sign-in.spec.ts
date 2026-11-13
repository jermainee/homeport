import { afterEach, describe, expect, it, vi } from "vitest";
import {
  BASE_URL,
  TIMESTAMP,
  UUID,
  bearer,
  call,
  confirm,
  findLoggedLink,
  loginRequestBody,
  newSessionToken,
  poll,
  postJson,
  startLogin,
  uniqueEmail,
  uuidv7,
} from "./helpers";

afterEach(() => {
  vi.restoreAllMocks();
});

describe("POST /v1/auth/login-requests", () => {
  it("returns the login request and, in dev email mode, logs the sign-in link", async () => {
    const email = uniqueEmail();
    const log = vi.spyOn(console, "log");

    const response = await postJson(
      "/v1/auth/login-requests",
      await loginRequestBody(email, newSessionToken()),
    );

    expect(response.status).toBe(201);
    expect(await response.json()).toEqual({
      login_request_id: expect.stringMatching(UUID),
      match_code: expect.stringMatching(/^\d{6}$/),
      expires_at: expect.stringMatching(TIMESTAMP),
    });

    const link = findLoggedLink(log.mock.calls, email);
    expect(link.origin).toBe(BASE_URL);
    expect(link.pathname).toBe("/auth/confirm");
    expect(link.searchParams.get("token")).toMatch(/^[A-Za-z0-9_-]{43}$/);
  });
});

describe("GET /auth/confirm", () => {
  it("shows the match code and a Confirm button without signing anyone in", async () => {
    const email = uniqueEmail();
    const { request, sessionToken, link } = await startLogin(email);

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

    const polled = await poll(request.login_request_id, sessionToken);
    expect(polled.status).toBe(202);
    expect(await polled.json()).toEqual({ status: "pending" });
  });
});

describe("signing in", () => {
  it("approving the link activates the app's session token for /v1/me", async () => {
    const email = uniqueEmail();
    const { request, sessionToken, link } = await startLogin(email);

    const confirmed = await confirm(link, request.match_code);
    expect(confirmed.status).toBe(200);
    expect(await confirmed.text()).toContain("You are signed in");

    const polled = await poll(request.login_request_id, sessionToken);
    expect(polled.status).toBe(200);
    const approved = await polled.json<{ member_id: string }>();
    expect(approved).toEqual({
      status: "approved",
      member_id: expect.stringMatching(UUID),
      expires_at: expect.stringMatching(TIMESTAMP),
    });

    const me = await call("/v1/me", bearer(sessionToken));
    expect(me.status).toBe(200);
    expect(await me.json()).toEqual({
      member_id: approved.member_id,
      email,
      identity_status: "unverified",
      credits: { purchased: 0, earned_pending: 0, earned_cashable: 0 },
    });
  });

  it("does not accept the session token before the member approves", async () => {
    const { sessionToken } = await startLogin(uniqueEmail());

    expect((await call("/v1/me", bearer(sessionToken))).status).toBe(401);
  });

  it("answers approved on every poll until the login request expires", async () => {
    const { request, sessionToken, link } = await startLogin(uniqueEmail());
    await confirm(link, request.match_code);

    const first = await poll(request.login_request_id, sessionToken);
    const second = await poll(request.login_request_id, sessionToken);

    expect(first.status).toBe(200);
    expect(second.status).toBe(200);
    expect(await second.json()).toEqual(await first.json());
  });

  it("answers 404 to a poll with another session token", async () => {
    const { request, link } = await startLogin(uniqueEmail());
    await confirm(link, request.match_code);

    const response = await poll(request.login_request_id, newSessionToken());

    expect(response.status).toBe(404);
    expect(await response.json()).toEqual({ error: "not_found" });
  });
});

describe("login request validation", () => {
  async function validBody() {
    return loginRequestBody(uniqueEmail(), newSessionToken());
  }

  it.each([
    ["a missing email", { email: undefined }],
    ["a non-string email", { email: 42 }],
    ["an email without @", { email: "member.example.com" }],
    ["an email over 254 characters", { email: `${"a".repeat(250)}@example.com` }],
    ["a missing session_token_sha256", { session_token_sha256: undefined }],
    ["an uppercase session_token_sha256", { session_token_sha256: "A".repeat(64) }],
    ["a short session_token_sha256", { session_token_sha256: "a".repeat(63) }],
    ["a missing request_id", { request_id: undefined }],
    ["a UUIDv4 request_id", { request_id: "6f1c1b7e-2f4a-4b8e-9c1d-3e5f7a9b1c2d" }],
    ["an unknown field", { install_id: "laptop" }],
  ])("rejects %s with 400 invalid_request", async (_case, change) => {
    const response = await postJson("/v1/auth/login-requests", { ...(await validBody()), ...change });

    expect(response.status).toBe(400);
    expect(await response.json()).toMatchObject({ error: "invalid_request" });
  });

  it("rejects a body that is not JSON", async () => {
    const response = await call("/v1/auth/login-requests", { method: "POST", body: "email=x" });

    expect(response.status).toBe(400);
    expect(await response.json()).toMatchObject({ error: "invalid_request" });
  });

  it("rejects a session token hash that another login request already uses", async () => {
    const sessionToken = newSessionToken();
    await postJson("/v1/auth/login-requests", await loginRequestBody(uniqueEmail(), sessionToken));

    const response = await postJson(
      "/v1/auth/login-requests",
      await loginRequestBody(uniqueEmail(), sessionToken),
    );

    expect(response.status).toBe(400);
    expect(await response.json()).toMatchObject({ error: "invalid_request" });
  });

  it.each([
    ["a missing session_token", { request_id: uuidv7() }],
    ["a session_token without the hpm_ prefix", { request_id: uuidv7(), session_token: "x".repeat(47) }],
    ["an unknown field", { request_id: uuidv7(), session_token: newSessionToken(), extra: 1 }],
  ])("rejects a poll with %s", async (_case, body) => {
    const { request } = await startLogin(uniqueEmail());

    const response = await postJson(`/v1/auth/login-requests/${request.login_request_id}/poll`, body);

    expect(response.status).toBe(400);
    expect(await response.json()).toMatchObject({ error: "invalid_request" });
  });
});

describe("request limits", () => {
  it("rejects bodies over 16 KB with 413", async () => {
    const response = await postJson("/v1/auth/login-requests", {
      ...(await loginRequestBody(uniqueEmail(), newSessionToken())),
      padding: "x".repeat(17 * 1024),
    });

    expect(response.status).toBe(413);
    expect(await response.json()).toEqual({ error: "payload_too_large" });
  });

  it.each(["/v1/gateway/usage", "/v1/gateway/line-events"])(
    "lets gateway batches to %s reach 128 KB, as the contract's x-max-body-bytes says",
    async (path) => {
      const within = await call(path, { method: "POST", body: "x".repeat(128 * 1024) });
      const over = await call(path, { method: "POST", body: "x".repeat(128 * 1024 + 1) });

      expect(within.status).not.toBe(413);
      expect(over.status).toBe(413);
    },
  );

  it("caps every other route at 16 KB", async () => {
    const response = await call("/v1/gateway/leases", { method: "POST", body: "x".repeat(16 * 1024 + 1) });

    expect(response.status).toBe(413);
  });

  it("refuses to send links when PUBLIC_BASE_URL is not set", async () => {
    const response = await postJson(
      "/v1/auth/login-requests",
      await loginRequestBody(uniqueEmail(), newSessionToken()),
      { PUBLIC_BASE_URL: "" },
    );

    expect(response.status).toBe(503);
    expect(await response.json()).toEqual({ error: "email_unavailable" });
  });
});
