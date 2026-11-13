import { afterEach, describe, expect, it, vi } from "vitest";
import { call, postForm, postJson, startLogin, tokenFrom, uniqueEmail } from "./helpers";

const MINUTE = 60 * 1000;

afterEach(() => {
  vi.useRealTimers();
});

function poll(request: { login_request_id: string; poll_secret: string }) {
  return postJson(`/v1/auth/login-requests/${request.login_request_id}/poll`, {
    poll_secret: request.poll_secret,
  });
}

describe("magic link tokens", () => {
  it("are single-use", async () => {
    const { request, link } = await startLogin(uniqueEmail());
    const fields = { token: tokenFrom(link), match_code: request.match_code };

    expect((await postForm("/auth/confirm", fields)).status).toBe(200);

    const again = await postForm("/auth/confirm", fields);
    expect(again.status).toBe(410);
    expect(await again.text()).toContain("expired or was already used");
    expect((await call(link.pathname + link.search)).status).toBe(410);
  });

  it("still work just before 15 minutes", async () => {
    vi.useFakeTimers({ toFake: ["Date"], now: Date.parse("2026-11-13T10:00:00Z") });
    const { request, link } = await startLogin(uniqueEmail());

    vi.setSystemTime(Date.parse("2026-11-13T10:14:59Z"));
    expect((await call(link.pathname + link.search)).status).toBe(200);
    const confirm = await postForm("/auth/confirm", {
      token: tokenFrom(link),
      match_code: request.match_code,
    });
    expect(confirm.status).toBe(200);
    expect((await poll(request)).status).toBe(200);
  });

  it("expire after 15 minutes", async () => {
    vi.useFakeTimers({ toFake: ["Date"], now: Date.parse("2026-11-13T10:00:00Z") });
    const { request, link } = await startLogin(uniqueEmail());

    vi.setSystemTime(Date.parse("2026-11-13T10:00:00Z") + 15 * MINUTE);

    expect((await call(link.pathname + link.search)).status).toBe(410);
    const confirm = await postForm("/auth/confirm", {
      token: tokenFrom(link),
      match_code: request.match_code,
    });
    expect(confirm.status).toBe(410);
    const expired = await poll(request);
    expect(expired.status).toBe(410);
    expect(await expired.json()).toEqual({ error: "expired" });
  });
});

describe("match code", () => {
  it("must agree, or the sign-in is not confirmed", async () => {
    const { request, link } = await startLogin(uniqueEmail());
    const wrongCode = request.match_code === "000000" ? "000001" : "000000";

    const confirm = await postForm("/auth/confirm", { token: tokenFrom(link), match_code: wrongCode });

    expect(confirm.status).toBe(400);
    expect(await confirm.text()).toContain("Codes do not match");
    expect((await poll(request)).status).toBe(202);
  });

  it("is required", async () => {
    const { request, link } = await startLogin(uniqueEmail());

    const confirm = await postForm("/auth/confirm", { token: tokenFrom(link) });

    expect(confirm.status).toBe(400);
    expect((await poll(request)).status).toBe(202);
  });
});

describe("polling", () => {
  it("hands out the session token only once", async () => {
    const { request, link } = await startLogin(uniqueEmail());
    await postForm("/auth/confirm", { token: tokenFrom(link), match_code: request.match_code });

    expect((await poll(request)).status).toBe(200);
    const second = await poll(request);
    expect(second.status).toBe(410);
    expect(await second.json()).toEqual({ error: "already_collected" });
  });

  it("can collect a session for 5 minutes after a confirmation made just before expiry", async () => {
    vi.useFakeTimers({ toFake: ["Date"], now: Date.parse("2026-11-13T10:00:00Z") });
    const { request, link } = await startLogin(uniqueEmail());

    vi.setSystemTime(Date.parse("2026-11-13T10:14:30Z"));
    await postForm("/auth/confirm", { token: tokenFrom(link), match_code: request.match_code });

    vi.setSystemTime(Date.parse("2026-11-13T10:19:29Z"));
    expect((await poll(request)).status).toBe(200);
  });

  it("cannot collect a session more than 5 minutes after a late confirmation", async () => {
    vi.useFakeTimers({ toFake: ["Date"], now: Date.parse("2026-11-13T10:00:00Z") });
    const { request, link } = await startLogin(uniqueEmail());

    vi.setSystemTime(Date.parse("2026-11-13T10:14:30Z"));
    await postForm("/auth/confirm", { token: tokenFrom(link), match_code: request.match_code });

    vi.setSystemTime(Date.parse("2026-11-13T10:19:30Z"));
    const late = await poll(request);
    expect(late.status).toBe(410);
    expect(await late.json()).toEqual({ error: "expired" });
  });

  it("needs the poll secret", async () => {
    const { request, link } = await startLogin(uniqueEmail());
    await postForm("/auth/confirm", { token: tokenFrom(link), match_code: request.match_code });

    const wrongSecret = await poll({ ...request, poll_secret: "not-the-secret" });
    expect(wrongSecret.status).toBe(404);
    expect((await poll(request)).status).toBe(200);
  });
});
