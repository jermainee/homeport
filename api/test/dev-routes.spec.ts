import { afterEach, describe, expect, it, vi } from "vitest";
import {
  loginRequestBody,
  newSessionToken,
  poll,
  postJson,
  startLogin,
  uniqueEmail,
} from "./helpers";

afterEach(() => {
  vi.restoreAllMocks();
});

function devConfirm(loginRequestId: string, sessionToken: string, envOverrides: Partial<Env> = {}) {
  return postJson(
    `/dev/login-requests/${loginRequestId}/confirm`,
    { session_token: sessionToken },
    envOverrides,
  );
}

describe("dev-only routes", () => {
  it("approve a login request without the email link when ENVIRONMENT=dev", async () => {
    const { request, sessionToken } = await startLogin(uniqueEmail());

    const response = await devConfirm(request.login_request_id, sessionToken, { ENVIRONMENT: "dev" });

    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({ status: "approved" });
    expect((await poll(request.login_request_id, sessionToken)).status).toBe(200);
  });

  it("need the login request's session token", async () => {
    const { request, sessionToken } = await startLogin(uniqueEmail());

    const response = await devConfirm(request.login_request_id, newSessionToken(), {
      ENVIRONMENT: "dev",
    });

    expect(response.status).toBe(404);
    expect((await poll(request.login_request_id, sessionToken)).status).toBe(202);
  });

  it.each(["production", "staging", ""])("return 404 when ENVIRONMENT=%j", async (environment) => {
    const { request, sessionToken } = await startLogin(uniqueEmail());

    const response = await devConfirm(request.login_request_id, sessionToken, {
      ENVIRONMENT: environment,
    });

    expect(response.status).toBe(404);
    expect((await poll(request.login_request_id, sessionToken)).status).toBe(202);
  });
});

describe("email log mode", () => {
  it("is refused outside dev, so production never logs sign-in links", async () => {
    const log = vi.spyOn(console, "log");

    const response = await postJson(
      "/v1/auth/login-requests",
      await loginRequestBody(uniqueEmail(), newSessionToken()),
      { ENVIRONMENT: "production", EMAIL_MODE: "log" },
    );

    expect(response.status).toBe(503);
    expect(await response.json()).toEqual({ error: "email_unavailable" });
    expect(JSON.stringify(log.mock.calls)).not.toContain("magic_link_email");
  });
});
