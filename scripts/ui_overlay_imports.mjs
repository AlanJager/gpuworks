#!/usr/bin/env node
// Lists the imports of console overlay modules (ui/src/extensions/<id>/, which
// a separately built console copies in) that reach past the overlay SDK: an
// overlay may import ui/src/extensions/sdk.ts and files of its own directory,
// nothing else, so the console can change its internals without breaking
// overlays. Prints one "file:line: specifier" per offending import; the count
// is the ee_ui_internal_imports budget in scripts/arch-constraints.txt.
//   node scripts/ui_overlay_imports.mjs [repo-root]
// Node built-ins only.
import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join, posix, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

const root = resolve(process.argv[2] ?? join(dirname(fileURLToPath(import.meta.url)), ".."));
const extensionsDir = join(root, "ui", "src", "extensions");
const sourceFile = /\.(?:[cm]?[jt]sx?)$/;
// Static imports and re-exports (the specifier may sit on the closing line of
// a multi-line import), side-effect imports, dynamic import() and require().
const specifierPatterns = [
  /\bfrom\s*["']([^"']+)["']/g,
  /^\s*import\s*["']([^"']+)["']/g,
  /\bimport\s*\(\s*["']([^"']+)["']\s*\)/g,
  /\brequire\s*\(\s*["']([^"']+)["']\s*\)/g,
];
const sdkSpecifiers = new Set(["@/extensions/sdk", "@/extensions/sdk.ts"]);

function filesUnder(directory) {
  const out = [];
  for (const name of readdirSync(directory).sort()) {
    const file = join(directory, name);
    if (statSync(file).isDirectory()) out.push(...filesUnder(file));
    else if (sourceFile.test(name)) out.push(file);
  }
  return out;
}

// allowed reports whether an overlay file may import specifier: the SDK, or a
// relative path that stays inside the overlay's own directory.
function allowed(file, overlayDir, specifier) {
  if (sdkSpecifiers.has(specifier)) return true;
  if (!specifier.startsWith(".")) return false;
  const target = resolve(dirname(file), specifier);
  const sdk = join(extensionsDir, "sdk");
  if (target === sdk || target === `${sdk}.ts`) return true;
  return target === overlayDir || target.startsWith(overlayDir + sep);
}

const hits = [];
if (existsSync(extensionsDir)) {
  for (const entry of readdirSync(extensionsDir).sort()) {
    const overlayDir = join(extensionsDir, entry);
    if (!statSync(overlayDir).isDirectory()) continue;
    for (const file of filesUnder(overlayDir)) {
      const lines = readFileSync(file, "utf8").split("\n");
      lines.forEach((line, index) => {
        if (/^\s*\/\//.test(line)) return;
        for (const pattern of specifierPatterns) {
          for (const match of line.matchAll(pattern)) {
            if (!allowed(file, overlayDir, match[1])) {
              hits.push(`${relative(root, file).split(sep).join(posix.sep)}:${index + 1}: ${match[1]}`);
            }
          }
        }
      });
    }
  }
}
for (const hit of hits) console.log(hit);
