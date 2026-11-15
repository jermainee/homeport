// Fake Go services for designing the screens in a browser (dev only).
// ?preview=signedOut|approve|offline|expired|home|unreachable picks the
// starting screen; the flow then runs with fake answers. On home,
// &share=off|connecting|online|paused|offline|gateway|error picks Share's
// state, &share=unverified|processing|requires_input the identity, and
// &use=ready|empty|broke|backup|unreachable|nolines how Use starts; &tab=use
// opens Use.

import type { IdentityApi } from "./identity";
import type { ShareApi, ShareStatus } from "./share";
import type { AuthApi, LoginStatus, Member } from "./signin";
import type { Balance, City, Device, ExitLine, Region, UseApi } from "./use";

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
    // The identity check is previewIdentity's; once it says verified, so
    // does the API.
    async identity() {
      await sleep(600);
      return "verified";
    },
  };
}

// The identity check: Stripe takes a moment, then reports processing and
// verified, as a member who finishes the check sees it.
export function previewIdentity(): IdentityApi {
  let polls = 0;
  return {
    async start() {
      await sleep(700);
      polls = 0;
      return "waiting";
    },
    async poll() {
      polls++;
      return polls < 2 ? "waiting" : polls < 4 ? "processing" : "verified";
    },
  };
}

// Where free lines are: Germany has more cities than the list shows, so the
// city search appears.
const previewCities: Record<string, City[]> = {
  DE: [
    ["Berlin", 17], ["Munich", 9], ["Hamburg", 7], ["Frankfurt am Main", 6], ["Cologne", 4], ["Leipzig", 3],
    ["Stuttgart", 3], ["Düsseldorf", 2], ["Dresden", 2], ["Bremen", 1], ["Hanover", 1], ["Nuremberg", 1],
  ].map(([city, lines]) => ({ city: city as string, lines: lines as number })),
  AT: [{ city: "Vienna", lines: 3 }, { city: "Graz", lines: 1 }],
  PT: [{ city: "Lisbon", lines: 2 }, { city: "Porto", lines: 1 }],
  FR: [{ city: "Lyon", lines: 1 }, { city: "Villeurbanne", lines: 1 }],
};
const previewCountries: Region[] = Object.entries(previewCities).map(([country, cities]) => ({
  country,
  lines: cities.reduce((sum, c) => sum + c.lines, 0) + (country === "DE" ? 2 : 0),
}));
const previewIsps: Record<string, string> = { DE: "Deutsche Telekom", AT: "A1", PT: "MEO", FR: "Orange" };

// In the backup preview, the travel router's line in Lyon is offline, and a
// line in Villeurbanne carries it meanwhile.
const offlineLine = "01a1c4af-8a0f-7f5c-b0ff-05e16a4457e8";
const backupExit = { lineId: "01a1c4af-8a0f-7f5c-b0ff-05e16a4457e9", country: "FR", city: "Villeurbanne" };

// Where a device's session exits, as the gateway reports it: its own line
// while that is online, else a backup line as its failover allows.
function exitOf(d: Pick<Device, "lineId" | "country" | "city" | "failover">, since: string): ExitLine | null {
  if (d.lineId !== offlineLine) return { lineId: d.lineId, country: d.country, city: d.city, since };
  return d.failover === "off" ? null : { ...backupExit, since };
}

// A real config's QR code, made by go-qrcode as UseService makes it, for a
// throwaway key: it scans in the WireGuard app, and connects nowhere.
const previewQR = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAATQAAAE0AQMAAACYT13cAAAABlBMVEX///8AAABVwtN+AAADZElEQVR42uzay3HzPBYG4c7gzT/LzqCnCMq/P29mCy98RBEgzsMqwuJFgMxf/MXdqJzbSrdo2PYU5pO+6gRXiANaDUC2QLjsUtfG834KmRu4xPwVrpliA4jJBH+HE9d4XiXJ2NnzvgODOUiQMjTKn+fLDVf5f+LH9XbDAdgcDOlZLAbIz7jhLGHAnD6breWI+V83brkIJzF8FofxornmVTeTT0at6cyGRMZVR2wbJ40ytwLUlt51qTNwjCSYlLQEvOoWG5s0HE89hyxryFWnc7g2nvbUsAmnpbuuBsxFsYGTUBvu+356xw16j3us9+Nnm346B1ddQiYxWQpz2wxk86oz6s3GyCJIPP0Q7jrbu3ah+9yHtq1lt124tCLRwL1vJLzrto2FarL2dkOGxPKqS5wtlfwes6icPbjqcGt+3SLHKQxWw7zrkkYpWrIkvgZXrrtO2+fbDbENILflCLnriGLMpRsm2lp41lfdSSbsvYTAkk1kbt51GxkzV59xwFIXfcNb7j3WaOjAIKWvp4x3XeAoYE9UMgVZJlx1ThoHHAJzAmd7ddfxgGUbNJ7c2mwnJ5cdIigKJpGyKUi3nRrWrFJtp9KG5mU3UdFAkWieCFGvulm+/djs2QrWPgNmuetaA80jp6MQkK/iogNMNoYFO6tNRNi46jY3oowtl+B21pZ3XaVjYzX1qW1TAPdjnvWCk2ZoJxe6Yi6Z6F23PJG0CTO3F+eUq67nVYBDmdpgY5HddiPIEHr/wgzDoN1261lkow0zmnOnJnnXURoCaw2kwsSG3HWnKeIz8THHgkXbL3ALkaHUiLlygxledU1QEYKxoeFnXLXLDjpBDkDQt3eAK666gg1kVrZ81kxY3XanLuZGyWhC8HOm4ZKrBqRzsFpzxJSBdx0VYxJbMEbme3awf07TG84x2wqJr7N0YK3Iqy729XuIhdA4LXP++zy/5M4H//nlgW0EDkH24/8ArjitDXpgc/hU32v+2R53HcDT7kLkqQIQTeZl9x4yjhqWcMoFZXcdQLK2BpM4yzBE7rpK8onYW/B52HxarzpBPTxmom6w9Sa869JeZ+pTjtX3HMlvcOLXF1coDt0iue8WowS1tx+Tp7rbDlyTNpw41Hc3d939N2X5vEQWxNq2N3PV/cVfXIv/DQCyuTTms+uOSgAAAABJRU5ErkJggg==";

// &use=ready|empty|broke|backup|unreachable picks how Use starts. On
// backup, the travel router's line in Lyon is offline, so it runs on a
// backup line nearby. The laptop has HTTPS only off, for SSH.
export function previewUse(start: string): UseApi {
  let balance: Balance =
    start === "empty" || start === "broke"
      ? { purchased: 0, earned: 0, total: 0, bytes: 0 }
      : { purchased: 1_000_500, earned: 41_250, total: 1_041_750, bytes: 104_175_000_000 };
  let devices: Device[] =
    start === "empty" || start === "broke"
      ? []
      : [
          start === "backup"
            ? { peerId: "p-router", name: "Travel router", lineId: offlineLine, country: "FR", city: "Lyon", isp: "Orange", failover: "country", httpsOnly: true, exitLine: null, ipv4: "10.77.0.3", ipv6: "fd77::3", createdAt: "2026-11-13T18:20:00Z" }
            : { peerId: "p-router", name: "Travel router", lineId: "01a1c4af-8a0f-7f5c-b0ff-05e16a4457e6", country: "DE", city: "Berlin", isp: "Deutsche Telekom", failover: "country", httpsOnly: true, exitLine: null, ipv4: "10.77.0.3", ipv6: "fd77::3", createdAt: "2026-11-13T18:20:00Z" },
          { peerId: "p-laptop", name: "Laptop", lineId: "01a1c4af-8a0f-7f5c-b0ff-05e16a4457e7", country: "PT", city: "Lisbon", isp: "MEO", failover: "country", httpsOnly: false, exitLine: null, ipv4: "10.77.0.2", ipv6: "fd77::2", createdAt: "2026-11-10T08:00:00Z" },
        ];
  const now = new Date().toISOString();
  devices = devices.map((d) => ({ ...d, exitLine: exitOf(d, now) }));
  let checkoutPolls = 0;
  let n = 4;
  return {
    async balance() {
      if (start === "unreachable") throw fail("offline");
      return balance;
    },
    async buyCredits() {
      await sleep(600);
      checkoutPolls = 0;
      return balance;
    },
    async pollCheckout() {
      if (++checkoutPolls < 3) return { arrived: false, balance };
      balance = { ...balance, purchased: balance.purchased + 1_000_000, total: balance.total + 1_000_000, bytes: balance.bytes + 100_000_000_000 };
      return { arrived: true, balance };
    },
    async countries() {
      await sleep(300);
      return start === "nolines" ? [] : previewCountries;
    },
    async cities(country) {
      await sleep(250);
      return previewCities[country] ?? [];
    },
    async devices() {
      return devices;
    },
    async addDevice(country, city, name) {
      await sleep(700);
      const device: Device = {
        peerId: `p-${n}`,
        name,
        lineId: `01a1c4af-8a0f-7f5c-b0ff-05e16a4457f${n}`,
        country,
        city: city || previewCities[country]?.[0]?.city || "",
        isp: previewIsps[country] ?? "",
        failover: "country",
        httpsOnly: true,
        exitLine: null,
        ipv4: `10.77.0.${n}`,
        ipv6: `fd77::${n}`,
        createdAt: new Date().toISOString(),
      };
      n++;
      devices = [device, ...devices];
      return { device, qrCode: previewQR, fileName: `Homeport-${name.replace(/[^A-Za-z0-9]+/g, "-")}.conf`.slice(0, 20) };
    },
    async saveConfig() {
      await sleep(400);
      return true;
    },
    async forgetConfig() {},
    async removeDevice(peerId) {
      await sleep(500);
      devices = devices.filter((d) => d.peerId !== peerId);
    },
    async updateDevice(peerId, change) {
      await sleep(500);
      const old = devices.find((d) => d.peerId === peerId);
      if (!old) throw fail("device_gone");
      const moved: Device =
        change.country === undefined
          ? { ...old }
          : {
              ...old,
              lineId: `01a1c4af-8a0f-7f5c-b0ff-05e16a4457f${n++}`,
              country: change.country,
              city: change.city || previewCities[change.country]?.[0]?.city || "",
              isp: previewIsps[change.country] ?? "",
            };
      if (change.failover) moved.failover = change.failover;
      const device: Device = { ...moved, exitLine: exitOf(moved, new Date().toISOString()) };
      devices = devices.map((d) => (d.peerId === peerId ? device : d));
      return device;
    },
    async setHttpsOnly(peerId, on) {
      await sleep(400);
      const old = devices.find((d) => d.peerId === peerId);
      if (!old) throw fail("device_gone");
      const device: Device = { ...old, httpsOnly: on };
      devices = devices.map((d) => (d.peerId === peerId ? device : d));
      return device;
    },
  };
}
