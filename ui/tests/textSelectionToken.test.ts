// Selected text has its own colour, and the rule that applies it stays outside
// the cascade layers so it beats Zeron's managed inputs and antd without
// editing them (GWKS-540).
import assert from "node:assert/strict";
import fs from "node:fs";
import { test } from "node:test";

const css = fs.readFileSync(new URL("../src/app.css", import.meta.url), "utf8");

test("::selection uses --text-selection and is not inside any @layer", () => {
  const at = css.indexOf("::selection {");
  assert.ok(at > 0, "a global ::selection rule exists");
  assert.match(css.slice(at, at + 120), /background-color:\s*var\(--text-selection\)/);
  let depth = 0;
  for (const ch of css.slice(0, at)) { if (ch === "{") depth++; else if (ch === "}") depth--; }
  assert.equal(depth, 0, "the rule is at top level, not inside @layer or another block");
});

test("both themes define --text-selection", () => {
  assert.match(css, /:root\s*\{[^}]*--text-selection:/);
  assert.match(css, /\.dark\s*\{[^}]*--text-selection:/);
});
