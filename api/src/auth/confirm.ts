import { Hono } from "hono";
import { confirmLink, findPendingLink } from "./login-requests";
import { PAGE_HEADERS, confirmPage, messagePage } from "./pages";

/** Browser pages behind the emailed sign-in link. */
export const confirmPages = new Hono<{ Bindings: Env }>();

confirmPages.use(async (c, next) => {
  await next();
  for (const [name, value] of Object.entries(PAGE_HEADERS)) c.header(name, value);
});

confirmPages.get("/confirm", async (c) => {
  const token = c.req.query("token") ?? "";
  const link = token ? await findPendingLink(c.env.DB, token, Date.now()) : null;
  if (!link) return c.html(invalidLinkPage(), 410);
  return c.html(confirmPage({ email: link.email, matchCode: link.matchCode, token }));
});

confirmPages.post("/confirm", async (c) => {
  const form = await c.req.parseBody();
  const token = typeof form["token"] === "string" ? form["token"] : "";
  const matchCode = typeof form["match_code"] === "string" ? form["match_code"] : "";

  const result = await confirmLink(c.env.DB, { token, matchCode, now: Date.now() });
  switch (result) {
    case "invalid_link":
      return c.html(invalidLinkPage(), 410);
    case "code_mismatch":
      return c.html(
        messagePage(
          "Codes do not match",
          "This sign-in was not confirmed. Start the sign-in again in the Homeport app.",
        ),
        400,
      );
    case "confirmed":
      return c.html(
        messagePage("You are signed in", "Return to the Homeport app. You can close this page."),
      );
  }
});

function invalidLinkPage() {
  return messagePage(
    "Link expired",
    "This sign-in link has expired or was already used. Start the sign-in again in the Homeport app.",
  );
}
