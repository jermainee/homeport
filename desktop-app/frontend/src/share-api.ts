// Connects the Share state machine to the Go ShareService through the
// generated Wails bindings, and to the status events it publishes.

import { Events } from "@wailsio/runtime";
import { ShareStatus as shareStatusEvent } from "../bindings/events";
import * as models from "../bindings/github.com/jermainee/homeport/desktop-app/services/models";
import { AuthService, ShareService } from "../bindings/github.com/jermainee/homeport/desktop-app/services";
import type { ShareApi, ShareState, ShareStatus } from "./share";
import { errorCode } from "./signin";

const states: readonly string[] = ["off", "connecting", "online", "paused", "offline", "error"] satisfies ShareState[];

// fromGo checks a status from Go and gives it the frontend's types.
function fromGo(s: models.ShareStatus): ShareStatus {
  if (!states.includes(s.state)) {
    throw Object.assign(new Error(`unknown share state "${s.state}"`), { cause: { code: "unexpected" } });
  }
  return {
    state: s.state as string as ShareState,
    sharing: s.sharing,
    error: s.error ? errorCode({ cause: { code: s.error } }) : undefined,
    bytesToday: s.bytesToday,
    flowsActive: s.flowsActive,
  };
}

export const wailsShare: ShareApi = {
  status: async () => fromGo(await ShareService.Status()),
  setSharing: async (on) => fromGo(await ShareService.SetSharing(on)),
  onStatus: (fn) =>
    Events.On(shareStatusEvent, (ev) => {
      try {
        fn(fromGo(ev.data));
      } catch {
        // An unknown state from a newer Go side; the next one may be known.
      }
    }),
  identity: async () => (await AuthService.Current())?.identityStatus ?? null,
};
