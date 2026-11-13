// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SignIn } from "./signin";
import { FakeAuth, serviceError } from "./test/fake-auth";
import { FakeShare } from "./test/fake-share";
import { mount } from "./ui";

let auth: FakeAuth;
let signIn: SignIn;
let app: HTMLElement;
let announcer: HTMLElement;

beforeEach(async () => {
  vi.useFakeTimers();
  document.body.innerHTML = `<main id="app"></main><p id="announcer" role="status"></p>`;
  app = document.getElementById("app")!;
  announcer = document.getElementById("announcer")!;
  auth = new FakeAuth();
  signIn = new SignIn(auth);
  mount(signIn, app, announcer, new FakeShare());
  await signIn.boot();
});

afterEach(() => {
  signIn.dispose();
  vi.useRealTimers();
});

async function submitEmail(value: string) {
  const input = app.querySelector<HTMLInputElement>("#email")!;
  input.value = value;
  app.querySelector<HTMLFormElement>("form")!.requestSubmit();
  await vi.advanceTimersByTimeAsync(0);
}

function click(action: string) {
  app.querySelector<HTMLElement>(`[data-action="${action}"]`)!.click();
}

describe("the sign-in screen", () => {
  it("keeps focus on the email field after a typo, with the text selected", async () => {
    // Email inputs have no selection range to read (selectionStart is null
    // by spec), so watch select() instead.
    const select = vi.spyOn(HTMLInputElement.prototype, "select");

    await submitEmail("member.example.com");

    const input = app.querySelector<HTMLInputElement>("#email")!;
    expect(document.activeElement).toBe(input);
    expect(select.mock.contexts.at(-1)).toBe(input);
    expect(input.getAttribute("aria-invalid")).toBe("true");
    select.mockRestore();
  });
});

describe("the waiting screen", () => {
  beforeEach(async () => {
    await submitEmail("member@example.com");
  });

  it("hangs the plate once, not again when the connection drops", async () => {
    expect(app.dataset.fresh).toBeDefined();
    expect(app.querySelector(".plate")?.getAttribute("aria-label")).toBe("Match code 4 8 2 9 1 3");

    auth.pollResults = [serviceError("offline")];
    await vi.advanceTimersByTimeAsync(2000);

    expect(app.querySelector(".status")?.textContent).toContain("can't be reached");
    expect(app.dataset.fresh).toBeUndefined();
    expect(announcer.textContent).toContain("can't be reached");
  });

  it("hangs a new plate for a new code", async () => {
    click("resend");
    await vi.advanceTimersByTimeAsync(0);

    expect(app.querySelector(".plate")?.getAttribute("aria-label")).toBe("Match code 4 8 2 9 1 4");
    expect(app.dataset.fresh).toBeDefined();
  });

  it("says when a new link can't be sent and keeps the first code", async () => {
    auth.startError = serviceError("rate_limited");

    click("resend");
    await vi.advanceTimersByTimeAsync(0);

    expect(app.querySelector(".plate")?.getAttribute("aria-label")).toBe("Match code 4 8 2 9 1 3");
    expect(app.querySelector("[role=alert]")?.textContent).toContain("Too many sign-in attempts");
  });

  it("goes back to the email when asked", async () => {
    click("change-email");
    await vi.advanceTimersByTimeAsync(0);

    expect(auth.cancels).toBe(1);
    expect(app.querySelector<HTMLInputElement>("#email")?.value).toBe("member@example.com");
  });
});
