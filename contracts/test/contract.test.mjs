import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import path from "node:path";
import { CONTRACTS_DIR, loadContract, unreferencedExampleFiles } from "../lib/contract.mjs";

const contract = await loadContract();

test("every example the spec references is valid for its operation", () => {
  const examples = contract.examples();
  assert.ok(examples.length > 0, "the spec references no examples");
  for (const example of examples) {
    const result = contract.validateExample(example);
    assert.deepEqual(result.errors, [], `${example.file} is invalid`);
  }
});

function readExample(name) {
  const example = contract.examples().find((candidate) => candidate.file.endsWith(`/${name}`));
  assert.ok(example, `the spec does not reference ${name}`);
  return { example, body: JSON.parse(readFileSync(example.file, "utf8")) };
}

test("a usage event that carries a destination is rejected", () => {
  const { body } = readExample("usage-batch.json");
  body.events[0].destination = "93.184.215.14:443";

  const { errors } = contract.validateRequest("POST", "/v1/gateway/usage", body);

  assert.ok(
    errors.includes('/events/0 must not have property "destination"'),
    `expected a rejection of "destination", got ${JSON.stringify(errors)}`,
  );
});

// Paths ("/events/0/session_start") of every object inside a JSON value.
function objectPaths(value, at = "") {
  if (Array.isArray(value)) return value.flatMap((item, index) => objectPaths(item, `${at}/${index}`));
  if (value === null || typeof value !== "object") return [];
  return [at, ...Object.entries(value).flatMap(([key, child]) => objectPaths(child, `${at}/${key}`))];
}

function objectAt(root, pointer) {
  return pointer.split("/").slice(1).reduce((node, key) => node[key], root);
}

test("every request rejects an unknown field at any depth", () => {
  const requests = contract.examples().filter((example) => example.part === "request");
  assert.ok(requests.length > 0);
  for (const example of requests) {
    const original = readFileSync(example.file, "utf8");
    for (const at of objectPaths(JSON.parse(original))) {
      const body = JSON.parse(original);
      objectAt(body, at).unexpected_field = true;

      const { errors } = contract.validateRequest(example.method, example.path, body);

      assert.ok(
        errors.includes(`${at || "/"} must not have property "unexpected_field"`),
        `${example.file}: an unknown field at "${at || "/"}" was accepted`,
      );
    }
  }
});

test("a usage batch holds at most 100 events", () => {
  const { body } = readExample("usage-batch.json");
  const event = body.events[1];
  body.events = Array.from({ length: 101 }, () => ({ ...event }));

  const { errors } = contract.validateRequest("POST", "/v1/gateway/usage", body);

  assert.ok(errors.includes("/events must NOT have more than 100 items"), JSON.stringify(errors));
});

test("only the first event of a session (seq 0) carries session_start", () => {
  const { body } = readExample("usage-batch.json");
  const [first, second] = body.events;
  const withoutStart = { ...first };
  delete withoutStart.session_start;
  const lateStart = { ...second, session_start: first.session_start };

  for (const event of [withoutStart, lateStart]) {
    const { errors } = contract.validateRequest("POST", "/v1/gateway/usage", { events: [event] });
    assert.notDeepEqual(errors, [], `seq ${event.seq} with session_start=${!!event.session_start} was accepted`);
  }
});

test("a rejected event result says why, and only a rejected one does", () => {
  const event_id = "01a1d422-9a80-71fd-82cf-7d6096776eeb";
  const batches = [
    { path: "/v1/gateway/usage", reason: "unknown_lease" },
    { path: "/v1/gateway/line-events", reason: "unknown_line" },
  ];
  for (const { path: apiPath, reason } of batches) {
    const validate = (result) => contract.validateResponse("POST", apiPath, 200, { results: [result] }).errors;

    assert.deepEqual(validate({ event_id, status: "rejected", reason }), [], apiPath);
    assert.notDeepEqual(validate({ event_id, status: "rejected" }), [], `${apiPath}: rejected without reason`);
    assert.notDeepEqual(validate({ event_id, status: "applied", reason }), [], `${apiPath}: applied with reason`);
  }
});

function operations() {
  return Object.entries(contract.spec.paths).flatMap(([path, item]) =>
    ["get", "put", "post", "patch", "delete"]
      .filter((method) => item[method])
      .map((method) => ({ method, path, operation: item[method] })),
  );
}

test("every POST is idempotent by request_id or by event_id", () => {
  for (const { method, path } of operations().filter((op) => op.method === "post")) {
    const example = contract
      .examples()
      .find((candidate) => candidate.method === method && candidate.path === path && candidate.part === "request");
    assert.ok(example, `POST ${path} has no request example`);

    const body = JSON.parse(readFileSync(example.file, "utf8"));
    if (Array.isArray(body.events)) {
      for (const event of body.events) delete event.event_id;
    } else {
      assert.ok("request_id" in body, `${example.file} has no request_id`);
      delete body.request_id;
    }

    const { errors } = contract.validateRequest(method, path, body);
    assert.notDeepEqual(errors, [], `POST ${path} accepts a body without its idempotency key`);
  }
});

const PUBLIC_OPERATIONS = new Set(["createLoginRequest", "pollLoginRequest"]);

test("gateway tokens work only on /v1/gateway/*, member sessions everywhere else", () => {
  const ops = operations();
  assert.ok(ops.some((op) => !op.path.startsWith("/v1/gateway/")), "the spec has no member endpoints");
  for (const { method, path, operation } of ops) {
    const schemes = (operation.security ?? contract.spec.security ?? []).flatMap(Object.keys);
    const expected = path.startsWith("/v1/gateway/")
      ? ["gatewayToken"]
      : PUBLIC_OPERATIONS.has(operation.operationId)
        ? []
        : ["memberSession"];
    assert.deepEqual(schemes, expected, `${method.toUpperCase()} ${path}`);
  }
});

test("every example file is referenced by the spec", () => {
  assert.deepEqual(unreferencedExampleFiles(contract), []);
});

test("validate-examples checks every example and exits 0", () => {
  const run = spawnSync(process.execPath, [path.join(CONTRACTS_DIR, "scripts/validate-examples.mjs")], {
    encoding: "utf8",
  });

  assert.equal(run.status, 0, run.stdout + run.stderr);
  assert.match(run.stdout, new RegExp(`${contract.examples().length} examples valid`));
});

test("the spec's version is the package version", () => {
  const { version } = JSON.parse(readFileSync(path.join(CONTRACTS_DIR, "package.json"), "utf8"));
  assert.equal(contract.spec.info.version, version);
});
