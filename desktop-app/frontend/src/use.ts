// The Use screen as a state machine: the member's credits, buying more,
// where they can get a line, and the devices that use one with the
// WireGuard app. It drives the Go UseService through the UseApi seam and knows
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

/** services.Region in Go: a country with free lines for the member. */
export interface Region {
  /** ISO 3166-1 alpha-2. */
  country: string;
  lines: number;
}

/** services.City in Go: a city with free lines for the member. */
export interface City {
  city: string;
  lines: number;
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
  /** The device's line, which the API picked in the region the member chose. */
  lineId: string;
  /** Where its line is, and who provides it; "" when unknown. ISO 3166-1 alpha-2. */
  country: string;
  city: string;
  isp: string;
  failover: Failover;
  /**
   * HTTPS only: the gateway lets through only the device's TCP that opens
   * with TLS. On for every device until the member turns it off.
   */
  httpsOnly: boolean;
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

/**
 * What UpdateDevice changes; what is left out stays. A country moves the
 * device to another free line there, in the city unless it is "".
 */
export interface DeviceChange {
  country?: string;
  city?: string;
  failover?: Failover;
  httpsOnly?: boolean;
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
  /** countries and cities leave out the line of exceptPeer, a device being moved. */
  countries(exceptPeer?: string): Promise<Region[]>;
  cities(country: string, exceptPeer?: string): Promise<City[]>;
  devices(): Promise<Device[]>;
  /** addDevice gets a line in country, and in city unless it is "". */
  addDevice(country: string, city: string, name: string, httpsOnly: boolean): Promise<NewDevice>;
  /** saveConfig resolves false when the member cancels the save dialog. */
  saveConfig(peerId: string): Promise<boolean>;
  forgetConfig(peerId: string): Promise<void>;
  removeDevice(peerId: string): Promise<void>;
  /** updateDevice moves a device to another line in a region, or changes its failover. */
  updateDevice(peerId: string, change: DeviceChange): Promise<Device>;
  /** setHttpsOnly turns HTTPS only on or off for one device. */
  setHttpsOnly(peerId: string, on: boolean): Promise<Device>;
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

/** Where a device's line should be: a country, then any city in it or one city. */
export interface RegionPick {
  /** The countries with free lines, by code; null while they load. */
  countries: Region[] | null;
  country: string | null;
  /** The picked country's cities, most lines first; null while they load or without a country. */
  cities: City[] | null;
  /** Why the cities didn't load; they can be loaded again. */
  citiesError?: ErrorCode;
  /** The picked city, or "" for any city in the country. */
  city: string;
  /** What the member typed to find a city. */
  query: string;
}

/** Adding a device: where its line should be, and its name. */
export interface AddStep extends RegionPick {
  kind: "add";
  name: string;
  /** HTTPS only for the new device: on unless the member turns it off. */
  httpsOnly: boolean;
  creating: boolean;
  error?: ErrorCode;
}

/**
 * Changing a device: whether it moves to another line, and where, and its
 * failover. It keeps its config.
 */
export interface EditStep extends RegionPick {
  kind: "edit";
  peerId: string;
  move: boolean;
  failover: Failover;
  httpsOnly: boolean;
  saving: boolean;
  error?: ErrorCode;
}

export type Step =
  | { kind: "list" }
  | AddStep
  // The new device's config, shown once.
  | { kind: "config"; device: NewDevice; saving: boolean; saved: boolean; error?: ErrorCode }
  | EditStep;

export interface Ready {
  kind: "ready";
  balance: Balance;
  devices: Device[];
  checkout: Checkout;
  step: Step;
  /** A device the member is removing: asked to confirm, or on its way. */
  removing?: { peerId: string; busy: boolean; error?: ErrorCode };
  /**
   * HTTPS only on its way to on or off, by device: the position asked for
   * while busy, else why the last try failed.
   */
  httpsOnly?: Readonly<Record<string, HttpsOnlyTurn>>;
}

export type HttpsOnlyTurn = { busy: true; on: boolean } | { busy: false; error: ErrorCode };

/**
 * httpsOnlyOf is what a device's HTTPS only switch shows: the position
 * asked for while it turns, else the device's own.
 */
export function httpsOnlyOf(v: Ready, d: Device): { on: boolean; busy: boolean; error?: ErrorCode } {
  const t = v.httpsOnly?.[d.peerId];
  if (t?.busy) return { on: t.on, busy: true };
  return t ? { on: d.httpsOnly, busy: false, error: t.error } : { on: d.httpsOnly, busy: false };
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

/** A step's region before the countries load. */
const noRegion: RegionPick = { countries: null, country: null, cities: null, city: "", query: "" };

/** The device a step moves, whose own line doesn't count as free. */
const moving = (step: AddStep | EditStep | undefined) => (step?.kind === "edit" ? step.peerId : undefined);

/** Whether a step is sending what the member picked, so the pick is fixed. */
const busy = (step: AddStep | EditStep) => (step.kind === "add" ? step.creating : step.saving);

// without returns turns without the one for peerId.
function without(turns: Ready["httpsOnly"], peerId: string): Ready["httpsOnly"] {
  const { [peerId]: _, ...rest } = turns ?? {};
  return rest;
}

// inFlight keeps the turns still on their way, for devices still listed.
// Once the devices load again, an error is old news: the switch shows
// where the device is.
function inFlight(turns: Ready["httpsOnly"], devices: Device[]): Ready["httpsOnly"] {
  if (!turns) return undefined;
  return Object.fromEntries(
    Object.entries(turns).filter(([peerId, t]) => t.busy && devices.some((d) => d.peerId === peerId)),
  );
}

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
  // Bumped by each load of the countries or cities, so only the newest
  // answer is shown.
  #regionsVersion = 0;
  #citiesVersion = 0;

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

  /** refresh reloads the balance and devices without a loading state. */
  async refresh(): Promise<void> {
    if (this.#view.kind !== "ready") return;
    const devicesVersion = this.#devicesVersion;
    const balanceVersion = this.#balanceVersion;
    let data: [Balance, Device[]];
    try {
      data = await Promise.all([this.api.balance(), this.api.devices()]);
    } catch (err) {
      this.#lost(errorCode(err));
      return;
    }
    const v = this.#ready();
    if (!v) return;
    const [balance, devices] = data;
    const now = devicesVersion === this.#devicesVersion ? this.#followMoves(devices) : v.devices;
    this.#set({
      ...v,
      balance: balanceVersion === this.#balanceVersion ? balance : v.balance,
      devices: now,
      httpsOnly: inFlight(v.httpsOnly, now),
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
    if (!v) return;
    const now = version === this.#devicesVersion ? this.#followMoves(devices) : v.devices;
    this.#set({ ...v, devices: now, httpsOnly: inFlight(v.httpsOnly, now) });
  }

  async #load(): Promise<void> {
    this.#set({ kind: "loading" });
    let data: [Balance, Device[]];
    try {
      data = await Promise.all([this.api.balance(), this.api.devices()]);
    } catch (err) {
      if (this.#disposed) return;
      const code = errorCode(err);
      if (!this.#lost(code)) this.#set({ kind: "failed", error: code });
      return;
    }
    if (this.#disposed) return;
    const [balance, devices] = data;
    this.#set({ kind: "ready", balance, devices, checkout: { phase: "idle" }, step: { kind: "list" } });
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

  /**
   * startAdding opens the form for a new device at once, then loads the
   * countries with free lines. It picks the country of the newest device,
   * or the only country there is, and loads that country's cities.
   */
  async startAdding(): Promise<void> {
    const v = this.#ready();
    if (!v || v.step.kind !== "list") return;
    this.#set({ ...v, step: { kind: "add", ...noRegion, name: "", httpsOnly: true, creating: false } });
    await this.#loadRegions(v.devices[0]?.country, true);
  }

  /** reloadRegions loads the countries again, after they failed to load. */
  async reloadRegions(): Promise<void> {
    const step = this.#picking();
    if (!step || busy(step)) return;
    this.#setStep({ ...step, countries: null, error: undefined });
    await this.#loadRegions(step.kind === "add" ? this.#ready()?.devices[0]?.country : undefined, true);
  }

  // #loadRegions loads the countries and keeps or makes a pick: the country
  // the step has by the time they arrive, else `prefer`, if it has free
  // lines; else, with `onlyOne`, the only country there is. It loads the
  // picked country's cities.
  async #loadRegions(prefer: string | undefined, onlyOne: boolean, error?: ErrorCode): Promise<void> {
    const version = ++this.#regionsVersion;
    let countries: Region[];
    try {
      countries = await this.api.countries(moving(this.#picking()));
    } catch (err) {
      const code = errorCode(err);
      const step = this.#picking();
      if (this.#lost(code) || !step || version !== this.#regionsVersion) return;
      this.#setStep({ ...step, countries: [], error: code });
      return;
    }
    const step = this.#picking();
    if (!step || version !== this.#regionsVersion) return;
    const has = (code: string | null | undefined) => !!code && countries.some((c) => c.country === code);
    const want = step.country ?? prefer;
    const country = has(want) ? want! : onlyOne && countries.length === 1 ? countries[0]!.country : null;
    const sameCountry = country === step.country;
    this.#setStep({
      ...step,
      countries,
      country,
      cities: sameCountry ? step.cities : null,
      city: sameCountry ? step.city : "",
      query: sameCountry ? step.query : "",
      error,
    });
    if (country !== null) await this.#loadCities(country, error);
  }

  // #loadCities loads country's cities, unless the member picked another
  // country meanwhile. A picked city that has no free line any more falls
  // back to any city.
  async #loadCities(country: string, error?: ErrorCode): Promise<void> {
    const version = ++this.#citiesVersion;
    let cities: City[];
    let failed: ErrorCode | undefined;
    try {
      cities = await this.api.cities(country, moving(this.#picking()));
    } catch (err) {
      failed = errorCode(err);
      if (this.#lost(failed)) return;
      cities = [];
    }
    const step = this.#picking();
    if (!step || step.country !== country || version !== this.#citiesVersion) return;
    const city = cities.some((c) => c.city === step.city) ? step.city : "";
    this.#setStep({ ...step, cities, city, citiesError: failed, error: error ?? step.error });
  }

  /** reloadCities loads the picked country's cities again, after they failed to load. */
  async reloadCities(): Promise<void> {
    const step = this.#picking();
    if (!step || busy(step) || step.country === null) return;
    this.#setStep({ ...step, cities: null, citiesError: undefined });
    await this.#loadCities(step.country);
  }

  /** pickCountry picks the country the device's line is in, and loads its cities. */
  async pickCountry(country: string): Promise<void> {
    const step = this.#picking();
    if (!step || busy(step) || step.country === country) return;
    this.#setStep({ ...step, country, cities: null, citiesError: undefined, city: "", query: "", error: undefined });
    await this.#loadCities(country);
  }

  /** pickCity picks a city of the country, or "" for any city in it. */
  pickCity(city: string): void {
    const step = this.#picking();
    if (!step || busy(step)) return;
    this.#setStep({ ...step, city, error: undefined });
  }

  /** setQuery keeps what the member types to find a city, so the list follows it. */
  setQuery(query: string): void {
    const step = this.#picking();
    if (!step) return;
    this.#setStep({ ...step, query });
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
    const step = this.#picking();
    if (step?.kind !== "add" || step.creating) return;
    const name = step.name.trim();
    if (name.length === 0 || [...name].length > MAX_DEVICE_NAME) {
      this.#setStep({ ...step, error: "invalid_name" });
      return;
    }
    if (step.country === null) {
      this.#setStep({ ...step, error: "no_country" });
      return;
    }
    this.#setStep({ ...step, creating: true, error: undefined });
    let dev: NewDevice;
    try {
      dev = await this.api.addDevice(step.country, step.city, name, step.httpsOnly);
    } catch (err) {
      await this.#createFailed(errorCode(err));
      return;
    }
    const now = this.#ready();
    if (!now) {
      // Use closed meanwhile; nobody will see the config.
      void this.#forget(dev.device.peerId);
      return;
    }
    this.#devicesVersion++;
    this.#set({
      ...now,
      devices: [dev.device, ...now.devices.filter((d) => d.peerId !== dev.device.peerId)],
      step: { kind: "config", device: dev, saving: false, saved: false },
    });
  }

  async #createFailed(code: ErrorCode): Promise<void> {
    if (this.#lost(code)) return;
    const step = this.#picking();
    if (step?.kind !== "add") return;
    this.#setStep({ ...step, creating: false, error: code });
    // The region filled up meanwhile: show where lines are free now.
    if (code === "no_free_line") await this.#loadRegions(undefined, false, code);
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

  /**
   * startEditing opens a device's failover, and the choice to move it to
   * another line, to change them. A move's region starts at the device's
   * country and city.
   */
  startEditing(peerId: string): void {
    const v = this.#ready();
    if (!v || v.step.kind !== "list") return;
    const d = v.devices.find((d) => d.peerId === peerId);
    if (!d) return;
    this.#set({
      ...v,
      removing: undefined,
      step: {
        kind: "edit",
        peerId,
        move: false,
        ...noRegion,
        country: d.country || null,
        city: d.city,
        failover: d.failover,
        // A turn of the row's switch still on its way counts as done.
        httpsOnly: httpsOnlyOf(v, d).on,
        saving: false,
      },
    });
  }

  /**
   * setMove says whether the device moves to another line. Moving loads the
   * countries with free lines, the first time.
   */
  async setMove(move: boolean): Promise<void> {
    const step = this.#picking();
    if (step?.kind !== "edit" || step.saving || step.move === move) return;
    this.#setStep({ ...step, move, error: undefined });
    if (move && step.countries === null) await this.#loadRegions(undefined, true);
  }

  /**
   * pickHttpsOnly turns HTTPS only on or off for the new or changed device.
   * An error stays: none of them is about HTTPS only.
   */
  pickHttpsOnly(on: boolean): void {
    const step = this.#picking();
    if (!step || busy(step)) return;
    this.#setStep({ ...step, httpsOnly: on });
  }

  /** pickFailover picks where the device goes while its line is offline. */
  pickFailover(failover: Failover): void {
    const step = this.#picking();
    if (step?.kind !== "edit" || step.saving) return;
    this.#setStep({ ...step, failover, error: undefined });
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
    if (!d) {
      // Removed elsewhere while the form was open.
      this.#setStep({ ...step, error: "device_gone" });
      return;
    }
    const change: DeviceChange = {};
    if (step.move) {
      if (step.country === null) {
        this.#setStep({ ...step, error: "no_country" });
        return;
      }
      change.country = step.country;
      change.city = step.city;
    }
    if (step.failover !== d.failover) change.failover = step.failover;
    // Against a turn of the row's switch still on its way, as the form started.
    if (step.httpsOnly !== httpsOnlyOf(v, d).on) change.httpsOnly = step.httpsOnly;
    if (Object.keys(change).length === 0) {
      this.#set({ ...v, step: { kind: "list" } });
      return;
    }
    this.#setStep({ ...step, saving: true, error: undefined });
    let dev: Device;
    try {
      dev = await this.api.updateDevice(step.peerId, change);
    } catch (err) {
      await this.#saveFailed(step.peerId, errorCode(err));
      return;
    }
    const now = this.#ready();
    if (!now) return;
    if (dev.lineId !== d.lineId) this.#movedFrom.set(dev.peerId, d.lineId);
    this.#devicesVersion++;
    this.#set({
      ...now,
      devices: this.#followMoves(now.devices.map((x) => (x.peerId === dev.peerId ? dev : x))),
      step: { kind: "list" },
    });
  }

  async #saveFailed(peerId: string, code: ErrorCode): Promise<void> {
    if (this.#lost(code)) return;
    const v = this.#ready();
    if (v?.step.kind !== "edit") return;
    if (code === "device_gone") {
      // Removed elsewhere: there is nothing left to change.
      this.#devicesVersion++;
      this.#set({ ...v, devices: v.devices.filter((x) => x.peerId !== peerId), step: { kind: "list" } });
      return;
    }
    this.#set({ ...v, step: { ...v.step, saving: false, error: code } });
    // The region has no other free line: show where lines are free now.
    if (code === "no_free_line") await this.#loadRegions(undefined, false, code);
  }

  /** toggleHttpsOnly turns HTTPS only off for one device, or on again. */
  async toggleHttpsOnly(peerId: string): Promise<void> {
    const v = this.#ready();
    const d = v?.devices.find((d) => d.peerId === peerId);
    if (!v || !d || v.httpsOnly?.[peerId]?.busy) return;
    const on = !d.httpsOnly;
    this.#set({ ...v, httpsOnly: { ...v.httpsOnly, [peerId]: { busy: true, on } } });
    let dev: Device;
    try {
      dev = await this.api.setHttpsOnly(peerId, on);
    } catch (err) {
      const code = errorCode(err);
      const now = this.#ready();
      if (this.#lost(code) || !now) return;
      if (code === "device_gone") {
        // Removed elsewhere: there is nothing left to turn.
        this.#devicesVersion++;
        this.#set({
          ...now,
          devices: now.devices.filter((x) => x.peerId !== peerId),
          httpsOnly: without(now.httpsOnly, peerId),
        });
        return;
      }
      this.#set({ ...now, httpsOnly: { ...now.httpsOnly, [peerId]: { busy: false, error: code } } });
      return;
    }
    const now = this.#ready();
    if (!now) return;
    // Only HTTPS only comes from the answer: a move saved while it was on
    // its way is newer than the rest of it.
    this.#devicesVersion++;
    this.#set({
      ...now,
      devices: now.devices.map((x) => (x.peerId === peerId ? { ...x, httpsOnly: dev.httpsOnly } : x)),
      httpsOnly: without(now.httpsOnly, peerId),
    });
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

  // The step that picks a region: adding a device, or changing one.
  #picking(): AddStep | EditStep | undefined {
    const step = this.#ready()?.step;
    return step?.kind === "add" || step?.kind === "edit" ? step : undefined;
  }

  #setStep(step: AddStep | EditStep): void {
    const v = this.#ready();
    if (v) this.#set({ ...v, step });
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
