import { describe, expect, it } from "vitest";
import { type FailoverLine, type FailoverPeer, fallbackLines } from "../src/wireguard/failover";

describe("fallbackLines", () => {
  const lines: FailoverLine[] = [
    { line_id: "berlin-own", owner_member_id: "owner-0", country: "DE", city: "Berlin", online: 1 },
    { line_id: "munich", owner_member_id: "owner-1", country: "DE", city: "Munich", online: 1 },
    { line_id: "berlin-a", owner_member_id: "owner-2", country: "DE", city: "Berlin", online: 1 },
    { line_id: "berlin-b", owner_member_id: "owner-3", country: "DE", city: "Berlin", online: 1 },
  ];
  const peers: FailoverPeer[] = Array.from({ length: 20 }, (_, i) => ({
    peer_id: `peer-${i}`,
    member_id: `member-${i}`,
    line_id: "berlin-own",
    failover: "country",
    country: "DE",
    city: "Berlin",
  }));
  const nothingLinked = { users: new Map<string, Set<string>>(), linked: () => false };

  it("spreads the members of one line over its fallback lines, same-city lines always first", () => {
    const lists = [...fallbackLines(peers, lines, nothingLinked).values()];

    expect(new Set(lists.map((list) => list[0]))).toEqual(new Set(["berlin-a", "berlin-b"]));
    for (const list of lists) expect(list[2]).toBe("munich");
  });

  it("gives every device of a member the same lines in the same order", () => {
    const phone = peers[0] as FailoverPeer;
    const laptop = { ...phone, peer_id: "laptop" };

    const result = fallbackLines([phone, laptop], lines, nothingLinked);

    expect(result.get("laptop")).toEqual(result.get(phone.peer_id));
  });

  it("puts online lines first within the city and within the country, so the few listed ones work", () => {
    const berlin = Array.from({ length: 6 }, (_, i) => ({
      line_id: `berlin-${i}`,
      owner_member_id: `owner-b${i}`,
      country: "DE",
      city: "Berlin",
      online: i === 4 ? 1 : 0,
    }));
    const munich = Array.from({ length: 3 }, (_, i) => ({
      line_id: `munich-${i}`,
      owner_member_id: `owner-m${i}`,
      country: "DE",
      city: "Munich",
      online: i === 2 ? 1 : 0,
    }));

    for (const list of fallbackLines(peers, [...munich, ...berlin], nothingLinked).values()) {
      expect(list[0]).toBe("berlin-4");
      expect(list).toHaveLength(5);
      expect(list.slice(1)).not.toContain("munich-2");
    }
  });
});
