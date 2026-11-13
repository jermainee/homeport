import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { postForm, postJson, signIn, startLogin, tokenFrom, uniqueEmail } from "./helpers";

async function sha256Hex(value: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value));
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

/** Every row of every auth table, serialized, to search for leaked plaintext. */
async function dumpAuthTables(): Promise<string> {
  const tables = ["members", "login_requests", "magic_link_tokens", "auth_sessions"];
  const rows = await Promise.all(
    tables.map((table) => env.DB.prepare(`SELECT * FROM ${table}`).all()),
  );
  return JSON.stringify(rows.map((result) => result.results));
}

describe("secret storage", () => {
  it("stores the poll secret and the link token only as SHA-256 hashes", async () => {
    const { request, link } = await startLogin(uniqueEmail());
    const token = tokenFrom(link);

    const stored = await env.DB.prepare(
      `SELECT r.poll_secret_hash, t.token_hash
         FROM login_requests r JOIN magic_link_tokens t ON t.login_request_id = r.id
        WHERE r.id = ?`,
    )
      .bind(request.login_request_id)
      .first<{ poll_secret_hash: string; token_hash: string }>();

    expect(stored).toEqual({
      poll_secret_hash: await sha256Hex(request.poll_secret),
      token_hash: await sha256Hex(token),
    });
    const dump = await dumpAuthTables();
    expect(dump).not.toContain(request.poll_secret);
    expect(dump).not.toContain(token);
  });

  it("stores the session token only as a SHA-256 hash", async () => {
    const { sessionToken } = await signIn(uniqueEmail());

    const stored = await env.DB.prepare(`SELECT token_hash FROM auth_sessions WHERE token_hash = ?`)
      .bind(await sha256Hex(sessionToken))
      .first<{ token_hash: string }>();

    expect(stored).not.toBeNull();
    expect(await dumpAuthTables()).not.toContain(sessionToken);
  });

  it("records the app's install id on the session", async () => {
    const installId = crypto.randomUUID();
    const { request, link } = await startLogin(uniqueEmail(), { install_id: installId });
    await postForm("/auth/confirm", { token: tokenFrom(link), match_code: request.match_code });
    const poll = await postJson(`/v1/auth/login-requests/${request.login_request_id}/poll`, {
      poll_secret: request.poll_secret,
    });
    const { session_token } = await poll.json<{ session_token: string }>();

    const stored = await env.DB.prepare(`SELECT install_id FROM auth_sessions WHERE token_hash = ?`)
      .bind(await sha256Hex(session_token))
      .first<{ install_id: string | null }>();

    expect(stored).toEqual({ install_id: installId });
  });
});
