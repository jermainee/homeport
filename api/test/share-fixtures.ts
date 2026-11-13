import { env } from "cloudflare:workers";
import { call, signIn, uniqueEmail, uuidv7 } from "./helpers";

/** The Frankfurt gateway from the contract's examples. */
export const GATEWAY = {
  wg_public_key: "YND9Jx2S347AP/Ywks3VrMciTOYivx8pQxgWoPy1HHY=",
  wg_endpoint: "fra1.gw.homeport.example:51820",
  quic_endpoint: "fra1.gw.homeport.example:443",
  quic_server_name: "fra1.gw.homeport.example",
  quic_spki_sha256: "2ca04a183e495439c9a77a4ce5f11babba2758321eb6db8d31e85b3980aea90a",
  version: "0.1.0",
};

/** Stores the example gateway, as PUT /v1/gateway/self will. */
export async function seedGateway(): Promise<string> {
  const id = crypto.randomUUID();
  await env.DB.prepare(
    `INSERT INTO gateways (id, wg_public_key, wg_endpoint, quic_endpoint, quic_server_name, quic_spki_sha256, version, updated_at)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
  )
    .bind(id, GATEWAY.wg_public_key, GATEWAY.wg_endpoint, GATEWAY.quic_endpoint, GATEWAY.quic_server_name, GATEWAY.quic_spki_sha256, GATEWAY.version, Date.now())
    .run();
  return id;
}

/** A signed-in member who passed Stripe Identity. */
export async function verifiedMember(): Promise<{ sessionToken: string; memberId: string }> {
  const member = await signIn(uniqueEmail());
  await env.DB.prepare(`UPDATE members SET identity_status = 'verified' WHERE id = ?`).bind(member.memberId).run();
  return member;
}

/** 32 random bytes in standard base64, like a WireGuard or raw Ed25519 public key. */
export function randomKey(): string {
  let binary = "";
  for (const byte of crypto.getRandomValues(new Uint8Array(32))) binary += String.fromCharCode(byte);
  return btoa(binary);
}

export function send(method: string, path: string, sessionToken: string, body?: unknown, init: RequestInit = {}): Promise<Response> {
  return call(path, {
    ...init,
    method,
    headers: { authorization: `Bearer ${sessionToken}`, ...(body === undefined ? {} : { "content-type": "application/json" }) },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

export async function registerDevice(sessionToken: string, agentPublicKey = randomKey()): Promise<string> {
  const response = await send("POST", "/v1/devices", sessionToken, {
    request_id: uuidv7(),
    name: "Mac mini",
    platform: "macos",
    agent_public_key: agentPublicKey,
  });
  if (response.status !== 201) throw new Error(`device registration failed: ${response.status}`);
  return (await response.json<{ device_id: string }>()).device_id;
}

/** A line of `owner`'s new device, with sharing on. */
export async function sharingLine(sessionToken: string, cf?: Partial<IncomingRequestCfProperties>): Promise<string> {
  const deviceId = await registerDevice(sessionToken);
  const created = await send("POST", "/v1/lines", sessionToken, { request_id: uuidv7(), device_id: deviceId }, cf ? { cf } : {});
  if (created.status !== 201) throw new Error(`line creation failed: ${created.status}`);
  const { line_id } = await created.json<{ line_id: string }>();
  const shared = await send("PATCH", `/v1/lines/${line_id}`, sessionToken, { sharing_enabled: true });
  if (shared.status !== 200) throw new Error(`sharing failed: ${shared.status}`);
  return line_id;
}

/** Links two members, as a shared household, card or Connect account will. */
export async function link(memberId: string, linkedMemberId: string, reason: string): Promise<void> {
  await env.DB.prepare(`INSERT INTO member_links (member_id, linked_member_id, reason, created_at) VALUES (?, ?, ?, ?)`)
    .bind(memberId, linkedMemberId, reason, Date.now())
    .run();
}
