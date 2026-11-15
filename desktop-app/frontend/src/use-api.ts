// Connects the Use state machine to the Go UseService through the generated
// Wails bindings. The session token and the new device's private key stay
// in Go; the webview gets the config only as a QR code.

import { UseService } from "../bindings/github.com/jermainee/homeport/desktop-app/services";
import type { Device as GoDevice } from "../bindings/github.com/jermainee/homeport/desktop-app/services/models";
import { asFailover, type Device, type UseApi } from "./use";

// Go sends the failover as a plain string.
const device = (d: GoDevice): Device => ({ ...d, failover: asFailover(d.failover) });

export const wailsUse: UseApi = {
  balance: () => UseService.Balance(),
  buyCredits: () => UseService.BuyCredits(),
  pollCheckout: () => UseService.PollCheckout(),
  countries: async (exceptPeer) => (await UseService.Countries(exceptPeer ?? "")) ?? [],
  cities: async (country, exceptPeer) => (await UseService.Cities(country, exceptPeer ?? "")) ?? [],
  devices: async () => ((await UseService.Devices()) ?? []).map(device),
  addDevice: async (country, city, name, httpsOnly) => {
    const dev = await UseService.AddDevice(country, city, name, httpsOnly);
    return { ...dev, device: device(dev.device) };
  },
  saveConfig: (peerId) => UseService.SaveConfig(peerId),
  forgetConfig: (peerId) => UseService.ForgetConfig(peerId),
  removeDevice: (peerId) => UseService.RemoveDevice(peerId),
  // An empty argument, or a null HTTPS only, keeps what the device has.
  updateDevice: async (peerId, change) =>
    device(
      await UseService.UpdateDevice(peerId, change.country ?? "", change.city ?? "", change.failover ?? "", change.httpsOnly ?? null),
    ),
  setHttpsOnly: async (peerId, on) => device(await UseService.SetHTTPSOnly(peerId, on)),
};
