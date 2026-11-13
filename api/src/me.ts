import { Hono } from "hono";
import { requireSession, type SessionVariables } from "./auth/middleware";
import { type Balances, getBalances } from "./ledger";

export const meApi = new Hono<{ Bindings: Env; Variables: SessionVariables }>();

meApi.use(requireSession);

meApi.get("/", async (c) => {
  const member = c.get("member");
  const balances = await getBalances(c.env.DB, member.id);
  return c.json({
    member_id: member.id,
    email: member.email,
    identity_status: member.identityStatus,
    credits: balances ? shownBalances(balances) : { purchased: 0, earned_pending: 0, earned_cashable: 0 },
  });
});

/**
 * Balances as the app shows them. An account can go below zero when metered
 * usage overruns a balance; the contract's credits are never negative, so
 * such an account shows 0.
 */
function shownBalances(balances: Balances): Balances {
  return {
    purchased: Math.max(balances.purchased, 0),
    earned_pending: Math.max(balances.earned_pending, 0),
    earned_cashable: Math.max(balances.earned_cashable, 0),
  };
}
