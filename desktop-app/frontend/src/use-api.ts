// Connects the Use state machine to the Go UseService through the generated
// Wails bindings. The session token and the new device's private key stay
// in Go; the webview gets the config only as a QR code.

import { UseService } from "../bindings/github.com/jermainee/homeport/desktop-app/services";
import type { UseApi } from "./use";

export const wailsUse: UseApi = {
  balance: () => UseService.Balance(),
  buyCredits: () => UseService.BuyCredits(),
  pollCheckout: () => UseService.PollCheckout(),
  lines: async () => (await UseService.Lines()) ?? [],
  devices: async () => (await UseService.Devices()) ?? [],
  addDevice: (lineId, name) => UseService.AddDevice(lineId, name),
  saveConfig: (peerId) => UseService.SaveConfig(peerId),
  forgetConfig: (peerId) => UseService.ForgetConfig(peerId),
  removeDevice: (peerId) => UseService.RemoveDevice(peerId),
};
