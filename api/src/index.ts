import { Hono } from "hono";
import { routePath } from "hono/route";
import { authApi } from "./auth/api";
import { confirmPages } from "./auth/confirm";
import { limitBodySize } from "./body-limits";
import { devRoutes } from "./dev";
import { devicesApi } from "./devices";
import { linesApi } from "./lines/api";
import { meApi } from "./me";
import { wireguardApi } from "./wireguard/api";

const app = new Hono<{ Bindings: Env }>();

app.use(limitBodySize);

app.route("/v1/auth", authApi);
app.route("/v1/me", meApi);
app.route("/v1/devices", devicesApi);
app.route("/v1/lines", linesApi);
app.route("/v1/wireguard", wireguardApi);
app.route("/auth", confirmPages);
app.route("/dev", devRoutes);

app.notFound((c) => c.json({ error: "not_found" }, 404));

app.onError((error, c) => {
  // Log only what went wrong and where, never request bodies or headers.
  console.error(
    JSON.stringify({
      event: "unhandled_error",
      method: c.req.method,
      route: routePath(c),
      error: error.name,
      message: error.message,
    }),
  );
  return c.json({ error: "internal_error" }, 500);
});

export default app;
