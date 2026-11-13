import type { D1Migration } from "cloudflare:test";

declare global {
  namespace Cloudflare {
    interface Env {
      TEST_MIGRATIONS: D1Migration[];
      /** Test-only: validates response bodies against the contract (vitest.config.ts). */
      CONTRACT: Fetcher;
      /** Test-only: an empty database for tests that apply migrations step by step. */
      MIGRATION_TEST_DB: D1Database;
    }
  }
}
