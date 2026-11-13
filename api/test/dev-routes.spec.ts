import { describe, expect, it, vi } from "vitest";
import { postJson, startLogin, uniqueEmail } from "./helpers";

function devConfirm(
  request: { login_request_id: string; poll_secret: string },
  envOverrides: Partial<Env> = {},
) {
  return postJson(
    `/dev/login-requests/${request.login_request_id}/confirm`,
    { poll_secret: request.poll_secret },
    envOverrides,
  );
}

function poll(request: { login_request_id: string; poll_secret: string }) {
  return postJson(`/v1/auth/login-requests/${request.login_request_id}/poll`, {
    poll_secret: request.poll_secret,
  });
}

describe("dev-only routes", () => {
  it("confirm a login request without the email link when ENVIRONMENT=dev", async () => {
    const { request } = await startLogin(uniqueEmail());

    const response = await devConfirm(request, { ENVIRONMENT: "dev" });

    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({ status: "confirmed" });
    expect((await poll(request)).status).toBe(200);
  });

  it("need the poll secret", async () => {
    const { request } = await startLogin(uniqueEmail());

    const response = await devConfirm({ ...request, poll_secret: "wrong" }, { ENVIRONMENT: "dev" });

    expect(response.status).toBe(404);
    expect((await poll(request)).status).toBe(202);
  });

  it.each(["production", "staging", ""])("return 404 when ENVIRONMENT=%j", async (environment) => {
    const { request } = await startLogin(uniqueEmail());

    const response = await devConfirm(request, { ENVIRONMENT: environment });

    expect(response.status).toBe(404);
    expect((await poll(request)).status).toBe(202);
  });
});

describe("email log mode", () => {
  it("is refused outside dev, so production never logs sign-in links", async () => {
    const log = vi.spyOn(console, "log");

    const response = await postJson(
      "/v1/auth/login-requests",
      { email: uniqueEmail() },
      { ENVIRONMENT: "production", EMAIL_MODE: "log" },
    );

    expect(response.status).toBe(503);
    expect(await response.json()).toEqual({ error: "email_unavailable" });
    expect(JSON.stringify(log.mock.calls)).not.toContain("magic_link_email");
    log.mockRestore();
  });
});
