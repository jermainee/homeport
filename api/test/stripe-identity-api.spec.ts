import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { stripeIdentityApi } from "../src/stripe/identity-api";

interface Recorded {
  method: string;
  url: URL;
  headers: Headers;
  body: string;
}

/** A fetch that records each request and answers with `reply`'s JSON, as Stripe's API would. */
function recordingFetch(reply: (request: Recorded) => unknown) {
  const requests: Recorded[] = [];
  const fetcher = async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = new Request(input, init);
    const recorded = { method: request.method, url: new URL(request.url), headers: request.headers, body: await request.text() };
    requests.push(recorded);
    return Response.json(reply(recorded), { headers: { "request-id": "req_test" } });
  };
  return { fetcher: fetcher, requests };
}

const keys = { ...env, STRIPE_SECRET_KEY: "sk_test_create", STRIPE_IDENTITY_RESTRICTED_KEY: "rk_test_sensitive" };

describe("Stripe Identity adapter", () => {
  it("creates a document check with a matching live selfie, tagged with the member", async () => {
    const { fetcher, requests } = recordingFetch(() => ({
      id: "vs_123",
      object: "identity.verification_session",
      url: "https://verify.stripe.com/start/test_abc",
    }));

    const session = await stripeIdentityApi(keys, fetcher).createVerificationSession({
      memberId: "01a1c4af-8795-7a28-9249-adbd2ba1c04a",
      idempotencyKey: "identity_session:01a1d422-aa20-78e7-a051-1eb39cc68896",
    });

    expect(session).toEqual({ id: "vs_123", url: "https://verify.stripe.com/start/test_abc" });
    const [request] = requests;
    expect(request?.method).toBe("POST");
    expect(request?.url.pathname).toBe("/v1/identity/verification_sessions");
    expect(request?.headers.get("authorization")).toBe("Bearer sk_test_create");
    expect(request?.headers.get("idempotency-key")).toBe("identity_session:01a1d422-aa20-78e7-a051-1eb39cc68896");
    expect(request?.headers.get("stripe-version")).toBe("2026-10-28.endive");
    expect(Object.fromEntries(new URLSearchParams(request?.body))).toEqual({
      type: "document",
      "options[document][require_matching_selfie]": "true",
      "options[document][require_live_capture]": "true",
      client_reference_id: "01a1c4af-8795-7a28-9249-adbd2ba1c04a",
      "metadata[member_id]": "01a1c4af-8795-7a28-9249-adbd2ba1c04a",
    });
  });

  it("reads document number, issuing country and birth date with the restricted key", async () => {
    const { fetcher, requests } = recordingFetch(() => ({
      id: "vs_123",
      object: "identity.verification_session",
      verified_outputs: { dob: { day: 12, month: 8, year: 1964 } },
      last_verification_report: {
        id: "vr_123",
        object: "identity.verification_report",
        document: { number: "C01X00T47", issuing_country: "DE", dob: { day: 12, month: 8, year: 1964 } },
      },
    }));

    const identity = await stripeIdentityApi(keys, fetcher).retrieveVerifiedIdentity("vs_123");

    expect(identity).toEqual({
      documentNumber: "C01X00T47",
      issuingCountry: "DE",
      dateOfBirth: { day: 12, month: 8, year: 1964 },
    });
    const [request] = requests;
    expect(request?.method).toBe("GET");
    expect(request?.url.pathname).toBe("/v1/identity/verification_sessions/vs_123");
    expect(request?.headers.get("authorization")).toBe("Bearer rk_test_sensitive");
    const expand = [...(request?.url.searchParams ?? [])].filter(([key]) => /^expand\[\d*\]$/.test(key)).map(([, value]) => value);
    expect(expand).toEqual([
      "verified_outputs.dob",
      "last_verification_report.document.number",
    ]);
  });

  it("returns nulls for a report without document data", async () => {
    const { fetcher } = recordingFetch(() => ({
      id: "vs_123",
      object: "identity.verification_session",
      verified_outputs: null,
      last_verification_report: null,
    }));

    expect(await stripeIdentityApi(keys, fetcher).retrieveVerifiedIdentity("vs_123")).toEqual({
      documentNumber: null,
      issuingCountry: null,
      dateOfBirth: null,
    });
  });

  it("refuses to call Stripe without its key", async () => {
    const { fetcher, requests } = recordingFetch(() => ({}));

    await expect(
      stripeIdentityApi({ ...keys, STRIPE_SECRET_KEY: "" }, fetcher).createVerificationSession({
        memberId: "m",
        idempotencyKey: "k",
      }),
    ).rejects.toThrow(/STRIPE_SECRET_KEY/);
    expect(requests).toEqual([]);
  });
});
