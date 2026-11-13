// Connects the sign-in state machine to the Go AuthService through the
// generated Wails bindings. The session token stays in Go.

import { AuthService } from "../bindings/github.com/jermainee/homeport/desktop-app/services";
import type { AuthApi, LoginStatus } from "./signin";

const statuses: readonly string[] = ["pending", "approved", "expired"] satisfies LoginStatus[];

export const wailsAuth: AuthApi = {
  current: () => AuthService.Current(),
  start: (email) => AuthService.Start(email),
  async poll() {
    const { status } = await AuthService.Poll();
    if (!statuses.includes(status)) {
      throw Object.assign(new Error(`unknown login status "${status}"`), { cause: { code: "unexpected" } });
    }
    return { status: status as string as LoginStatus };
  },
  cancel: () => AuthService.Cancel(),
  signOut: () => AuthService.SignOut(),
};
