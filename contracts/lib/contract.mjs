// Loads control-api.openapi.yaml and validates JSON bodies against it with
// ajv (JSON Schema 2020-12, the dialect of OpenAPI 3.1).
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { parse } from "yaml";
import Ajv2020 from "ajv/dist/2020.js";
import addFormats from "ajv-formats";

export const CONTRACTS_DIR = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
export const SPEC_PATH = path.join(CONTRACTS_DIR, "control-api.openapi.yaml");

const SPEC_ID = "https://contracts.homeport.invalid/control-api.openapi.yaml";
const JSON_MEDIA_TYPE = "application/json";
const HTTP_METHODS = ["get", "put", "post", "patch", "delete"];
// Top-level OpenAPI fields. ajv ignores them, so the whole document can be
// loaded as one schema and every $ref in it resolves as written.
const OPENAPI_ROOT_FIELDS = [
  "openapi",
  "info",
  "jsonSchemaDialect",
  "servers",
  "paths",
  "webhooks",
  "components",
  "security",
  "tags",
  "externalDocs",
];

export async function loadContract(specPath = SPEC_PATH) {
  const spec = parse(readFileSync(specPath, "utf8"));
  const specDir = path.dirname(specPath);

  // strictRequired would reject `then: {required: [...]}`. Every schema is
  // closed and every example must validate, so a misspelled `required` entry
  // still fails the tests.
  const ajv = new Ajv2020({ strict: true, strictRequired: false, allErrors: true });
  addFormats(ajv);
  ajv.addVocabulary(OPENAPI_ROOT_FIELDS);
  ajv.addSchema({ ...spec, $id: SPEC_ID });

  const validators = new Map();
  function validatorFor(target) {
    const parts = mediaSchemaParts(spec, target);
    const ref = SPEC_ID + toPointer(parts);
    if (!validators.has(ref)) validators.set(ref, ajv.compile({ $ref: ref }));
    return validators.get(ref);
  }

  function validate(target, body) {
    const check = validatorFor(target);
    return { errors: check(body) ? [] : check.errors.map(formatError) };
  }

  return {
    spec,

    /** Validates a request body for `method path`. */
    validateRequest(method, apiPath, body) {
      return validate({ method, path: apiPath, part: "request" }, body);
    },

    /** Validates a response body for `method path` and `status`. */
    validateResponse(method, apiPath, status, body) {
      return validate({ method, path: apiPath, part: "response", status: String(status) }, body);
    },

    /** Every JSON example the spec references with `externalValue`. */
    examples() {
      return listExamples(spec, specDir);
    },

    /** Reads an example file and validates it against its operation. */
    validateExample(example) {
      const body = JSON.parse(readFileSync(example.file, "utf8"));
      return validate(example, body);
    },
  };
}

function listExamples(spec, specDir) {
  const found = [];
  for (const [apiPath, pathItem] of Object.entries(spec.paths ?? {})) {
    for (const method of HTTP_METHODS) {
      const operation = pathItem[method];
      if (!operation) continue;
      const bodies = [];
      if (operation.requestBody) {
        bodies.push({ part: "request", node: resolve(spec, operation.requestBody).node });
      }
      for (const [status, response] of Object.entries(operation.responses ?? {})) {
        bodies.push({ part: "response", status, node: resolve(spec, response).node });
      }
      for (const { part, status, node } of bodies) {
        const examples = node.content?.[JSON_MEDIA_TYPE]?.examples ?? {};
        for (const [name, exampleOrRef] of Object.entries(examples)) {
          const example = resolve(spec, exampleOrRef).node;
          if (!example.externalValue) continue;
          found.push({
            name,
            method,
            path: apiPath,
            part,
            ...(status ? { status } : {}),
            file: path.resolve(specDir, example.externalValue),
          });
        }
      }
    }
  }
  return found;
}

function mediaSchemaParts(spec, { method, path: apiPath, part, status }) {
  const operationParts = ["paths", apiPath, method.toLowerCase()];
  const operation = get(spec, operationParts);
  if (!operation) throw new Error(`No operation ${method.toUpperCase()} ${apiPath} in the spec`);

  const bodyParts =
    part === "request"
      ? [...operationParts, "requestBody"]
      : [...operationParts, "responses", String(status)];
  const body = resolve(spec, get(spec, bodyParts), bodyParts);
  if (!body.node) {
    const what = part === "request" ? "request body" : `${status} response`;
    throw new Error(`${method.toUpperCase()} ${apiPath} has no ${what}`);
  }
  const schemaParts = [...body.parts, "content", JSON_MEDIA_TYPE, "schema"];
  if (!get(spec, schemaParts)) {
    throw new Error(`${method.toUpperCase()} ${apiPath} ${part} ${status ?? ""} has no JSON schema`);
  }
  return schemaParts;
}

// Follows local OpenAPI $refs (#/components/...) to the object they name.
function resolve(spec, node, parts = []) {
  const seen = new Set();
  while (node && typeof node.$ref === "string") {
    if (!node.$ref.startsWith("#/")) throw new Error(`Only local $refs are supported: ${node.$ref}`);
    if (seen.has(node.$ref)) throw new Error(`Circular $ref: ${node.$ref}`);
    seen.add(node.$ref);
    parts = node.$ref
      .slice(2)
      .split("/")
      .map((part) => part.replaceAll("~1", "/").replaceAll("~0", "~"));
    node = get(spec, parts);
  }
  return { node, parts };
}

function get(root, parts) {
  return parts.reduce((node, part) => (node == null ? undefined : node[part]), root);
}

function toPointer(parts) {
  return "#" + parts.map((part) => "/" + part.replaceAll("~", "~0").replaceAll("/", "~1")).join("");
}

function formatError(error) {
  const where = error.instancePath || "/";
  if (error.keyword === "additionalProperties") {
    return `${where} must not have property "${error.params.additionalProperty}"`;
  }
  return `${where} ${error.message}`;
}

/** JSON files under examples/ that no `externalValue` in the spec points to. */
export function unreferencedExampleFiles(contract, examplesDir = path.join(CONTRACTS_DIR, "examples")) {
  const referenced = new Set(contract.examples().map((example) => example.file));
  return readdirSync(examplesDir, { recursive: true })
    .filter((file) => file.endsWith(".json"))
    .map((file) => path.join(examplesDir, file))
    .filter((file) => !referenced.has(file))
    .sort();
}
