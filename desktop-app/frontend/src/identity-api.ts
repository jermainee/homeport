// Connects the identity state machine to the Go IdentityService through the
// generated Wails bindings.

import { IdentityService } from "../bindings/github.com/jermainee/homeport/desktop-app/services";
import { IDENTITY_STATES, type IdentityApi, type IdentityState } from "./identity";

// fromGo checks a state from Go and gives it the frontend's type.
function fromGo(state: string): IdentityState {
  if (!(IDENTITY_STATES as readonly string[]).includes(state)) {
    throw Object.assign(new Error(`unknown identity state "${state}"`), { cause: { code: "unexpected" } });
  }
  return state as IdentityState;
}

export const wailsIdentity: IdentityApi = {
  start: async () => fromGo((await IdentityService.Start()).state),
  poll: async () => fromGo((await IdentityService.Poll()).state),
};
