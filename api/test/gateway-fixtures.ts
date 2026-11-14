import { env } from "cloudflare:workers";
import { newGatewayCredential } from "../src/gateway/auth";
import { call, uuidv7 } from "./helpers";
import { GATEWAY, randomKey, send, sharingLine, verifiedMember } from "./share-fixtures";

/** The Frankfurt gateway's announcement from the contract's examples. */
export const GATEWAY_SELF = GATEWAY;

/** Sends a request with a gateway token. */
export function gatewayCall(
  token: string,
  method: string,
  path: string,
  body?: unknown,
  headers: Record<string, string> = {},
): Promise<Response> {
  return call(path, {
    method,
    headers: {
      authorization: `Bearer ${token}`,
      ...(body === undefined ? {} : { "content-type": "application/json" }),
      ...headers,
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

/** Stores a gateway's credential, as `npm run seed:dev` and provisioning do, and returns its token. */
export async function provisionGateway(): Promise<{ gatewayId: string; token: string }> {
  const credential = await newGatewayCredential();
  await env.DB.prepare(`INSERT INTO gateway_credentials (gateway_id, secret_sha256, created_at) VALUES (?, ?, ?)`)
    .bind(credential.gatewayId, credential.secretSha256, Date.now())
    .run();
  return { gatewayId: credential.gatewayId, token: credential.token };
}

/** A provisioned gateway that announced itself with PUT /v1/gateway/self. */
export async function announcedGateway(): Promise<{ gatewayId: string; token: string }> {
  const gateway = await provisionGateway();
  const response = await gatewayCall(gateway.token, "PUT", "/v1/gateway/self", GATEWAY_SELF);
  if (response.status !== 204) throw new Error(`PUT /v1/gateway/self failed: ${response.status}`);
  return gateway;
}

/**
 * A verified member's shared line, served by `gatewayId`, where `cf` says the
 * device is. Lines go to the newest gateway, so the line is moved to this one.
 */
export async function lineOn(gatewayId: string, cf?: Partial<IncomingRequestCfProperties>) {
  const owner = await verifiedMember();
  const lineId = await sharingLine(owner.sessionToken, cf);
  await env.DB.prepare(`UPDATE lines SET gateway_id = ? WHERE id = ?`).bind(gatewayId, lineId).run();
  return { owner, lineId };
}

/** A verified member's WireGuard peer on `lineId`. */
export async function peerOn(lineId: string, user?: { sessionToken: string; memberId: string }) {
  const member = user ?? (await verifiedMember());
  const response = await send("POST", "/v1/wireguard/peers", member.sessionToken, {
    request_id: uuidv7(),
    line_id: lineId,
    name: "iPhone",
    public_key: randomKey(),
  });
  if (response.status !== 201) throw new Error(`peer creation failed: ${response.status}`);
  const { peer } = await response.json<{ peer: { peer_id: string; ipv4: string; ipv6: string; public_key: string } }>();
  return { user: member, peer };
}

/** Gives a member purchased credits through the dev route. */
export async function giveCredits(memberId: string, credits: number): Promise<void> {
  const response = await call(`/dev/members/${memberId}/credits`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ credits }),
  });
  if (response.status !== 200) throw new Error(`dev grant failed: ${response.status} ${await response.text()}`);
}

/**
 * A gateway serving a peer on a line in Berlin and another Berlin line that
 * takes over while the peer's line is offline. The peer's member holds
 * 1,000,000 credits.
 */
export async function failoverPeer() {
  const gateway = await announcedGateway();
  const primary = await lineOn(gateway.gatewayId, BERLIN);
  const fallback = await lineOn(gateway.gatewayId, BERLIN);
  const { user, peer } = await peerOn(primary.lineId);
  await giveCredits(user.memberId, 1_000_000);
  return { gateway, primary, fallback, user, peer };
}

export const BERLIN: Partial<IncomingRequestCfProperties> = { country: "DE", city: "Berlin" };
export const MUNICH: Partial<IncomingRequestCfProperties> = { country: "DE", city: "Munich" };
export const PARIS: Partial<IncomingRequestCfProperties> = { country: "FR", city: "Paris" };

/** A gateway serving one line with one peer whose member holds `credits`. */
export async function servedPeer(credits = 1_000_000) {
  const gateway = await announcedGateway();
  const { owner, lineId } = await lineOn(gateway.gatewayId);
  const { user, peer } = await peerOn(lineId);
  if (credits > 0) await giveCredits(user.memberId, credits);
  return { gateway, owner, lineId, user, peer };
}

export function requestLease(
  token: string,
  input: { peerId: string; sessionId: string; lineId?: string; supersedes?: string; requestId?: string },
): Promise<Response> {
  return gatewayCall(token, "POST", "/v1/gateway/leases", {
    request_id: input.requestId ?? uuidv7(),
    peer_id: input.peerId,
    session_id: input.sessionId,
    ...(input.lineId === undefined ? {} : { line_id: input.lineId }),
    ...(input.supersedes === undefined ? {} : { supersedes_lease_id: input.supersedes }),
  });
}

export interface LeaseBody {
  lease_id: string;
  granted_bytes: number;
  renew_at_bytes: number;
  expires_at: string;
  grace_seconds: number;
}

export async function grantedLease(
  token: string,
  peerId: string,
  sessionId = uuidv7(),
  lineId?: string,
): Promise<LeaseBody & { sessionId: string }> {
  const response = await requestLease(token, { peerId, sessionId, ...(lineId === undefined ? {} : { lineId }) });
  if (response.status !== 201) throw new Error(`lease failed: ${response.status} ${await response.text()}`);
  return { ...(await response.json<LeaseBody>()), sessionId };
}

export interface UsageEventInput {
  sessionId: string;
  peerId: string;
  lineId: string;
  leaseId: string;
  seq: number;
  bytesUp: number;
  bytesDown: number;
  /** Seconds after `start` the period begins; each period lasts 30 seconds. */
  start?: number;
  final?: boolean;
  eventId?: string;
}

/** A usage event as the gateway cuts it: 30-second periods from a fixed session start. */
export function usageEvent(input: UsageEventInput, sessionStart = SESSION_START) {
  const offset = input.start ?? input.seq * 30;
  const at = (seconds: number) => new Date(sessionStart + seconds * 1000).toISOString().replace(/\.\d{3}Z$/, "Z");
  return {
    event_id: input.eventId ?? uuidv7(),
    session_id: input.sessionId,
    peer_id: input.peerId,
    line_id: input.lineId,
    lease_id: input.leaseId,
    seq: input.seq,
    bytes_up: input.bytesUp,
    bytes_down: input.bytesDown,
    period_start: at(offset),
    period_end: at(offset + 30),
    final: input.final ?? false,
    ...(input.seq === 0 ? { session_start: { started_at: at(offset), exit_ip: "203.0.113.47" } } : {}),
  };
}

/** When test sessions start: a whole second a minute ago. */
export const SESSION_START = Math.floor(Date.now() / 1000) * 1000 - 60_000;

export function reportUsage(token: string, events: unknown[]): Promise<Response> {
  return gatewayCall(token, "POST", "/v1/gateway/usage", { events });
}

export async function balancesOf(memberId: string) {
  const { results } = await env.DB.prepare(`SELECT kind, balance FROM ledger_accounts WHERE member_id = ?`)
    .bind(memberId)
    .all<{ kind: string; balance: number }>();
  return Object.fromEntries(results.map((row) => [row.kind, row.balance])) as Record<
    "purchased" | "earned_pending" | "earned_cashable",
    number
  >;
}
