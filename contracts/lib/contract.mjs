// Loads control-api.openapi.yaml and validates JSON bodies against it with
// ajv (JSON Schema 2020-12, the dialect of OpenAPI 3.1).
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { parse } from "yaml";
import { Ajv2020 } from "ajv/dist/2020.js";
import ajvFormats from "ajv-formats";

/** @typedef {import("ajv").ErrorObject} ErrorObject */
/** @typedef {import("ajv").ValidateFunction} ValidateFunction */

/**
 * Which body of which operation to validate.
 * @typedef {object} Target
 * @property {string} method HTTP method, any case.
 * @property {string} path Path template as written in the spec, e.g. `/v1/gateway/leases/{lease_id}/return`.
 * @property {"request" | "response"} part
 * @property {string} [status] Response status, e.g. `"201"`. Only for responses.
 */

/**
 * An example file the spec references with `externalValue`.
 * @typedef {Target & { name: string, file: string }} Example
 */

/**
 * @typedef {object} ValidationResult
 * @property {string[]} errors Empty when the body is valid.
 */

/**
 * @typedef {object} Contract
 * @property {any} spec The parsed OpenAPI document.
 * @property {(method: string, apiPath: string, body: unknown) => ValidationResult} validateRequest
 * @property {(method: string, apiPath: string, status: number | string, body: unknown) => ValidationResult} validateResponse
 * @property {() => Example[]} examples
 * @property {(example: Example) => ValidationResult} validateExample
 */

// ajv-formats is CommonJS; its plugin is both the module and `.default`.
const addFormats = ajvFormats.default;

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

/**
 * @param {string} [specPath]
 * @returns {Promise<Contract>}
 */
export async function loadContract(specPath = SPEC_PATH) {
  const spec = parse(readFileSync(specPath, "utf8"));
  const specDir = path.dirname(specPath);

  const ajv = new Ajv2020({ strict: true, allErrors: true });
  addFormats(ajv);
  ajv.addVocabulary(OPENAPI_ROOT_FIELDS);
  ajv.addSchema({ ...spec, $id: SPEC_ID });

  /** @type {Map<string, ValidateFunction>} */
  const validators = new Map();

  /** @param {Target} target */
  function validatorFor(target) {
    const ref = SPEC_ID + toPointer(mediaSchemaParts(spec, target));
    let check = validators.get(ref);
    if (!check) {
      check = ajv.compile({ $ref: ref });
      validators.set(ref, check);
    }
    return check;
  }

  /**
   * @param {Target} target
   * @param {unknown} body
   * @returns {ValidationResult}
   */
  function validate(target, body) {
    const check = validatorFor(target);
    return { errors: check(body) ? [] : (check.errors ?? []).map(formatError) };
  }

  return {
    spec,

    validateRequest(method, apiPath, body) {
      return validate({ method, path: apiPath, part: "request" }, body);
    },

    validateResponse(method, apiPath, status, body) {
      return validate({ method, path: apiPath, part: "response", status: String(status) }, body);
    },

    examples() {
      return listExamples(spec, specDir);
    },

    validateExample(example) {
      let body;
      try {
        body = JSON.parse(readFileSync(example.file, "utf8"));
      } catch (error) {
        return { errors: [`cannot read ${example.file}: ${error instanceof Error ? error.message : error}`] };
      }
      return validate(example, body);
    },
  };
}

/**
 * JSON files under examples/ that no `externalValue` in the spec points to.
 * @param {Contract} contract
 * @param {string} [examplesDir]
 * @returns {string[]}
 */
export function unreferencedExampleFiles(contract, examplesDir = path.join(CONTRACTS_DIR, "examples")) {
  const referenced = new Set(contract.examples().map((example) => example.file));
  return readdirSync(examplesDir, { recursive: true, encoding: "utf8" })
    .filter((file) => file.endsWith(".json"))
    .map((file) => path.join(examplesDir, file))
    .filter((file) => !referenced.has(file))
    .sort();
}

/**
 * @param {any} spec
 * @param {string} specDir
 * @returns {Example[]}
 */
function listExamples(spec, specDir) {
  /** @type {Example[]} */
  const found = [];
  for (const [apiPath, pathItem] of Object.entries(spec.paths ?? {})) {
    for (const method of HTTP_METHODS) {
      const operation = /** @type {any} */ (pathItem)[method];
      if (!operation) continue;
      /** @type {{ part: "request" | "response", status?: string, node: any }[]} */
      const bodies = [];
      if (operation.requestBody) {
        bodies.push({ part: "request", node: resolve(spec, operation.requestBody).node });
      }
      for (const [status, response] of Object.entries(operation.responses ?? {})) {
        bodies.push({ part: "response", status, node: resolve(spec, response).node });
      }
      for (const { part, status, node } of bodies) {
        const examples = node?.content?.[JSON_MEDIA_TYPE]?.examples ?? {};
        for (const [name, exampleOrRef] of Object.entries(examples)) {
          const example = resolve(spec, exampleOrRef).node;
          if (!example?.externalValue) continue;
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

/**
 * JSON-pointer parts of the schema for a target's JSON body.
 * @param {any} spec
 * @param {Target} target
 * @returns {string[]}
 */
function mediaSchemaParts(spec, { method, path: apiPath, part, status }) {
  const operationParts = ["paths", apiPath, method.toLowerCase()];
  if (!get(spec, operationParts)) throw new Error(`No operation ${method.toUpperCase()} ${apiPath} in the spec`);

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

/**
 * Follows local OpenAPI $refs (#/components/...) to the object they name.
 * @param {any} spec
 * @param {any} node
 * @param {string[]} parts Where `node` sits in the spec.
 * @returns {{ node: any, parts: string[] }}
 */
function resolve(spec, node, parts = []) {
  const seen = new Set();
  while (node && typeof node.$ref === "string") {
    if (!node.$ref.startsWith("#/")) throw new Error(`Only local $refs are supported: ${node.$ref}`);
    if (seen.has(node.$ref)) throw new Error(`Circular $ref: ${node.$ref}`);
    seen.add(node.$ref);
    parts = node.$ref
      .slice(2)
      .split("/")
      .map((/** @type {string} */ part) => part.replaceAll("~1", "/").replaceAll("~0", "~"));
    node = get(spec, parts);
  }
  return { node, parts };
}

/**
 * @param {any} root
 * @param {string[]} parts
 * @returns {any}
 */
function get(root, parts) {
  return parts.reduce((node, part) => (node == null ? undefined : node[part]), root);
}

/** @param {string[]} parts */
function toPointer(parts) {
  return "#" + parts.map((part) => "/" + part.replaceAll("~", "~0").replaceAll("/", "~1")).join("");
}

/** @param {ErrorObject} error */
function formatError(error) {
  const where = error.instancePath || "/";
  if (error.keyword === "additionalProperties") {
    return `${where} must not have property "${error.params.additionalProperty}"`;
  }
  return `${where} ${error.message}`;
}
