import { describe, expect, it } from "vitest";
import {
  CREDIT_PACK_CREDITS,
  CREDIT_PACK_NET_CENTS,
  LEASE_BYTES,
  LEASE_CREDITS,
  LEASE_GRACE_SECONDS,
  LEASE_RENEW_AT_BYTES,
  SHARE_CREDITS_PER_MB,
  USE_CREDITS_PER_MB,
  creditsForBytes,
  leaseTerms,
} from "../src/pricing";

describe("pricing", () => {
  it("charges 10 credits per MB used and pays 5 per MB shared, rounding down", () => {
    // The worked example of the plan: 123,456,789 bytes.
    expect(creditsForBytes(123_456_789, USE_CREDITS_PER_MB)).toBe(1_234);
    expect(creditsForBytes(123_456_789, SHARE_CREDITS_PER_MB)).toBe(617);
  });

  it("counts 1 MB as 10^6 bytes", () => {
    expect(creditsForBytes(999_999, USE_CREDITS_PER_MB)).toBe(9);
    expect(creditsForBytes(1_000_000, USE_CREDITS_PER_MB)).toBe(10);
    expect(creditsForBytes(0, USE_CREDITS_PER_MB)).toBe(0);
  });

  it("stays exact for byte totals up to the largest safe integer", () => {
    expect(creditsForBytes(Number.MAX_SAFE_INTEGER, USE_CREDITS_PER_MB)).toBe(90_071_992_547);
    expect(creditsForBytes(Number.MAX_SAFE_INTEGER, SHARE_CREDITS_PER_MB)).toBe(45_035_996_273);
  });

  it("refuses byte counts that are not non-negative safe integers", () => {
    expect(() => creditsForBytes(-1, USE_CREDITS_PER_MB)).toThrow(RangeError);
    expect(() => creditsForBytes(1.5, USE_CREDITS_PER_MB)).toThrow(RangeError);
    expect(() => creditsForBytes(2 ** 53, USE_CREDITS_PER_MB)).toThrow(RangeError);
  });

  it("sizes a lease at 2,000 credits = 200,000,000 bytes, renewed at 80%", () => {
    expect(LEASE_CREDITS).toBe(2_000);
    expect(LEASE_BYTES).toBe(200_000_000);
    expect(LEASE_RENEW_AT_BYTES).toBe(160_000_000);
    expect(LEASE_GRACE_SECONDS).toBe(300);
  });

  it("sells 1,000,000 credits for €40 net", () => {
    expect(CREDIT_PACK_CREDITS).toBe(1_000_000);
    expect(CREDIT_PACK_NET_CENTS).toBe(4_000);
  });

  it("turns a lease's credits into bytes at the use rate, renewed at 80%", () => {
    expect(leaseTerms(LEASE_CREDITS)).toEqual({ credits: 2_000, grantedBytes: LEASE_BYTES, renewAtBytes: LEASE_RENEW_AT_BYTES });
    expect(leaseTerms(500)).toEqual({ credits: 500, grantedBytes: 50_000_000, renewAtBytes: 40_000_000 });
    expect(leaseTerms(1)).toEqual({ credits: 1, grantedBytes: 100_000, renewAtBytes: 80_000 });
  });

  it("never grants a lease of no credits, of part of a credit, or larger than a full one", () => {
    for (const credits of [0, -1, 1.5, LEASE_CREDITS + 1]) expect(() => leaseTerms(credits)).toThrow(RangeError);
  });
});
