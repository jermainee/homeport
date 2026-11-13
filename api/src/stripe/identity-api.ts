import Stripe from "stripe";

/** What a verified document says about its holder; the fields of the identity hash. */
export interface VerifiedIdentity {
  documentNumber: string | null;
  /** ISO 3166-1 alpha-2. */
  issuingCountry: string | null;
  dateOfBirth: { day: number; month: number; year: number } | null;
}

/**
 * The calls Homeport makes to Stripe Identity. Tests replace it with a fake
 * (test/fake-stripe.ts); webhook signatures are checked without it.
 */
export interface StripeIdentityApi {
  /**
   * Creates a document check with a matching live selfie for the member and
   * returns the URL of Stripe's hosted page. Stripe answers a repeated
   * idempotency key with the first session.
   */
  createVerificationSession(input: { memberId: string; idempotencyKey: string }): Promise<{ id: string; url: string }>;

  /**
   * Reads the sensitive results of a verified session with the restricted
   * key. Stripe allows this for 48 hours after the verification.
   */
  retrieveVerifiedIdentity(sessionId: string): Promise<VerifiedIdentity>;
}

const API_VERSION = "2026-10-28.endive";

/** The Stripe Identity port backed by Stripe's API. `fetcher` is replaceable for adapter tests. */
export function stripeIdentityApi(env: Env, fetcher: typeof fetch = fetch): StripeIdentityApi {
  const client = (key: string) =>
    new Stripe(key, {
      apiVersion: API_VERSION,
      httpClient: Stripe.createFetchHttpClient(fetcher),
      maxNetworkRetries: 2,
    });

  return {
    async createVerificationSession({ memberId, idempotencyKey }) {
      const session = await client(requireKey(env.STRIPE_SECRET_KEY, "STRIPE_SECRET_KEY")).identity.verificationSessions.create(
        {
          type: "document",
          options: { document: { require_matching_selfie: true, require_live_capture: true } },
          client_reference_id: memberId,
          metadata: { member_id: memberId },
        },
        { idempotencyKey },
      );
      if (!session.url) throw new Error("Stripe returned a verification session without a URL");
      return { id: session.id, url: session.url };
    },

    async retrieveVerifiedIdentity(sessionId) {
      const session = await client(
        requireKey(env.STRIPE_IDENTITY_RESTRICTED_KEY, "STRIPE_IDENTITY_RESTRICTED_KEY"),
      ).identity.verificationSessions.retrieve(sessionId, {
        expand: ["verified_outputs.dob", "last_verification_report.document.number"],
      });
      const report = session.last_verification_report;
      const document = report && typeof report === "object" ? report.document : null;
      const dob = session.verified_outputs?.dob ?? document?.dob ?? null;
      return {
        documentNumber: document?.number ?? null,
        issuingCountry: document?.issuing_country ?? null,
        dateOfBirth:
          dob && dob.day !== null && dob.month !== null && dob.year !== null
            ? { day: dob.day, month: dob.month, year: dob.year }
            : null,
      };
    },
  };
}

function requireKey(value: string | undefined, name: string): string {
  if (!value) throw new Error(`${name} is not set`);
  return value;
}
