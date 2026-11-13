import path from "node:path";
import { cloudflareTest, readD1Migrations } from "@cloudflare/vitest-plugin";
import { loadContract } from "@homeport/contracts/lib/contract.mjs";
import { defineConfig } from "vitest/config";

// ajv compiles schemas with `new Function`, which workerd forbids, so the
// contract is checked here in Node and reached from tests through the
// CONTRACT service binding (see test/contract.ts).
const contract = await loadContract();

interface ContractCheck {
  method: string;
  path: string;
  status: number;
  body: unknown;
}

function checkResponse({ method, path: apiPath, status, body }: ContractCheck): string[] {
  try {
    return contract.validateResponse(method, apiPath, status, body).errors;
  } catch (error) {
    // An operation or status the spec does not define is a contract error too.
    return [error instanceof Error ? error.message : String(error)];
  }
}

export default defineConfig({
  plugins: [
    cloudflareTest(async () => {
      const migrations = await readD1Migrations(path.join(import.meta.dirname, "migrations"));
      return {
        wrangler: { configPath: "./wrangler.jsonc" },
        miniflare: {
          bindings: {
            // Tests run as local development unless a test overrides ENVIRONMENT.
            ENVIRONMENT: "dev",
            EMAIL_MODE: "log",
            PUBLIC_BASE_URL: "https://api.homeport.test",
            // Stripe is faked in tests (test/fake-stripe.ts); webhooks are
            // signed with this secret.
            STRIPE_SECRET_KEY: "sk_test_unused",
            STRIPE_IDENTITY_RESTRICTED_KEY: "rk_test_unused",
            STRIPE_WEBHOOK_SECRET: "whsec_test_homeport",
            IDENTITY_HASH_KEY: "test-identity-hash-key",
            // Test-only binding so the setup file can apply the migrations.
            TEST_MIGRATIONS: migrations,
          },
          serviceBindings: {
            // Test-only: validates a response body against contracts/control-api.openapi.yaml.
            CONTRACT: async (request) => {
              const errors = checkResponse((await request.json()) as ContractCheck);
              return Response.json({ errors });
            },
          },
        },
      };
    }),
  ],
  test: {
    setupFiles: ["./test/apply-migrations.ts"],
  },
});
