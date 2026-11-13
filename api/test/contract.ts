import { env } from "cloudflare:workers";
import { expect } from "vitest";

/**
 * Errors from validating a response body against
 * contracts/control-api.openapi.yaml. The check runs in Node (see
 * vitest.config.ts) because ajv cannot compile schemas inside workerd.
 */
export async function contractErrors(
  method: string,
  path: string,
  status: number,
  body: unknown,
): Promise<string[]> {
  const response = await env.CONTRACT.fetch("https://contract.test/validate", {
    method: "POST",
    body: JSON.stringify({ method, path, status, body }),
  });
  const { errors } = await response.json<{ errors: string[] }>();
  return errors;
}

/**
 * Reads a JSON response, checks it against the contract's schema for
 * `method path` (a path template as the spec writes it) and its status, and
 * returns the body.
 */
export async function expectContract<T = Record<string, unknown>>(
  method: string,
  path: string,
  response: Response,
): Promise<T> {
  const body = await response.json<T>();
  expect(await contractErrors(method, path, response.status, body)).toEqual([]);
  return body;
}
