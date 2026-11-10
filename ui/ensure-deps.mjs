#!/usr/bin/env node
// Keeps ui/node_modules in step with package-lock.json. node_modules is not in
// git, so a pull or rebase that changes the dependencies leaves an old install
// behind; this runs before the UI is built, served or tested (npm pre-scripts
// and `make ui`) and reinstalls with `npm ci` when any package npm recorded as
// installed is missing or at a different version than package-lock.json pins,
// transitive ones included. Nothing to do is the common case and costs two
// file reads. Node built-ins only: it must run when node_modules is stale or
// absent.
//
// Set GPUWORKS_SKIP_UI_DEPS_CHECK=1 to skip it (offline work on a known-good
// install).
import { existsSync, readFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const skip = process.env.GPUWORKS_SKIP_UI_DEPS_CHECK;
if (skip && skip !== "0" && skip !== "false") process.exit(0);

const ui = dirname(fileURLToPath(import.meta.url));
const readJSON = (path) => JSON.parse(readFileSync(join(ui, path), "utf8"));
const lock = readJSON("package-lock.json");
const wanted = Object.entries(lock.packages ?? {})
  // The root, linked local packages and platform-optional ones are not compared.
  .filter(([key, entry]) => key.startsWith("node_modules/") && entry.version && !entry.link && !entry.optional);

const stale = [];
const record = join(ui, "node_modules", ".package-lock.json");
if (existsSync(record)) {
  // npm records every package it installed here, transitive ones included,
  // so one read covers the whole tree.
  const installed = JSON.parse(readFileSync(record, "utf8")).packages ?? {};
  for (const [key, entry] of wanted) {
    const version = installed[key]?.version;
    if (version !== entry.version) stale.push(`${key.slice("node_modules/".length)} ${version ?? "missing"} -> ${entry.version}`);
  }
} else {
  stale.push("no install found");
}
if (stale.length === 0) process.exit(0);

const shown = stale.slice(0, 3).join(", ") + (stale.length > 3 ? `, +${stale.length - 3} more` : "");
console.error(`ui: installed dependencies differ from package-lock.json (${shown}); running npm ci`);
const windows = process.platform === "win32";
const result = spawnSync(windows ? "npm.cmd" : "npm", ["ci", "--no-audit", "--no-fund"], { cwd: ui, stdio: "inherit", shell: windows });
if (result.status !== 0) {
  console.error("ui: npm ci failed. Run it in ui/ by hand, or set GPUWORKS_SKIP_UI_DEPS_CHECK=1 to skip this check.");
  process.exit(result.status ?? 1);
}
