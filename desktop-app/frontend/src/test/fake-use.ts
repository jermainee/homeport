// A stand-in for the Go UseService behind the Wails bindings, for tests.

import type { Balance, CheckoutCheck, City, Device, DeviceChange, NewDevice, Region, UseApi } from "../use";

export const germany: Region = { country: "DE", lines: 4 };
export const france: Region = { country: "FR", lines: 1 };
export const portugal: Region = { country: "PT", lines: 2 };

export const berlin: City = { city: "Berlin", lines: 2 };
export const munich: City = { city: "Munich", lines: 1 };
export const lyon: City = { city: "Lyon", lines: 1 };

export const balance: Balance = { purchased: 25_000, earned: 5_000, total: 30_000, bytes: 3_000_000_000 };

export const laptop: Device = {
  peerId: "peer-laptop",
  name: "Laptop",
  lineId: "line-berlin",
  country: "DE",
  city: "Berlin",
  isp: "Deutsche Telekom",
  failover: "country",
  httpsOnly: true,
  exitLine: null,
  ipv4: "10.77.0.2",
  ipv6: "fd77::2",
  createdAt: "2026-11-12T09:00:00Z",
};

/** The device the fake adds in country and city ("" for any city). */
export function newDevice(country: string, city: string, name: string, n = 3): NewDevice {
  return {
    device: {
      peerId: `peer-${n}`,
      name,
      lineId: `line-${country}-${city || "any"}`,
      country,
      city: city || "Somewhere",
      isp: "Vodafone",
      failover: "country",
      httpsOnly: true,
      exitLine: null,
      ipv4: `10.77.0.${n}`,
      ipv6: `fd77::${n}`,
      createdAt: "2026-11-14T09:00:00Z",
    },
    qrCode: "data:image/png;base64,iVBORw0KGgo=",
    fileName: `Homeport-${name}.conf`,
  };
}

type Answer<T> = T | Error;

const settle = <T>(a: Answer<T>): T => {
  if (a instanceof Error) throw a;
  return a;
};

export class FakeUse implements UseApi {
  balanceAnswer: Answer<Balance> = balance;
  countriesAnswer: Answer<Region[]> = [germany, france];
  citiesAnswers: Record<string, Answer<City[]>> = { DE: [berlin, munich], FR: [lyon] };
  devicesAnswer: Answer<Device[]> = [laptop];
  buyAnswer: Answer<Balance> | null = null;
  /** What the next checkout polls answer, in order; then not arrived. */
  checkoutPolls: Array<Answer<CheckoutCheck>> = [];
  addAnswer: Answer<NewDevice> | null = null;
  saveAnswer: Answer<boolean> = true;
  removeAnswer: Error | null = null;
  /** What the next updateDevice answers; by default the device, changed. */
  updateAnswer: Answer<Device> | null = null;
  /** What the next setHttpsOnly answers; by default the device, changed. */
  httpsOnlyAnswer: Answer<Device> | null = null;

  calls: string[] = [];
  updates: Array<[string, DeviceChange]> = [];
  httpsOnly: Array<[string, boolean]> = [];
  forgotten: string[] = [];
  removed: string[] = [];
  #hold: Promise<void> | null = null;
  #release: (() => void) | null = null;
  #holdRegions: Promise<void> | null = null;
  #releaseRegions: (() => void) | null = null;

  count(call: string): number {
    return this.calls.filter((c) => c === call).length;
  }

  async balance() {
    this.calls.push("balance");
    return settle(this.balanceAnswer);
  }
  async buyCredits() {
    this.calls.push("buyCredits");
    await this.#hold;
    return settle(this.buyAnswer ?? (this.balanceAnswer as Balance));
  }
  async pollCheckout() {
    this.calls.push("pollCheckout");
    return settle(this.checkoutPolls.shift() ?? { arrived: false, balance: this.balanceAnswer as Balance });
  }
  async countries(exceptPeer?: string) {
    this.calls.push(exceptPeer ? `countries ${exceptPeer}` : "countries");
    await this.#holdRegions;
    return settle(this.countriesAnswer);
  }
  async cities(country: string, exceptPeer?: string) {
    this.calls.push(exceptPeer ? `cities ${country} ${exceptPeer}` : `cities ${country}`);
    await this.#holdRegions;
    return settle(this.citiesAnswers[country] ?? []);
  }
  async devices() {
    this.calls.push("devices");
    return settle(this.devicesAnswer);
  }
  async addDevice(country: string, city: string, name: string) {
    this.calls.push(`addDevice ${country} ${city || "-"} ${name}`);
    await this.#hold;
    const a = this.addAnswer ?? newDevice(country, city, name);
    this.addAnswer = null;
    const dev = settle(a);
    if (Array.isArray(this.devicesAnswer)) this.devicesAnswer = [dev.device, ...this.devicesAnswer];
    return dev;
  }
  async saveConfig(peerId: string) {
    this.calls.push(`saveConfig ${peerId}`);
    await this.#hold;
    return settle(this.saveAnswer);
  }
  async forgetConfig(peerId: string) {
    this.forgotten.push(peerId);
  }
  async removeDevice(peerId: string) {
    this.calls.push(`removeDevice ${peerId}`);
    await this.#hold;
    if (this.removeAnswer) throw this.removeAnswer;
    this.removed.push(peerId);
    if (Array.isArray(this.devicesAnswer)) this.devicesAnswer = this.devicesAnswer.filter((d) => d.peerId !== peerId);
  }

  async updateDevice(peerId: string, change: DeviceChange) {
    this.calls.push(`updateDevice ${peerId}`);
    this.updates.push([peerId, change]);
    await this.#hold;
    const a = this.updateAnswer;
    this.updateAnswer = null;
    if (a) return settle(a);
    const devices = settle(this.devicesAnswer);
    const old = devices.find((d) => d.peerId === peerId);
    if (!old) throw new Error(`no device ${peerId}`);
    // A move lands on another line in the region, as the API picks it.
    const moved =
      change.country === undefined
        ? {}
        : { lineId: `line-${change.country}-${change.city || "any"}-2`, country: change.country, city: change.city || "Somewhere", isp: "M-net" };
    const dev: Device = { ...old, ...moved, ...(change.failover ? { failover: change.failover } : {}) };
    this.devicesAnswer = devices.map((d) => (d.peerId === peerId ? dev : d));
    return dev;
  }

  async setHttpsOnly(peerId: string, on: boolean) {
    this.calls.push(`setHttpsOnly ${peerId}`);
    this.httpsOnly.push([peerId, on]);
    await this.#hold;
    const a = this.httpsOnlyAnswer;
    this.httpsOnlyAnswer = null;
    if (a) return settle(a);
    const devices = settle(this.devicesAnswer);
    const old = devices.find((d) => d.peerId === peerId);
    if (!old) throw new Error(`no device ${peerId}`);
    const dev: Device = { ...old, httpsOnly: on };
    this.devicesAnswer = devices.map((d) => (d.peerId === peerId ? dev : d));
    return dev;
  }

  /** hold keeps the next calls that change something waiting until release. */
  hold() {
    this.#hold = new Promise((r) => (this.#release = r));
  }
  release() {
    this.#release?.();
    this.#hold = null;
  }

  /** holdRegions keeps the next countries and cities answers waiting until releaseRegions. */
  holdRegions() {
    this.#holdRegions = new Promise((r) => (this.#releaseRegions = r));
  }
  releaseRegions() {
    this.#releaseRegions?.();
    this.#holdRegions = null;
  }
}
