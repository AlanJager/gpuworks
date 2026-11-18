import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { copyFileSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

test("the UI gate enforces copy budgets and fails on invalid locale data", (t) => {
  const root = mkdtempSync(join(tmpdir(), "gpuworks-copy-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const dir = join(root, "ui/src/i18n/locales/zh");
  mkdirSync(dir, { recursive: true });
  mkdirSync(join(root, "scripts"));
  for (const file of ["check_ui_architecture.sh", "ui_copy_audit.mjs", "ui_overlay_imports.mjs", "ui_antd_inventory.mjs"]) {
    copyFileSync(new URL("../../scripts/" + file, import.meta.url), join(root, "scripts", file));
  }
  copyFileSync(new URL("../antd-inventory.json", import.meta.url), join(root, "ui/antd-inventory.json"));
  writeFileSync(join(root, "scripts/arch-constraints.txt"),
    "ui_copy_long_strings\t0\tmax\nui_copy_caveat_strings\t0\tmax\nee_ui_internal_imports\t0\tmax\n");
  const run = (json: string) => {
    writeFileSync(join(dir, "common.json"), json);
    return spawnSync("sh", ["scripts/check_ui_architecture.sh"], { cwd: root, encoding: "utf8" });
  };
  const ok = run(JSON.stringify({ title: "字".repeat(120), infoHelp: "字".repeat(200) }));
  assert.equal(ok.status, 0, ok.stderr);
  for (const [value, kind] of [["字".repeat(121), "long"], ["仅供参考", "caveat"]]) {
    const failure = run(JSON.stringify({ title: value }));
    assert.equal(failure.status, 1);
    assert.match(failure.stderr, new RegExp(kind + "\\tcommon:title"));
  }
  assert.notEqual(run("{").status, 0);
});
