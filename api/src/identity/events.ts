import type Stripe from "stripe";
import type { StripeIdentityApi } from "../stripe/identity-api";
import { identityHash } from "./hash";

/** The Stripe Identity events Homeport acts on, and the status each means while the member is not verified. */
const STATUS_BY_EVENT: Readonly<Record<string, "processing" | "requires_input">> = {
  "identity.verification_session.processing": "processing",
  "identity.verification_session.requires_input": "requires_input",
  // A canceled check was abandoned; the member starts a new one.
  "identity.verification_session.canceled": "requires_input",
};

const VERIFIED = "identity.verification_session.verified";

export function isIdentityEvent(type: string): boolean {
  return type === VERIFIED || type in STATUS_BY_EVENT;
}

/**
 * The statements that apply one Stripe Identity event to its member. An
 * event of a session Homeport did not create for that member changes
 * nothing.
 *
 * - `verified` always applies: verification is final, so it wins over any
 *   event that arrives after it, and over older events that arrived first.
 *   In live mode it stores the identity hash, unless another member already
 *   has it: then the member stays at requires_input (one person, one
 *   account). Test mode skips the hash, as every test document is the same.
 * - The other events apply only while the member is not verified, only for
 *   the member's newest check (an abandoned check that ends later says
 *   nothing about the current one), and only if no newer event was applied
 *   already (Stripe does not keep order).
 *
 * members.identity_event_created holds the newest applied event's `created`
 * in epoch milliseconds, like every other timestamp.
 */
export async function identityEventStatements(
  db: D1Database,
  env: Env,
  stripeIdentity: StripeIdentityApi,
  event: Stripe.Event,
): Promise<D1PreparedStatement[]> {
  const session = event.data.object as { id?: unknown; metadata?: Record<string, string> | null };
  const memberId = session.metadata?.["member_id"];
  if (typeof session.id !== "string" || typeof memberId !== "string") return [];
  const known = await db
    .prepare(`SELECT 1 FROM identity_sessions WHERE id = ? AND member_id = ?`)
    .bind(session.id, memberId)
    .first();
  if (!known) return [];

  const created = event.created * 1000;
  const status = STATUS_BY_EVENT[event.type];
  if (status) {
    return [
      db
        .prepare(
          `UPDATE members SET identity_status = ?1, identity_failure = NULL, identity_event_created = ?2
            WHERE id = ?3 AND identity_status <> 'verified'
              AND (identity_event_created IS NULL OR identity_event_created <= ?2)
              AND ?4 = (SELECT s.id FROM identity_sessions s WHERE s.member_id = ?3
                         ORDER BY s.created_at DESC, s.rowid DESC LIMIT 1)`,
        )
        .bind(status, created, memberId, session.id),
    ];
  }
  if (event.type !== VERIFIED) return [];

  // Test mode skips the hash, so it reads nothing sensitive.
  const hash = event.livemode
    ? await identityHash(env.IDENTITY_HASH_KEY, await stripeIdentity.retrieveVerifiedIdentity(session.id))
    : null;
  if (event.livemode && hash === null) return [notVerified(db, memberId, created, "identity_data_missing")];

  return [
    db
      .prepare(
        `UPDATE members SET identity_status = 'verified', identity_hash = ?1, identity_failure = NULL,
                identity_event_created = MAX(COALESCE(identity_event_created, 0), ?2)
          WHERE id = ?3 AND identity_status <> 'verified'
            AND (?1 IS NULL OR NOT EXISTS (SELECT 1 FROM members other WHERE other.identity_hash = ?1 AND other.id <> ?3))`,
      )
      .bind(hash, created, memberId),
    // Runs after the statement above: it matches only if that one did not verify the member.
    db
      .prepare(
        `UPDATE members SET identity_status = 'requires_input', identity_failure = 'duplicate_identity',
                identity_event_created = MAX(COALESCE(identity_event_created, 0), ?2)
          WHERE id = ?3 AND identity_status <> 'verified'
            AND EXISTS (SELECT 1 FROM members other WHERE other.identity_hash = ?1 AND other.id <> ?3)`,
      )
      .bind(hash, created, memberId),
  ];
}

function notVerified(db: D1Database, memberId: string, created: number, failure: string): D1PreparedStatement {
  return db
    .prepare(
      `UPDATE members SET identity_status = 'requires_input', identity_failure = ?1,
              identity_event_created = MAX(COALESCE(identity_event_created, 0), ?2)
        WHERE id = ?3 AND identity_status <> 'verified'`,
    )
    .bind(failure, created, memberId);
}
