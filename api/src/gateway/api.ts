import { Hono } from "hono";
import { invalidRequest, readJsonObject } from "../http";
import { type IdempotentVariables, idempotent } from "../idempotency";
import { type GatewayVariables, requireGateway } from "./auth";
import { gatewayConfig, matchesEtag } from "./config";
import { createLease, returnLease } from "./leases";
import { reportLineEvents } from "./line-events";
import { parseGatewaySelf, storeGatewaySelf } from "./self";
import { reportUsage } from "./usage";

/**
 * The endpoints edge gateways call with their token (contract tag
 * "gateway"). Member session tokens get 401 here.
 */
export const gatewayApi = new Hono<{ Bindings: Env; Variables: GatewayVariables & IdempotentVariables }>();

gatewayApi.use(requireGateway);

gatewayApi.put("/self", async (c) => {
  const self = parseGatewaySelf(await readJsonObject(c.req.raw));
  if (!self) return invalidRequest(c, "body must be the contract's GatewaySelf");
  await storeGatewaySelf(c.env.DB, c.get("gateway").id, self, Date.now()).run();
  return c.body(null, 204);
});

gatewayApi.get("/config", async (c) => {
  const config = await gatewayConfig(c.env.DB, c.env, c.get("gateway").id);
  c.header("etag", config.etag);
  if (matchesEtag(c.req.header("if-none-match"), config.etag)) return c.body(null, 304);
  return c.json(config, 200);
});

gatewayApi.post("/leases", idempotent, createLease);
gatewayApi.post("/leases/:lease_id/return", idempotent, returnLease);
gatewayApi.post("/usage", reportUsage);
gatewayApi.post("/line-events", reportLineEvents);
