import { env } from "cloudflare:workers";

/** Inserts a member directly, as a first sign-in would, and returns its id. */
export async function createMember(): Promise<string> {
  const id = crypto.randomUUID();
  await env.DB.prepare(`INSERT INTO members (id, email, created_at) VALUES (?, ?, ?)`)
    .bind(id, `ledger-${id}@example.com`, Date.now())
    .run();
  return id;
}

/** Transactions whose entries do not sum to zero; empty when the ledger balances. */
export async function unbalancedTransactions(): Promise<{ transaction_id: string; total: number }[]> {
  const { results } = await env.DB.prepare(
    `SELECT t.id AS transaction_id, COALESCE(SUM(e.amount), 0) AS total
       FROM ledger_transactions t LEFT JOIN ledger_entries e ON e.transaction_id = t.id
      GROUP BY t.id HAVING COALESCE(SUM(e.amount), 0) <> 0 OR COUNT(e.id) < 2`,
  ).all<{ transaction_id: string; total: number }>();
  return results;
}

/** Accounts whose cached balance differs from the sum of their entries; empty when they agree. */
export async function driftedBalances(): Promise<{ id: string; balance: number; sum: number }[]> {
  const { results } = await env.DB.prepare(
    `SELECT a.id, a.balance, COALESCE(SUM(e.amount), 0) AS sum
       FROM ledger_accounts a LEFT JOIN ledger_entries e ON e.account_id = a.id
      GROUP BY a.id HAVING a.balance <> COALESCE(SUM(e.amount), 0)`,
  ).all<{ id: string; balance: number; sum: number }>();
  return results;
}

export async function platformBalance(kind: "platform_revenue" | "credits_issued" | "dev_grants"): Promise<number> {
  const row = await env.DB.prepare(`SELECT balance FROM ledger_accounts WHERE id = ?`)
    .bind(`platform:${kind}`)
    .first<{ balance: number }>();
  if (!row) throw new Error(`no platform account ${kind}`);
  return row.balance;
}
