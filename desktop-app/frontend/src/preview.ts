// Fake AuthService and ShareService for designing the screens in a browser
// (dev only). ?preview=signedOut|approve|offline|expired|home|unreachable
// picks the starting screen; the flow then runs with fake answers. On home,
// &share=off|connecting|online|paused|offline|gateway|error picks Share's
// state, and &share=unverified|processing|requires_input the identity.

import type { ShareApi, ShareStatus } from "./share";
import type { AuthApi, LoginStatus, Member } from "./signin";

const identities = ["unverified", "processing", "requires_input"];

const fail = (code: string) => Object.assign(new Error(code), { cause: { code } });

function previewMember(): Member {
  const share = new URLSearchParams(location.search).get("share") ?? "";
  return {
    memberId: "01a1c4af-8795-7a28-9249-adbd2ba1c04a",
    email: "member@example.com",
    identityStatus: identities.includes(share) ? share : "verified",
    credits: { purchased: 0, earnedPending: 41_250, earnedCashable: 12_000 },
  };
}

export function previewAuth(start: string): AuthApi {
  const member = previewMember();
  let signedIn = start === "home";
  let polls = 0;
  const pollAnswer = (): LoginStatus => {
    polls++;
    if (start === "expired") return "expired";
    if (start === "offline") throw fail("offline");
    return start === "approve" && polls > 2 ? "approved" : "pending";
  };
  return {
    async current() {
      if (start === "unreachable") throw fail("offline");
      return signedIn ? member : null;
    },
    async start(email) {
      await new Promise((r) => setTimeout(r, 600));
      return { email, matchCode: "482913", expiresAt: new Date(Date.now() + 15 * 60_000).toISOString() };
    },
    async poll() {
      const status = pollAnswer();
      if (status === "approved") signedIn = true;
      return { status };
    },
    async cancel() {},
    async signOut() {
      signedIn = false;
    },
  };
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

const startStatus: Record<string, ShareStatus> = {
  off: { state: "off", sharing: false, bytesToday: 0, flowsActive: 0 },
  connecting: { state: "connecting", sharing: true, bytesToday: 0, flowsActive: 0 },
  online: { state: "online", sharing: true, bytesToday: 1_243_800_000, flowsActive: 3 },
  paused: { state: "paused", sharing: true, bytesToday: 312_400_000, flowsActive: 0 },
  offline: { state: "offline", sharing: true, error: "offline", bytesToday: 52_000_000, flowsActive: 0 },
  gateway: { state: "offline", sharing: true, error: "gateway_unavailable", bytesToday: 0, flowsActive: 0 },
  error: { state: "error", sharing: false, error: "unexpected", bytesToday: 0, flowsActive: 0 },
};

export function previewShare(start: string): ShareApi {
  let status = startStatus[start] ?? startStatus.off!;
  let checks = 0;
  const listeners = new Set<(s: ShareStatus) => void>();
  const publish = (s: Partial<ShareStatus>) => {
    status = { ...status, ...s };
    for (const fn of listeners) fn(status);
  };
  // While online, members use the line: the count goes up every second.
  setInterval(() => {
    if (status.state !== "online") return;
    publish({ bytesToday: status.bytesToday + 180_000 + Math.round(Math.random() * 900_000) });
  }, 1000);

  return {
    async status() {
      return status;
    },
    async setSharing(on) {
      await sleep(500);
      if (!on) {
        status = { ...status, state: "off", sharing: false, error: undefined, flowsActive: 0 };
        return status;
      }
      status = { ...status, state: "connecting", sharing: true, error: undefined };
      void sleep(1500).then(() => status.sharing && publish({ state: "online", flowsActive: 2 }));
      return status;
    },
    onStatus(fn) {
      listeners.add(fn);
      return () => void listeners.delete(fn);
    },
    async identity() {
      await sleep(600);
      return ++checks > 1 ? "verified" : "processing";
    },
  };
}
