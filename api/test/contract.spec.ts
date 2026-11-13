import { describe, expect, it } from "vitest";
import { contractErrors } from "./contract";

describe("contract check in tests", () => {
  it("accepts the contract's own example of GET /v1/me", async () => {
    const example = {
      member_id: "01a1c4af-8795-7a28-9249-adbd2ba1c04a",
      email: "member@example.com",
      identity_status: "verified",
      credits: { purchased: 998766, earned_pending: 617, earned_cashable: 0 },
    };

    expect(await contractErrors("GET", "/v1/me", 200, example)).toEqual([]);
  });

  it("rejects a body the contract does not allow", async () => {
    const errors = await contractErrors("GET", "/v1/me", 200, { member_id: "x", extra: true });

    expect(errors.length).toBeGreaterThan(0);
  });

  it("rejects a status the operation does not define", async () => {
    const errors = await contractErrors("GET", "/v1/me", 418, {});

    expect(errors).toEqual([expect.stringContaining("418")]);
  });
});
