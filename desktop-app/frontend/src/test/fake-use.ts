// A stand-in for the Go UseService behind the Wails bindings, for tests.

import type { Balance, CheckoutCheck, Device, Line, NewDevice, UseApi } from "../use";

export const berlin: Line = { lineId: "line-berlin", country: "DE", city: "Berlin", isp: "", ipv6: true, online: true };
export const lyon: Line = { lineId: "line-lyon", country: "FR", city: "Lyon", isp: "Orange", ipv6: false, online: false };
export const porto: Line = { lineId: "line-porto", country: "PT", city: "", isp: "", ipv6: false, online: true };

export const balance: Balance = { purchased: 25_000, earned: 5_000, total: 30_000, bytes: 3_000_000_000 };

export const laptop: Device = {
  peerId: "peer-laptop",
  name: "Laptop",
  lineId: "line-berlin",
  ipv4: "10.77.0.2",
  ipv6: "fd77::2",
  createdAt: "2026-11-12T09:00:00Z",
};

export function newDevice(lineId: string, name: string, n = 3): NewDevice {
  return {
    device: { peerId: `peer-${n}`, name, lineId, ipv4: `10.77.0.${n}`, ipv6: `fd77::${n}`, createdAt: "2026-11-14T09:00:00Z" },
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
  linesAnswer: Answer<Line[]> = [berlin, lyon];
  devicesAnswer: Answer<Device[]> = [laptop];
  buyAnswer: Answer<Balance> | null = null;
  /** What the next checkout polls answer, in order; then not arrived. */
  checkoutPolls: Array<Answer<CheckoutCheck>> = [];
  addAnswer: Answer<NewDevice> | null = null;
  saveAnswer: Answer<boolean> = true;
  removeAnswer: Error | null = null;

  calls: string[] = [];
  forgotten: string[] = [];
  removed: string[] = [];
  #hold: Promise<void> | null = null;
  #release: (() => void) | null = null;

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
  async lines() {
    this.calls.push("lines");
    return settle(this.linesAnswer);
  }
  async devices() {
    this.calls.push("devices");
    return settle(this.devicesAnswer);
  }
  async addDevice(lineId: string, name: string) {
    this.calls.push(`addDevice ${lineId} ${name}`);
    await this.#hold;
    const a = this.addAnswer ?? newDevice(lineId, name);
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

  /** hold keeps the next calls that change something waiting until release. */
  hold() {
    this.#hold = new Promise((r) => (this.#release = r));
  }
  release() {
    this.#release?.();
    this.#hold = null;
  }
}
