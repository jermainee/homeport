import { describe, expect, it } from "vitest";
import { type FailoverLine, type FailoverPeer, fallbackLines } from "../src/wireguard/failover";

describe("fallbackLines", () => {
  const lines: FailoverLine[] = [
    { line_id: "berlin-own", owner_member_id: "owner-0", country: "DE", city: "Berlin" },
    { line_id: "munich", owner_member_id: "owner-1", country: "DE", city: "Munich" },
    { line_id: "berlin-a", owner_member_id: "owner-2", country: "DE", city: "Berlin" },
    { line_id: "berlin-b", owner_member_id: "owner-3", country: "DE", city: "Berlin" },
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
});
