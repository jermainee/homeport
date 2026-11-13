import { env } from "cloudflare:workers";
import { afterEach, describe, expect, it, vi } from "vitest";
import { requestFingerprint } from "../src/idempotency";
import {
  confirm,
  loginRequestBody,
  newSessionToken,
  poll,
  postJson,
  startLogin,
  uniqueEmail,
  uuidv7,
} from "./helpers";

const HOUR = 60 * 60 * 1000;

afterEach(() => {
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe("request_id", () => {
  it("replays the first response to a retry with the same body, and sends one email", async () => {
    const email = uniqueEmail();
    const body = await loginRequestBody(email, newSessionToken());
    const log = vi.spyOn(console, "log");

    const first = await postJson("/v1/auth/login-requests", body);
    const retry = await postJson("/v1/auth/login-requests", body);

    expect(first.status).toBe(201);
    expect(retry.status).toBe(201);
    expect(await retry.json()).toEqual(await first.json());
    const emails = log.mock.calls.filter((args) => String(args[0]).includes("magic_link_email"));
    expect(emails).toHaveLength(1);
  });

  it("answers 409 idempotency_conflict when the same request_id comes with another body", async () => {
    const body = await loginRequestBody(uniqueEmail(), newSessionToken());
    await postJson("/v1/auth/login-requests", body);

    const conflict = await postJson("/v1/auth/login-requests", { ...body, email: uniqueEmail() });

    expect(conflict.status).toBe(409);
    expect(await conflict.json()).toMatchObject({ error: "idempotency_conflict" });
  });

  it("answers 409 when a request_id is reused on another endpoint", async () => {
    const body = await loginRequestBody(uniqueEmail(), newSessionToken());
    const created = await postJson("/v1/auth/login-requests", body);
    const { login_request_id } = await created.json<{ login_request_id: string }>();

    const conflict = await poll(login_request_id, newSessionToken(), body.request_id);

    expect(conflict.status).toBe(409);
  });

  it("replays a poll, so the app sends a new request_id for each poll", async () => {
    const { request, sessionToken, link } = await startLogin(uniqueEmail());
    const requestId = uuidv7();
    expect((await poll(request.login_request_id, sessionToken, requestId)).status).toBe(202);

    await confirm(link, request.match_code);

    expect((await poll(request.login_request_id, sessionToken, requestId)).status).toBe(202);
    expect((await poll(request.login_request_id, sessionToken)).status).toBe(200);
  });

  it("does not keep a 5xx response, so a retry can succeed", async () => {
    const body = await loginRequestBody(uniqueEmail(), newSessionToken());

    const failed = await postJson("/v1/auth/login-requests", body, { PUBLIC_BASE_URL: "" });
    const retry = await postJson("/v1/auth/login-requests", body);

    expect(failed.status).toBe(503);
    expect(retry.status).toBe(201);
  });

  it("accepts a request_id from 23 hours ago", async () => {
    const body = await loginRequestBody(uniqueEmail(), newSessionToken());

    const response = await postJson("/v1/auth/login-requests", {
      ...body,
      request_id: uuidv7(Date.now() - 23 * HOUR),
    });

    expect(response.status).toBe(201);
  });

  it("rejects a request_id more than 24 hours old with 400 request_id_expired", async () => {
    const body = await loginRequestBody(uniqueEmail(), newSessionToken());

    const response = await postJson("/v1/auth/login-requests", {
      ...body,
      request_id: uuidv7(Date.now() - 24 * HOUR - 1000),
    });

    expect(response.status).toBe(400);
    expect(await response.json()).toMatchObject({ error: "request_id_expired" });
  });

  it("rejects a request_id more than 1 hour in the future with 400 invalid_request", async () => {
    const body = await loginRequestBody(uniqueEmail(), newSessionToken());

    const response = await postJson("/v1/auth/login-requests", {
      ...body,
      request_id: uuidv7(Date.now() + HOUR + 1000),
    });

    expect(response.status).toBe(400);
    expect(await response.json()).toMatchObject({ error: "invalid_request" });
  });
});

describe("a request_id whose first request has not answered yet", () => {
  async function seedClaim(body: Record<string, unknown>, claimedAt: number) {
    const fingerprint = await requestFingerprint("POST", "/v1/auth/login-requests", body);
    await env.DB.prepare(
      `INSERT INTO idempotency_records (request_id, fingerprint, claimed_at, created_at, expires_at)
       VALUES (?, ?, ?, ?, ?)`,
    )
      .bind(body["request_id"], fingerprint, claimedAt, claimedAt, claimedAt + 25 * HOUR)
      .run();
  }

  it("gets 429 rate_limited with Retry-After while the first request may still be running", async () => {
    const body = await loginRequestBody(uniqueEmail(), newSessionToken());
    await seedClaim(body, Date.now() - 5000);

    const retry = await postJson("/v1/auth/login-requests", body);

    expect(retry.status).toBe(429);
    expect(retry.headers.get("retry-after")).toBe("1");
    expect(await retry.json()).toMatchObject({ error: "rate_limited" });
  });

  it("is taken over by a retry once the first request has been silent for a minute", async () => {
    const body = await loginRequestBody(uniqueEmail(), newSessionToken());
    await seedClaim(body, Date.now() - 61_000);

    const retry = await postJson("/v1/auth/login-requests", body);
    const again = await postJson("/v1/auth/login-requests", body);

    expect(retry.status).toBe(201);
    expect(again.status).toBe(201);
    expect(await again.json()).toEqual(await retry.json());
  });
});
