import { Hono } from "hono";
import { approveWithSessionToken } from "./auth/login-requests";
import { apiError, hasOnlyFields, invalidRequest, readJsonObject } from "./http";
import { KEY32 } from "./keys";

/**
 * Shortcuts for local development and automated tests. Every route here
 * answers 404 unless ENVIRONMENT is exactly "dev".
 */
export const devRoutes = new Hono<{ Bindings: Env }>();

devRoutes.use(async (c, next) => {
  if (c.env.ENVIRONMENT !== "dev") return apiError(c, 404, "not_found");
  await next();
});

/** Approves a login request as if the member had confirmed the emailed link. */
devRoutes.post("/login-requests/:id/confirm", async (c) => {
  const body = await readJsonObject(c.req.raw);
  const sessionToken = body?.["session_token"];
  if (typeof sessionToken !== "string") return invalidRequest(c, "session_token is required");

  const result = await approveWithSessionToken(c.env.DB, {
    id: c.req.param("id"),
    sessionToken,
    now: Date.now(),
  });
  switch (result) {
    case "not_found":
      return apiError(c, 404, "not_found");
    case "expired":
      return apiError(c, 410, "login_request_expired");
    case "approved":
      return c.json({ status: "approved" });
  }
});

const GATEWAY_FIELDS = ["wg_public_key", "wg_endpoint", "quic_endpoint", "quic_server_name", "quic_spki_sha256", "version"];
const HOST_PORT = /^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]+):[0-9]{1,5}$/;
const HOSTNAME = /^(?=.{1,253}$)[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$/;
// The contract's Version pattern.
const SEMVER = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$/;

/**
 * Stores a gateway as PUT /v1/gateway/self will once gateways authenticate,
 * so lines and WireGuard configs can be created in local runs before then.
 * The body is the contract's GatewaySelf.
 */
devRoutes.put("/gateways/:id", async (c) => {
  const body = await readJsonObject(c.req.raw);
  if (!body || !hasOnlyFields(body, GATEWAY_FIELDS) || !GATEWAY_FIELDS.every((field) => typeof body[field] === "string")) {
    return invalidRequest(c, `body must have exactly ${GATEWAY_FIELDS.join(", ")}`);
  }
  const gateway = body as Record<string, string>;
  if (
    !KEY32.test(gateway["wg_public_key"] ?? "") ||
    !HOST_PORT.test(gateway["wg_endpoint"] ?? "") ||
    !HOST_PORT.test(gateway["quic_endpoint"] ?? "") ||
    !HOSTNAME.test(gateway["quic_server_name"] ?? "") ||
    !SEMVER.test(gateway["version"] ?? "") ||
    !/^[0-9a-f]{64}$/.test(gateway["quic_spki_sha256"] ?? "")
  ) {
    return invalidRequest(c, "malformed gateway field");
  }
  await c.env.DB.prepare(
    `INSERT INTO gateways (id, wg_public_key, wg_endpoint, quic_endpoint, quic_server_name, quic_spki_sha256, version, updated_at)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?)
     ON CONFLICT (id) DO UPDATE SET
       wg_public_key = excluded.wg_public_key, wg_endpoint = excluded.wg_endpoint,
       quic_endpoint = excluded.quic_endpoint, quic_server_name = excluded.quic_server_name,
       quic_spki_sha256 = excluded.quic_spki_sha256, version = excluded.version, updated_at = excluded.updated_at`,
  )
    .bind(
      c.req.param("id"),
      gateway["wg_public_key"],
      gateway["wg_endpoint"],
      gateway["quic_endpoint"],
      gateway["quic_server_name"],
      gateway["quic_spki_sha256"],
      gateway["version"],
      Date.now(),
    )
    .run();
  return c.body(null, 204);
});
