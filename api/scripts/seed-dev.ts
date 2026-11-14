/**
 * `npm run seed:dev`: adds a gateway credential to the local D1 database
 * (`wrangler dev`) and prints its token, for the manual end-to-end run.
 * The gateway announces itself with PUT /v1/gateway/self when it starts.
 *
 * `npm run seed:dev -- --persist-to <dir>` seeds the local database kept in
 * <dir> instead of .wrangler/state, for a `wrangler dev --persist-to <dir>`
 * (the full-stack smoke test in e2e/ starts every run from an empty one).
 *
 * Local only: it never touches a remote database.
 */
import { execFileSync } from "node:child_process";
import { createHash, randomBytes, randomUUID } from "node:crypto";
import { parseArgs } from "node:util";

const { values } = parseArgs({ options: { "persist-to": { type: "string" } } });
const persistTo = values["persist-to"] ? ["--persist-to", values["persist-to"]] : [];

const wrangler = (...args: string[]) => {
  execFileSync("npx", ["wrangler", ...args, "--local", ...persistTo], { stdio: ["ignore", "ignore", "inherit"] });
};

wrangler("d1", "migrations", "apply", "homeport");

const gatewayId = randomUUID();
const secret = randomBytes(32).toString("base64url");
const secretSha256 = createHash("sha256").update(secret).digest("hex");
// Both values are generated here (a UUID and hex), so the SQL needs no escaping.
wrangler(
  "d1",
  "execute",
  "homeport",
  "--command",
  `INSERT INTO gateway_credentials (gateway_id, secret_sha256, created_at) VALUES ('${gatewayId}', '${secretSha256}', ${Date.now()})`,
);

console.log(`Dev gateway ${gatewayId} added to the local database. Start the gateway with:

HOMEPORT_GATEWAY_TOKEN=hpgw_${gatewayId}.${secret}
`);
