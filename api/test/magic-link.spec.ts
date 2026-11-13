import { afterEach, describe, expect, it, vi } from "vitest";
import { call, confirm, poll, postForm, startLogin, tokenFrom, uniqueEmail } from "./helpers";

const MINUTE = 60 * 1000;
const START = Date.parse("2026-11-13T10:00:00Z");

afterEach(() => {
  vi.useRealTimers();
});

describe("magic link tokens", () => {
  it("are single-use", async () => {
    const { request, link } = await startLogin(uniqueEmail());

    expect((await confirm(link, request.match_code)).status).toBe(200);

    const again = await confirm(link, request.match_code);
    expect(again.status).toBe(410);
    expect(await again.text()).toContain("expired or was already used");
    expect((await call(link.pathname + link.search)).status).toBe(410);
  });

  it("still work just before 15 minutes", async () => {
    vi.useFakeTimers({ toFake: ["Date"], now: START });
    const { request, sessionToken, link } = await startLogin(uniqueEmail());

    vi.setSystemTime(START + 15 * MINUTE - 1000);
    expect((await call(link.pathname + link.search)).status).toBe(200);
    expect((await confirm(link, request.match_code)).status).toBe(200);
    expect((await poll(request.login_request_id, sessionToken)).status).toBe(200);
  });

  it("expire after 15 minutes", async () => {
    vi.useFakeTimers({ toFake: ["Date"], now: START });
    const { request, sessionToken, link } = await startLogin(uniqueEmail());

    vi.setSystemTime(START + 15 * MINUTE);

    expect((await call(link.pathname + link.search)).status).toBe(410);
    expect((await confirm(link, request.match_code)).status).toBe(410);
    const expired = await poll(request.login_request_id, sessionToken);
    expect(expired.status).toBe(410);
    expect(await expired.json()).toEqual({ error: "login_request_expired" });
  });
});

describe("match code", () => {
  it("must agree, or the sign-in is not approved", async () => {
    const { request, sessionToken, link } = await startLogin(uniqueEmail());
    const wrongCode = request.match_code === "000000" ? "000001" : "000000";

    const confirmed = await confirm(link, wrongCode);

    expect(confirmed.status).toBe(400);
    expect(await confirmed.text()).toContain("Codes do not match");
    expect((await poll(request.login_request_id, sessionToken)).status).toBe(202);
  });

  it("is required", async () => {
    const { request, sessionToken, link } = await startLogin(uniqueEmail());

    const confirmed = await postForm("/auth/confirm", { token: tokenFrom(link) });

    expect(confirmed.status).toBe(400);
    expect((await poll(request.login_request_id, sessionToken)).status).toBe(202);
  });
});

describe("approval near expiry", () => {
  it("keeps answering approved for 5 minutes after a confirmation made just before expiry", async () => {
    vi.useFakeTimers({ toFake: ["Date"], now: START });
    const { request, sessionToken, link } = await startLogin(uniqueEmail());

    vi.setSystemTime(START + 14.5 * MINUTE);
    await confirm(link, request.match_code);

    vi.setSystemTime(START + 19.5 * MINUTE - 1000);
    expect((await poll(request.login_request_id, sessionToken)).status).toBe(200);
  });

  it("answers expired more than 5 minutes after a late confirmation", async () => {
    vi.useFakeTimers({ toFake: ["Date"], now: START });
    const { request, sessionToken, link } = await startLogin(uniqueEmail());

    vi.setSystemTime(START + 14.5 * MINUTE);
    await confirm(link, request.match_code);

    vi.setSystemTime(START + 19.5 * MINUTE);
    const late = await poll(request.login_request_id, sessionToken);
    expect(late.status).toBe(410);
    expect(await late.json()).toEqual({ error: "login_request_expired" });
  });
});
