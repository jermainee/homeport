#!/usr/bin/env node
// Validates every example the spec references against its operation's schema,
// and fails on example files the spec does not reference.
import path from "node:path";
import { CONTRACTS_DIR, loadContract, unreferencedExampleFiles } from "../lib/contract.mjs";

const contract = await loadContract();
const examples = contract.examples();
let failures = 0;

for (const example of examples) {
  const where = `${example.method.toUpperCase()} ${example.path} ${example.part}${example.status ? ` ${example.status}` : ""}`;
  const file = path.relative(CONTRACTS_DIR, example.file);
  const { errors } = contract.validateExample(example);
  if (errors.length === 0) {
    console.log(`ok    ${file} (${where})`);
  } else {
    failures++;
    console.log(`FAIL  ${file} (${where})`);
    for (const error of errors) console.log(`        ${error}`);
  }
}

for (const file of unreferencedExampleFiles(contract)) {
  failures++;
  console.log(`FAIL  ${path.relative(CONTRACTS_DIR, file)} is not referenced by the spec`);
}

if (failures > 0) {
  console.log(`\n${failures} problem(s) in ${examples.length} examples`);
  process.exit(1);
}
console.log(`\n${examples.length} examples valid`);
