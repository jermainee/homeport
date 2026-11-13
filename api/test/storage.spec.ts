import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { poll, sha256Hex, signIn, startLogin, tokenFrom, uniqueEmail } from "./helpers";

/** Every row of every table the sign-in touches, serialized, to search for leaked plaintext. */
async function dumpTables(): Promise<string> {
  const tables = ["members", "login_requests", "magic_link_tokens", "auth_sessions", "idempotency_records"];
  const rows = await Promise.all(
    tables.map((table) => env.DB.prepare(`SELECT * FROM ${table}`).all()),
  );
  return JSON.stringify(rows.map((result) => result.results));
}

describe("secret storage", () => {
  it("stores the link token and the app's session token only as SHA-256 hashes", async () => {
    const { request, sessionToken, link } = await startLogin(uniqueEmail());
    const token = tokenFrom(link);

    const stored = await env.DB.prepare(
      `SELECT r.session_token_hash, t.token_hash
         FROM login_requests r JOIN magic_link_tokens t ON t.login_request_id = r.id
        WHERE r.id = ?`,
    )
      .bind(request.login_request_id)
      .first<{ session_token_hash: string; token_hash: string }>();

    expect(stored).toEqual({
      session_token_hash: await sha256Hex(sessionToken),
      token_hash: await sha256Hex(token),
    });
    const dump = await dumpTables();
    expect(dump).not.toContain(sessionToken);
    expect(dump).not.toContain(token);
  });

  it("activates the session under the hash only, even after polls that carry the token", async () => {
    const { sessionToken } = await signIn(uniqueEmail());

    const stored = await env.DB.prepare(`SELECT token_hash FROM auth_sessions WHERE token_hash = ?`)
      .bind(await sha256Hex(sessionToken))
      .first<{ token_hash: string }>();

    expect(stored).not.toBeNull();
    expect(await dumpTables()).not.toContain(sessionToken);
  });

  it("keeps no plaintext token from a pending poll either", async () => {
    const { request, sessionToken } = await startLogin(uniqueEmail());
    await poll(request.login_request_id, sessionToken);

    expect(await dumpTables()).not.toContain(sessionToken);
  });
});
