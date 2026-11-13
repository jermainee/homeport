import { bodyLimit } from "hono/body-limit";
import { createMiddleware } from "hono/factory";

const KB = 1024;

/** The body limit for every route the table below does not name. */
export const DEFAULT_MAX_BODY_BYTES = 16 * KB;

/**
 * Larger limits, copied from each operation's `x-max-body-bytes` in
 * contracts/control-api.openapi.yaml. A full batch of 100 events stays under
 * 128 KB.
 */
const MAX_BODY_BYTES: Readonly<Record<string, number>> = {
  "/v1/gateway/usage": 128 * KB,
  "/v1/gateway/line-events": 128 * KB,
};

/** The largest request body `path` accepts. */
export function maxBodyBytes(path: string): number {
  return MAX_BODY_BYTES[path] ?? DEFAULT_MAX_BODY_BYTES;
}

/**
 * Rejects bodies over the route's limit with 413 before anything reads them.
 * One global middleware, so a route without an entry gets the default.
 */
export const limitBodySize = createMiddleware((c, next) =>
  bodyLimit({
    maxSize: maxBodyBytes(c.req.path),
    onError: (c) => c.json({ error: "payload_too_large" }, 413),
  })(c, next),
);
