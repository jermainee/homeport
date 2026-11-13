# contracts

The contract between the Homeport control API (`api/`), the edge gateway (`gateway/`) and the apps. Each side builds against this folder, so the API and the gateway can be built in parallel.

| Path | What it is |
| --- | --- |
| `control-api.openapi.yaml` | OpenAPI 3.1 spec of every endpoint: gateway endpoints first, then member endpoints |
| `examples/gateway/*.json` | Request and response bodies of the gateway endpoints |
| `examples/member/*.json` | Request and response bodies of the member endpoints |
| `lib/contract.mjs` | Loads the spec and validates JSON bodies against it with ajv |
| `scripts/validate-examples.mjs` | Validates every example against its operation and fails on unreferenced example files |
| `test/` | Tests for the spec's rules |

The spec links each example to its operation with `externalValue`. The example files are plain JSON, so other folders can use them directly as test fixtures.

## Rules the spec enforces

- **Two token scopes:**
  - Gateways send `Authorization: Bearer hpgw_<gatewayId>.<secret>`, which works only on `/v1/gateway/*`.
  - Members send a session token that the app creates and a sign-in activates. It works everywhere else.
- **Closed schemas:**
  - Every object schema sets `additionalProperties: false`, and the API rejects unknown request fields with `400 invalid_request`.
  - A usage event therefore cannot carry a destination or any content.
- **Idempotent POSTs:**
  - Every POST body carries a `request_id` (UUIDv7) that the client reuses on retries.
  - The batch endpoints carry an `event_id` (UUIDv7) on every event instead, and answer each event as `applied`, `duplicate` or `rejected`.
  - A retried request gets the first response again, so no response carries a secret. The app creates the session token itself and sends only its SHA-256 when it starts a sign-in.
- **Lowercase UUIDs** everywhere.
- **Body size limits:** each request body states its limit in `x-max-body-bytes`. The limit is 16 KB, or 128 KB for the gateway's batch endpoints, which fits 100 events at their largest. A larger body gets `413 payload_too_large`.
- **Bytes, not prices:**
  - Leases and usage are counted in bytes, and the gateway never sees a price.
  - Credits and euros stay in the API's pricing module.

The spec's `info.description` has the details: the `request_id` and `event_id` windows, error codes and units.

## Commands

Use Node 26 (`.nvmrc`).

```bash
npm ci
```

```bash
npm run lint
```

```bash
npm run typecheck
```

```bash
npm test
```

```bash
npm run validate
```

- `lint` runs Redocly with `recommended-strict`, so any warning fails.
- `typecheck` runs TypeScript over the JavaScript (`checkJs`, strict), using the JSDoc types in `lib/contract.mjs`.
- `test` runs the tests in `test/`, including `validate-examples.mjs`.
- `validate` prints the result for each example.

CI runs `npm ci`, `lint`, `typecheck` and `test` on every pull request that touches this folder (`.github/workflows/contracts-ci.yml`).

## Changing the contract

1. Change `control-api.openapi.yaml` and the examples together. Every example file must be referenced from the spec.
2. Bump `version` in `package.json` and `info.version` in the spec to the same value; a test checks that they match. Before 1.0, a breaking change bumps the minor version.
3. When a change breaks a consumer, update `api/` and `gateway/` in follow-up PRs that point to this one.

## Using it from another folder

- **Go (`gateway/`):** read `../contracts/examples/gateway/*.json` as test fixtures. Add `contracts/**` to the folder's CI path filter.
- **TypeScript (`api/`):**
  1. Add `"@homeport/contracts": "file:../contracts"` to `devDependencies`.
  2. Add `install-links=true` to the folder's `.npmrc`. npm then installs a copy of `contracts/` together with its dependencies (ajv, yaml); a plain link would leave them out. Run `npm install` again after the contract changes.
  3. Validate bodies in tests:

     ```js
     import { loadContract } from "@homeport/contracts/lib/contract.mjs";

     const contract = await loadContract();
     const { errors } = contract.validateResponse("POST", "/v1/gateway/leases", 201, body);
     ```

  4. Add `contracts/**` to the folder's CI path filter.

  ajv compiles schemas with `new Function`, which workerd forbids. Run the validation in Node, for example in `vitest.config.ts` behind a Miniflare service binding, not inside the Worker isolate.
