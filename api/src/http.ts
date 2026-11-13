import type { Context } from "hono";
import type { ContentfulStatusCode } from "hono/utils/http-status";

/** The JSON object in a request body, or null if the body is not a JSON object. */
export async function readJsonObject(request: Request): Promise<Record<string, unknown> | null> {
  try {
    const value: unknown = await request.json();
    return typeof value === "object" && value !== null && !Array.isArray(value)
      ? (value as Record<string, unknown>)
      : null;
  } catch {
    return null;
  }
}

/** True when `body` has no field outside `allowed`; the contract's schemas are closed. */
export function hasOnlyFields(body: Record<string, unknown>, allowed: readonly string[]): boolean {
  return Object.keys(body).every((key) => allowed.includes(key));
}

/** An error body as the contract defines it: `{error, message?}`. */
export function apiError(
  c: Context,
  status: ContentfulStatusCode,
  error: string,
  message?: string,
): Response {
  return c.json(message === undefined ? { error } : { error, message }, status);
}

export function invalidRequest(c: Context, message: string): Response {
  return apiError(c, 400, "invalid_request", message);
}
