// A fake AuthService for designing the screens in a browser (dev only).
// ?preview=signedOut|waiting|offline|expired|home|unreachable picks the
// starting screen; the flow then runs with fake answers.

import type { AuthApi, LoginStatus, Member } from "./signin";

const member: Member = {
  memberId: "01a1c4af-8795-7a28-9249-adbd2ba1c04a",
  email: "member@example.com",
  identityStatus: "unverified",
  credits: { purchased: 0, earnedPending: 0, earnedCashable: 0 },
};

const fail = (code: string) => Object.assign(new Error(code), { cause: { code } });

export function previewAuth(start: string): AuthApi {
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
