import { env } from "cloudflare:workers";
import { Hono } from "hono";
import { describe, expect, it } from "vitest";
import { requireSession } from "../src/auth/middleware";
import { requireVerifiedIdentity } from "../src/identity/require-verified";
import { expectContract } from "./contract";
import { callWith, signIn, uniqueEmail } from "./helpers";

// Devices, lines and peers (P12) sit behind this gate; here it guards a stand-in route.
const app = new Hono<{ Bindings: Env }>();
app.post("/v1/lines", requireSession, requireVerifiedIdentity, (c) => c.json({ ok: true }, 201));

function post(sessionToken: string) {
  return callWith(app, "/v1/lines", { method: "POST", headers: { authorization: `Bearer ${sessionToken}` } });
}

async function setStatus(memberId: string, status: string) {
  await env.DB.prepare(`UPDATE members SET identity_status = ? WHERE id = ?`).bind(status, memberId).run();
}

describe("requireVerifiedIdentity", () => {
  it.each(["unverified", "requires_input", "processing"])("answers 403 identity_required while %s", async (status) => {
    const { sessionToken, memberId } = await signIn(uniqueEmail());
    await setStatus(memberId, status);

    const response = await post(sessionToken);

    expect(response.status).toBe(403);
    expect(await expectContract("POST", "/v1/lines", response)).toEqual({ error: "identity_required" });
  });

  it("lets a verified member through", async () => {
    const { sessionToken, memberId } = await signIn(uniqueEmail());
    await setStatus(memberId, "verified");

    const response = await post(sessionToken);

    expect(response.status).toBe(201);
  });
});
