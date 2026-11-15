import { env } from "cloudflare:workers";
import { gatewayCall } from "./gateway-fixtures";
import { uuidv7 } from "./helpers";
import { sharingLine, verifiedMember } from "./share-fixtures";

export type Member = { sessionToken: string; memberId: string };

/** Where a line is, as Cloudflare saw the request that created it. */
export interface Place {
  country: string;
  city?: string;
  isp?: string;
}

/**
 * Hands out the user-assigned ISO 3166 codes in `codes`, one per test, so
 * lines of one test never count in another's region.
 */
export function countryPool(codes: string[]): () => string {
  const left = [...codes];
  return () => {
    const code = left.shift();
    if (!code) throw new Error("out of test country codes");
    return code;
  };
}

/** A shared line at `place`, served by `gateway`, whose agent is connected. */
export async function onlineLine(gateway: { gatewayId: string; token: string }, place: Place, owner?: Member): Promise<string> {
  const member = owner ?? (await verifiedMember());
  const cf = {
    country: place.country,
    ...(place.city !== undefined ? { city: place.city } : {}),
    ...(place.isp ? { asOrganization: place.isp } : {}),
  } as Partial<IncomingRequestCfProperties>;
  const lineId = await sharingLine(member.sessionToken, cf);
  await env.DB.prepare(`UPDATE lines SET gateway_id = ? WHERE id = ?`).bind(gateway.gatewayId, lineId).run();
  const response = await gatewayCall(gateway.token, "POST", "/v1/gateway/line-events", {
    events: [
      {
        event_id: uuidv7(),
        line_id: lineId,
        type: "online",
        observed_ip: "203.0.113.47",
        agent_version: "0.1.0",
        at: new Date().toISOString(),
      },
    ],
  });
  if (response.status !== 200) throw new Error(`line event failed: ${response.status}`);
  return lineId;
}
