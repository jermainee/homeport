import { describe, expect, it } from "vitest";
import { fallbackLines } from "../src/wireguard/failover";

describe("fallbackLines", () => {
  it("spreads the members of one line over its fallback lines, same-city lines always first", () => {
    const candidates = Array.from({ length: 20 }, (_, i) => [
      { peer_id: `peer-${i}`, member_id: `member-${i}`, line_id: "munich", same_city: 0 },
      { peer_id: `peer-${i}`, member_id: `member-${i}`, line_id: "berlin-a", same_city: 1 },
      { peer_id: `peer-${i}`, member_id: `member-${i}`, line_id: "berlin-b", same_city: 1 },
    ]).flat();

    const lists = [...fallbackLines(candidates).values()];

    expect(new Set(lists.map((list) => list[0]))).toEqual(new Set(["berlin-a", "berlin-b"]));
    for (const list of lists) expect(list[2]).toBe("munich");
  });
});
