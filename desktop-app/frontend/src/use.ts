// The Use screen as a state machine: the member's credits, buying more, the
// lines they can use, and the devices that use them with the WireGuard
// app. It drives the Go UseService through the UseApi seam and knows
// nothing about the DOM, so it is tested without a webview.

import { startPolling } from "./poll";
import { errorCode, type ErrorCode } from "./signin";

/** services.Balance in Go. */
export interface Balance {
  purchased: number;
  earned: number;
  total: number;
  /** How much data the total pays for. */
  bytes: number;
}

/** services.CheckoutCheck in Go. */
export interface CheckoutCheck {
  arrived: boolean;
  balance: Balance;
}

/** services.Line in Go. */
export interface Line {
  lineId: string;
  /** ISO 3166-1 alpha-2. */
  country: string;
  city: string;
  isp: string;
  ipv6: boolean;
  online: boolean;
}

/**
 * Where a device goes while its line is offline (services.Failover* in Go):
 * another line in the same city, else in the same country; in the same
 * city only; or nowhere until its line is back.
 */
export type Failover = "country" | "city" | "off";

export const FAILOVERS: readonly Failover[] = ["country", "city", "off"];

/** asFailover reads Go's failover, taking anything unknown as the default. */
export function asFailover(s: string): Failover {
  return (FAILOVERS as readonly string[]).includes(s) ? (s as Failover) : "country";
}

/** services.ExitLine in Go: the line a device's session exits through. */
export interface ExitLine {
  lineId: string;
  /** ISO 3166-1 alpha-2, or "" when unknown. */
  country: string;
  city: string;
  since: string;
}

/** services.Device in Go. */
export interface Device {
  peerId: string;
  name: string;
  /** The line the member chose. */
  lineId: string;
  failover: Failover;
  /** The line its session exits through; null without a session. */
  exitLine: ExitLine | null;
  ipv4: string;
  ipv6: string;
  createdAt: string;
}

/**
 * backupLine is the line a device runs on while its own is offline, or
 * undefined while it runs on its own line or has no session.
 */
export function backupLine(d: Device): ExitLine | undefined {
  return d.exitLine && d.exitLine.lineId !== d.lineId ? d.exitLine : undefined;
}

/** What UpdateDevice changes; what is left out stays. */
export interface DeviceChange {
  lineId?: string;
  failover?: Failover;
}

/** services.NewDevice in Go: a device just added, with its config's QR code. */
export interface NewDevice {
  device: Device;
  /** A PNG data URL. */
  qrCode: string;
  fileName: string;
}

/** What the Go UseService offers the webview. */
export interface UseApi {
  balance(): Promise<Balance>;
  buyCredits(): Promise<Balance>;
  pollCheckout(): Promise<CheckoutCheck>;
  lines(): Promise<Line[]>;
  devices(): Promise<Device[]>;
  addDevice(lineId: string, name: string): Promise<NewDevice>;
  /** saveConfig resolves false when the member cancels the save dialog. */
  saveConfig(peerId: string): Promise<boolean>;
  forgetConfig(peerId: string): Promise<void>;
  removeDevice(peerId: string): Promise<void>;
  /** updateDevice moves a device to another line, or changes its failover. */
  updateDevice(peerId: string, change: DeviceChange): Promise<Device>;
}

export type Checkout =
  // arrived: the last purchase's credits are in the balance.
  | { phase: "idle"; error?: ErrorCode; arrived?: boolean }
  // Stripe Checkout is being opened in the browser.
  | { phase: "opening" }
  // The member is paying; the app polls until the credits arrive.
  | { phase: "waiting"; offline?: boolean }
  // Polling gave up; "Check again" asks once more.
  | { phase: "stalled" };

export type Step =
  | { kind: "list" }
  | { kind: "add"; lineId: string | null; name: string; creating: boolean; error?: ErrorCode }
  // The new device's config, shown once.
  | { kind: "config"; device: NewDevice; saving: boolean; saved: boolean; error?: ErrorCode }
  // A device's line and failover, being changed. It keeps its config.
  | { kind: "edit"; peerId: string; lineId: string; failover: Failover; saving: boolean; error?: ErrorCode };

export interface Ready {
  kind: "ready";
  balance: Balance;
  lines: Line[];
  devices: Device[];
  checkout: Checkout;
  step: Step;
  /** A device the member is removing: asked to confirm, or on its way. */
  removing?: { peerId: string; busy: boolean; error?: ErrorCode };
}

export type UseView =
  | { kind: "loading" }
  // Use needs a verified identity.
  | { kind: "verify" }
  | { kind: "failed"; error: ErrorCode }
  | Ready
  // The session is gone; the window goes back to sign-in.
  | { kind: "signedOut" };

/** How often to ask whether a purchase's credits arrived, and for how long. */
export const CHECKOUT_POLL_MS = 3000;
export const CHECKOUT_GIVE_UP_MS = 10 * 60_000;

/**
 * How often the device list follows the gateway while Use is open, so a
 * device moving to a backup line shows. The API keeps a session's line for
 * 3 minutes after the gateway last reported it.
 */
export const DEVICES_REFRESH_MS = 30_000;

/** The contract's limit on a device name. */
export const MAX_DEVICE_NAME = 64;

export class Use {
  #view: UseView = { kind: "loading" };
  #listeners = new Set<(v: UseView) => void>();
  #stopPolling: (() => void) | undefined;
  #disposed = false;
  // Bumped whenever the devices or the balance change other than by a
  // refresh, so a refresh that started earlier doesn't undo the change.
  #devicesVersion = 0;
  // Devices the member moved to another line, and the line each left: the
  // API reports the old session there until the gateway moves the device.
  #movedFrom = new Map<string, string>();
  #balanceVersion = 0;

  constructor(
    private readonly api: UseApi,
    private identity: string,
  ) {}

  get view(): UseView {
    return this.#view;
  }

  /** subscribe calls fn on every view change and returns an unsubscribe. */
  subscribe(fn: (v: UseView) => void): () => void {
    this.#listeners.add(fn);
    return () => this.#listeners.delete(fn);
  }

  /** start loads Use, or asks an unverified member to verify first. */
  async start(): Promise<void> {
    if (this.identity !== "verified") {
      this.#set({ kind: "verify" });
      return;
    }
    await this.#load();
  }

  /** unlock loads Use once the identity check found the member verified. */
  async unlock(): Promise<void> {
    this.identity = "verified";
    if (this.#view.kind === "verify") await this.#load();
  }

  /** retry loads Use again after it failed to load. */
  async retry(): Promise<void> {
    if (this.#view.kind === "failed") await this.#load();
  }

  /** refresh reloads the balance, lines and devices without a loading state. */
  async refresh(): Promise<void> {
    if (this.#view.kind !== "ready") return;
    const devicesVersion = this.#devicesVersion;
    const balanceVersion = this.#balanceVersion;
    let data: [Balance, Line[], Device[]];
    try {
      data = await Promise.all([this.api.balance(), this.api.lines(), this.api.devices()]);
    } catch (err) {
      this.#lost(errorCode(err));
      return;
    }
    const v = this.#ready();
    if (!v) return;
    const [balance, lines, devices] = data;
    this.#set({
      ...v,
      lines,
      balance: balanceVersion === this.#balanceVersion ? balance : v.balance,
      devices: devicesVersion === this.#devicesVersion ? this.#followMoves(devices) : v.devices,
    });
  }

  /**
   * followMoves hides the exit line of a device still on the line it was
   * moved away from: that is its old session, not a backup line. Once the
   * device has left that line, its exit line shows again.
   */
  #followMoves(devices: Device[]): Device[] {
    return devices.map((d) => {
      const from = this.#movedFrom.get(d.peerId);
      if (from === undefined) return d;
      if (d.exitLine?.lineId === from && d.lineId !== from) return { ...d, exitLine: null };
      this.#movedFrom.delete(d.peerId);
      return d;
    });
  }

  /**
   * refreshDevices reloads only the devices, to follow the line each one
   * exits through. A failure is left for the next try, unless it takes
   * Use away.
   */
  async refreshDevices(): Promise<void> {
    if (this.#view.kind !== "ready") return;
    const version = this.#devicesVersion;
    let devices: Device[];
    try {
      devices = await this.api.devices();
    } catch (err) {
      this.#lost(errorCode(err));
      return;
    }
    const v = this.#ready();
    if (v && version === this.#devicesVersion) this.#set({ ...v, devices: this.#followMoves(devices) });
  }

  async #load(): Promise<void> {
    this.#set({ kind: "loading" });
    let data: [Balance, Line[], Device[]];
    try {
      data = await Promise.all([this.api.balance(), this.api.lines(), this.api.devices()]);
    } catch (err) {
      if (this.#disposed) return;
      const code = errorCode(err);
      if (!this.#lost(code)) this.#set({ kind: "failed", error: code });
      return;
    }
    if (this.#disposed) return;
    const [balance, lines, devices] = data;
    this.#set({ kind: "ready", balance, lines, devices, checkout: { phase: "idle" }, step: { kind: "list" } });
  }

  // #lost handles the errors that take Use away: a gone session, or an
  // identity the API no longer accepts. It reports whether it did.
  #lost(code: ErrorCode): boolean {
    if (this.#disposed) return true;
    if (code === "signed_out") {
      this.#stop();
      this.#set({ kind: "signedOut" });
      return true;
    }
    if (code === "identity_required") {
      this.#stop();
      this.identity = "unverified";
      this.#set({ kind: "verify" });
      return true;
    }
    return false;
  }

  /** buy opens Stripe Checkout in the browser and waits for the credits. */
  async buy(): Promise<void> {
    const v = this.#ready();
    if (!v || v.checkout.phase === "opening") return;
    this.#stop();
    this.#set({ ...v, checkout: { phase: "opening" } });
    let balance: Balance;
    try {
      balance = await this.api.buyCredits();
    } catch (err) {
      const code = errorCode(err);
      const now = this.#ready();
      if (!this.#lost(code) && now) this.#set({ ...now, checkout: { phase: "idle", error: code } });
      return;
    }
    const now = this.#ready();
    if (!now) return;
    this.#balanceVersion++;
    this.#set({ ...now, balance, checkout: { phase: "waiting" } });
    this.#waitForCredits();
  }

  /** checkPayment asks once more after waiting for the credits gave up. */
  async checkPayment(): Promise<void> {
    const v = this.#ready();
    if (!v || v.checkout.phase !== "stalled") return;
    this.#set({ ...v, checkout: { phase: "waiting" } });
    let check: CheckoutCheck;
    try {
      check = await this.api.pollCheckout();
    } catch (err) {
      const now = this.#ready();
      if (!this.#lost(errorCode(err)) && now) this.#set({ ...now, checkout: { phase: "stalled" } });
      return;
    }
    if (!this.#arrived(check)) this.#waitForCredits();
  }

  #waitForCredits(): void {
    this.#stop();
    this.#stopPolling = startPolling({
      every: CHECKOUT_POLL_MS,
      giveUpAfter: CHECKOUT_GIVE_UP_MS,
      tick: async (live) => {
        const check = await this.api.pollCheckout();
        if (!live()) return false;
        return !this.#arrived(check);
      },
      failed: (code) => {
        if (this.#lost(code)) return false;
        const v = this.#ready();
        if (!v) return false;
        this.#set({ ...v, checkout: code === "offline" ? { phase: "waiting", offline: true } : { phase: "waiting" } });
        return true;
      },
      gaveUp: () => {
        const v = this.#ready();
        if (v) this.#set({ ...v, checkout: { phase: "stalled" } });
      },
    });
  }

  // #arrived shows a checkout poll's answer and reports whether the
  // credits arrived.
  #arrived(check: CheckoutCheck): boolean {
    const v = this.#ready();
    if (!v) return true;
    this.#balanceVersion++;
    this.#set({
      ...v,
      balance: check.balance,
      checkout: check.arrived ? { phase: "idle", arrived: true } : { phase: "waiting" },
    });
    return check.arrived;
  }

  /** startAdding opens the form for a new device, with an online line picked. */
  startAdding(): void {
    const v = this.#ready();
    if (!v || v.step.kind !== "list") return;
    const line = v.lines.find((l) => l.online) ?? v.lines[0];
    this.#set({ ...v, step: { kind: "add", lineId: line?.lineId ?? null, name: "", creating: false } });
  }

  /** pickLine picks the line the new or changed device will use. */
  pickLine(lineId: string): void {
    const v = this.#ready();
    if (v?.step.kind === "add" && !v.step.creating) {
      this.#set({ ...v, step: { ...v.step, lineId, error: undefined } });
    } else if (v?.step.kind === "edit" && !v.step.saving) {
      this.#set({ ...v, step: { ...v.step, lineId, error: undefined } });
    }
  }

  /**
   * setName keeps the name as the member types it. It tells no listener,
   * so the field isn't redrawn under the caret.
   */
  setName(name: string): void {
    const v = this.#ready();
    if (!v || v.step.kind !== "add") return;
    this.#view = { ...v, step: { ...v.step, name } };
  }

  cancelAdding(): void {
    const v = this.#ready();
    if (!v || v.step.kind !== "add" || v.step.creating) return;
    this.#set({ ...v, step: { kind: "list" } });
  }

  /** create makes the new device's config and shows it, once. */
  async create(): Promise<void> {
    const v = this.#ready();
    if (!v || v.step.kind !== "add" || v.step.creating) return;
    const step = v.step;
    const name = step.name.trim();
    if (name.length === 0 || [...name].length > MAX_DEVICE_NAME) {
      this.#set({ ...v, step: { ...step, error: "invalid_name" } });
      return;
    }
    if (step.lineId === null) {
      this.#set({ ...v, step: { ...step, error: "line_unavailable" } });
      return;
    }
    this.#set({ ...v, step: { ...step, creating: true, error: undefined } });
    let dev: NewDevice;
    try {
      dev = await this.api.addDevice(step.lineId, name);
    } catch (err) {
      await this.#createFailed(step, errorCode(err));
      return;
    }
    const now = this.#ready();
    if (!now) {
      // Use closed meanwhile; nobody will see the config.
      this.#forget(dev.device.peerId);
      return;
    }
    this.#devicesVersion++;
    this.#set({
      ...now,
      devices: [dev.device, ...now.devices.filter((d) => d.peerId !== dev.device.peerId)],
      step: { kind: "config", device: dev, saving: false, saved: false },
    });
  }

  async #createFailed(step: Extract<Step, { kind: "add" }>, code: ErrorCode): Promise<void> {
    if (this.#lost(code)) return;
    let lineId = step.lineId;
    let lines: Line[] | undefined;
    if (code === "line_unavailable") {
      // The line stopped sharing: show the lines there are now.
      try {
        lines = await this.api.lines();
      } catch {
        // The old list stays; the error already says what happened.
      }
      if (lines && !lines.some((l) => l.lineId === lineId)) lineId = null;
    }
    const v = this.#ready();
    if (!v) return;
    this.#set({ ...v, lines: lines ?? v.lines, step: { ...step, lineId, creating: false, error: code } });
  }

  /** save asks where to save the config file and saves it there. */
  async save(): Promise<void> {
    const v = this.#ready();
    if (!v || v.step.kind !== "config" || v.step.saving) return;
    const step = v.step;
    this.#set({ ...v, step: { ...step, saving: true, error: undefined } });
    let saved: boolean;
    try {
      saved = await this.api.saveConfig(step.device.device.peerId);
    } catch (err) {
      const code = errorCode(err);
      const now = this.#ready();
      if (!this.#lost(code) && now?.step.kind === "config") {
        this.#set({ ...now, step: { ...now.step, saving: false, error: code } });
      }
      return;
    }
    const now = this.#ready();
    if (now?.step.kind === "config") {
      this.#set({ ...now, step: { ...now.step, saving: false, saved: now.step.saved || saved } });
    }
  }

  /** done closes the config. Its private key is gone for good. */
  async done(): Promise<void> {
    const v = this.#ready();
    if (!v || v.step.kind !== "config") return;
    const peerId = v.step.device.device.peerId;
    this.#set({ ...v, step: { kind: "list" } });
    await this.#forget(peerId);
  }

  // #forget tells Go to drop a config. If that call fails, Go drops it
  // anyway at sign-out or quit; nothing here can show it again.
  async #forget(peerId: string): Promise<void> {
    try {
      await this.api.forgetConfig(peerId);
    } catch {
      // See above.
    }
  }

  /** startEditing opens a device's line and failover, to change them. */
  startEditing(peerId: string): void {
    const v = this.#ready();
    if (!v || v.step.kind !== "list") return;
    const d = v.devices.find((d) => d.peerId === peerId);
    if (!d) return;
    this.#set({
      ...v,
      removing: undefined,
      step: { kind: "edit", peerId, lineId: d.lineId, failover: d.failover, saving: false },
    });
  }

  /** pickFailover picks where the device goes while its line is offline. */
  pickFailover(failover: Failover): void {
    const v = this.#ready();
    if (!v || v.step.kind !== "edit" || v.step.saving) return;
    this.#set({ ...v, step: { ...v.step, failover, error: undefined } });
  }

  cancelEditing(): void {
    const v = this.#ready();
    if (!v || v.step.kind !== "edit" || v.step.saving) return;
    this.#set({ ...v, step: { kind: "list" } });
  }

  /**
   * saveDevice sends what the member changed, and only that, so a change
   * made elsewhere to the other setting survives.
   */
  async saveDevice(): Promise<void> {
    const v = this.#ready();
    if (!v || v.step.kind !== "edit" || v.step.saving) return;
    const step = v.step;
    const d = v.devices.find((d) => d.peerId === step.peerId);
    const change: DeviceChange = {};
    if (!d) {
      // Removed elsewhere while the form was open.
      this.#set({ ...v, step: { ...step, error: "device_gone" } });
      return;
    }
    if (step.lineId !== d.lineId) change.lineId = step.lineId;
    if (step.failover !== d.failover) change.failover = step.failover;
    if (Object.keys(change).length === 0) {
      this.#set({ ...v, step: { kind: "list" } });
      return;
    }
    this.#set({ ...v, step: { ...step, saving: true, error: undefined } });
    let dev: Device;
    try {
      dev = await this.api.updateDevice(step.peerId, change);
    } catch (err) {
      await this.#saveFailed(step, d, errorCode(err));
      return;
    }
    const now = this.#ready();
    if (!now) return;
    if (change.lineId !== undefined) this.#movedFrom.set(dev.peerId, d.lineId);
    this.#devicesVersion++;
    this.#set({
      ...now,
      devices: this.#followMoves(now.devices.map((x) => (x.peerId === dev.peerId ? dev : x))),
      step: { kind: "list" },
    });
  }

  async #saveFailed(step: Extract<Step, { kind: "edit" }>, d: Device, code: ErrorCode): Promise<void> {
    if (this.#lost(code)) return;
    if (code === "device_gone") {
      // Removed elsewhere: there is nothing left to change.
      const v = this.#ready();
      if (!v) return;
      this.#devicesVersion++;
      this.#set({ ...v, devices: v.devices.filter((x) => x.peerId !== step.peerId), step: { kind: "list" } });
      return;
    }
    let lines: Line[] | undefined;
    let lineId = step.lineId;
    if (code === "line_unavailable") {
      // The picked line stopped sharing: show the lines there are now,
      // with the device's own line picked again.
      try {
        lines = await this.api.lines();
      } catch {
        // The old list stays; the error already says what happened.
      }
      lineId = d.lineId;
    }
    const v = this.#ready();
    if (!v) return;
    this.#set({ ...v, lines: lines ?? v.lines, step: { ...step, lineId, saving: false, error: code } });
  }

  askRemove(peerId: string): void {
    const v = this.#ready();
    if (!v || v.removing?.busy) return;
    this.#set({ ...v, removing: { peerId, busy: false } });
  }

  cancelRemove(): void {
    const v = this.#ready();
    if (!v || !v.removing || v.removing.busy) return;
    this.#set({ ...v, removing: undefined });
  }

  /** confirmRemove removes the device the member asked to remove. */
  async confirmRemove(): Promise<void> {
    const v = this.#ready();
    if (!v || !v.removing || v.removing.busy) return;
    const { peerId } = v.removing;
    this.#set({ ...v, removing: { peerId, busy: true } });
    try {
      await this.api.removeDevice(peerId);
    } catch (err) {
      const code = errorCode(err);
      const now = this.#ready();
      if (!this.#lost(code) && now) this.#set({ ...now, removing: { peerId, busy: false, error: code } });
      return;
    }
    const now = this.#ready();
    if (!now) return;
    this.#devicesVersion++;
    this.#set({ ...now, devices: now.devices.filter((d) => d.peerId !== peerId), removing: undefined });
  }

  /** dispose stops polling, and forgets a config still on screen. */
  dispose(): void {
    const v = this.#view;
    if (v.kind === "ready" && v.step.kind === "config") void this.#forget(v.step.device.device.peerId);
    this.#disposed = true;
    this.#stop();
  }

  #ready(): Ready | undefined {
    return !this.#disposed && this.#view.kind === "ready" ? this.#view : undefined;
  }

  #stop(): void {
    this.#stopPolling?.();
    this.#stopPolling = undefined;
  }

  #set(v: UseView): void {
    if (this.#disposed) return;
    this.#view = v;
    for (const fn of this.#listeners) fn(v);
  }
}
