#!/usr/bin/env node
// Counts UI strings that break docs/dev/ui.md "Copy on the page".
//   node scripts/ui_copy_audit.mjs <kind>   -> count for one kind (see KINDS)
//   node scripts/ui_copy_audit.mjs list     -> all, one "kind id len text" per line
// Each count feeds ui_copy_<kind>_strings (ui_copy_inline_help_refs for
// inlinehelp) in scripts/arch-constraints.txt. Node built-ins only.
import { readdirSync, readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

// Hard ceilings: past these a string is cut or moved to the docs.
export const LONG = 120;
export const HELP_LONG = 200;
// One line of helper text on the page (ui.md "One helper line").
export const HELPER = 40;
// An error, warning or reason: the problem and the next step.
export const MESSAGE = 80;
// A string on the page is at most this many sentences; more is an explanation.
export const SENTENCES = 2;
// English runs about twice the Chinese length.
export const EN_LONG = 240;
export const EN_HELP_LONG = 400;

// A nested reference or a placeholder renders as the value it stands for, not
// as its name.
const NESTED = /\$t\([^)]*\)|\{\{[^}]*\}\}/g;
const isHelp = (key) => /help$/i.test(key);
const isMessage = (key) => key.split(".").some((part) =>
  /^(apiError|warnings?|errors?|schedule|finding|fallback|blocked|unavailable|todo)$/.test(part) || /(Reason|Error|Warning)$/.test(part) || part === "reason");
// Caveats the rules move into a HelpTip; facts such as "不包含模型文件" are not caveats.
export const CAVEAT = /不保证|仅供参考|不代表|未经(节点)?实测|并不意味|不改变|仅用于界面演示/;

const root = join(dirname(fileURLToPath(import.meta.url)), "..", "ui", "src");

function* strings(node, prefix) {
  for (const [key, value] of Object.entries(node)) {
    const path = prefix ? `${prefix}.${key}` : key;
    if (value && typeof value === "object") yield* strings(value, path);
    else if (typeof value === "string") yield [path, value];
  }
}

function* locale(name) {
  const dir = join(root, "i18n", "locales", name);
  for (const file of readdirSync(dir).filter((f) => f.endsWith(".json")).sort()) {
    const ns = file.slice(0, -5);
    for (const [key, text] of strings(JSON.parse(readFileSync(join(dir, file), "utf8")), "")) yield [`${ns}:${key}`, key, text, [...text.replace(NESTED, "名称")].length];
  }
}

const found = [];
const add = (kind, id, length, text) => found.push({ kind, id, length, text });

for (const [id, key, text, length] of locale("zh")) {
  const help = isHelp(key);
  if (length > (help ? HELP_LONG : LONG)) add("long", id, length, text);
  if (help) continue;
  if (CAVEAT.test(text)) add("caveat", id, length, text);
  if (isMessage(key)) { if (length > MESSAGE) add("message", id, length, text); }
  else if (length > HELPER) add("helper", id, length, text);
  if ((text.match(/[。！？]/g) ?? []).length > SENTENCES) add("sentences", id, length, text);
}
for (const [id, key, text, length] of locale("en")) {
  if (length > (isHelp(key) ? EN_HELP_LONG : EN_LONG)) add("en", id, length, text);
}

// A key named *Help is HelpTip content and is shown only there. These are the
// places that put one on the page instead: a form item's `extra`, or the text
// of a paragraph, span, div or antd Text.
const HELP_CALL = String.raw`t\(\s*["'\x60][^"'\x60]*[Hh]elp["'\x60]`;
const INLINE = new RegExp(String.raw`extra=\{[^\n]*${HELP_CALL}|<(?:p|span|div|Text|Typography\.Text|Typography\.Paragraph)\b[^>]*>[^<\n]*\{[^<\n]*${HELP_CALL}`);
function* sources(dir) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) { if (entry.name !== "i18n") yield* sources(path); }
    else if (entry.name.endsWith(".tsx")) yield path;
  }
}
for (const path of [...sources(root)].sort()) {
  readFileSync(path, "utf8").split("\n").forEach((line, i) => {
    if (INLINE.test(line)) add("inlinehelp", `${path.slice(root.length + 1)}:${i + 1}`, 0, line.trim());
  });
}

export const KINDS = ["long", "caveat", "helper", "message", "sentences", "en", "inlinehelp"];
const mode = process.argv[2] ?? "list";
if (KINDS.includes(mode)) console.log(found.filter((f) => f.kind === mode).length);
else if (mode === "list") for (const f of found) console.log(`${f.kind}\t${f.id}\t${f.length}\t${f.text.replace(/\n/g, "\\n")}`);
else { console.error(`unknown mode ${mode}: want list or one of ${KINDS.join(", ")}`); process.exit(2); }
