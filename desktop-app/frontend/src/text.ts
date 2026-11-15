// Text the screens share: error messages, escaping and number formats.

import type { ErrorCode } from "./signin";

export const errorText: Record<ErrorCode, string> = {
  invalid_email: "Enter a full email address, like name@example.com.",
  clock_skew: "Your computer's clock is more than an hour off. Set the date and time automatically, then try again.",
  offline: "Homeport can't be reached. Check your internet connection and try again.",
  rate_limited: "Too many sign-in attempts. Wait a minute, then try again.",
  email_unavailable: "Sign-in emails can't be sent right now. Try again in a few minutes.",
  no_sign_in: "That sign-in was cancelled. Enter your email to start again.",
  identity_required: "Verify your identity first.",
  gateway_unavailable: "No Homeport gateway can take your line yet. Try again in a few minutes.",
  signed_out: "You were signed out. Sign in again.",
  device_gone: "This device was removed. Add it again to use it.",
  config_gone: "This config is gone. Remove the device and add it again.",
  invalid_name: "Give the device a name of up to 64 characters.",
  no_country: "Pick the country its traffic should come from.",
  no_free_line: "No free line there right now. Pick another city, or any city in the country.",
  unexpected: "Sign-in didn't work. Try again.",
};

/**
 * useErrorText says what went wrong outside sign-in, where some of the
 * sign-in wording doesn't fit.
 */
export function useErrorText(code: ErrorCode): string {
  switch (code) {
    case "rate_limited":
      return "Homeport asked to slow down. Wait a minute, then try again.";
    case "unexpected":
      return "That didn't work. Try again.";
    default:
      return errorText[code];
  }
}

let regions: Intl.DisplayNames | undefined;

/** A country's name in the member's language, from its ISO code. */
export function countryName(code: string): string {
  try {
    regions ??= new Intl.DisplayNames(undefined, { type: "region" });
    return regions.of(code) ?? code;
  } catch {
    return code;
  }
}

const upTo1 = new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 });

/**
 * Says roughly how much data a byte count is, for what credits buy: whole
 * MB below 1 GB, then GB with at most one decimal.
 */
export function formatAbout(bytes: number): string {
  const gb = bytes / 1e9;
  if (gb < 1) return `${whole.format(bytes / 1e6)} MB`;
  if (gb < 10) return `${upTo1.format(gb)} GB`;
  return `${whole.format(gb)} GB`;
}

export const esc = (s: string) =>
  s.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);

export const decimal1 = new Intl.NumberFormat(undefined, { minimumFractionDigits: 1, maximumFractionDigits: 1 });
export const decimal2 = new Intl.NumberFormat(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 });
export const whole = new Intl.NumberFormat(undefined, { maximumFractionDigits: 0 });

/**
 * Formats a byte count the way Homeport bills it: 1 MB is 10^6 bytes, with
 * about three significant digits.
 */
export function formatBytes(bytes: number): string {
  const mb = bytes / 1e6;
  if (bytes === 0) return "0 MB";
  if (mb < 0.1) return `< ${decimal1.format(0.1)} MB`;
  if (mb < 100) return `${decimal1.format(mb)} MB`;
  if (mb < 1000) return `${whole.format(mb)} MB`;
  const gb = mb / 1000;
  return gb < 10 ? `${decimal2.format(gb)} GB` : `${decimal1.format(gb)} GB`;
}

