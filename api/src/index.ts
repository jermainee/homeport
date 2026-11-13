import { Hono } from "hono";
import { bodyLimit } from "hono/body-limit";
import { routePath } from "hono/route";
import { authApi } from "./auth/api";
import { confirmPages } from "./auth/confirm";
import { devRoutes } from "./dev";
import { meApi } from "./me";

const app = new Hono<{ Bindings: Env }>();

// Every request body this API accepts is a few hundred bytes.
app.use(
  bodyLimit({
    maxSize: 16 * 1024,
    onError: (c) => c.json({ error: "payload_too_large" }, 413),
  }),
);

app.route("/v1/auth", authApi);
app.route("/v1/me", meApi);
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
