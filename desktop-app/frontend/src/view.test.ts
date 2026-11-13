// @vitest-environment happy-dom
import { describe, expect, it } from "vitest";
import { render, shareLine } from "./view";

describe("render", () => {
  it("shows text from the API as text, never as markup", () => {
    const el = document.createElement("div");
    el.innerHTML = render({
      kind: "waiting",
      email: '"><img src=x onerror=alert(1)>@example.com',
      matchCode: '<img src=x onerror="alert(1)">',
      expiresAt: new Date().toISOString(),
      offline: false,
      resending: false,
    });

    expect(el.querySelector("img")).toBeNull();
    expect(el.querySelector(".plate-digits")?.textContent).toBe('<img src=x onerror="alert(1)">');
  });
});

describe("shareLine", () => {
  it("says when Homeport asked Share to wait", () => {
    expect(shareLine({ state: "offline", sharing: true, error: "rate_limited", bytesToday: 0, flowsActive: 0 })).toBe(
      "Homeport asked Share to wait. Trying again later…",
    );
  });
});
