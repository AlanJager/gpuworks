// The guards around console overlays, run as the build runs them:
// scripts/ui_overlay_imports.mjs (the ee_ui_internal_imports budget),
// scripts/ui_overlay_guard.sh (make ui / release-packages) and the extra
// locale and code roots of scripts/i18n-check.mjs.
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { after, test } from "node:test";
import { fileURLToPath } from "node:url";

const repo = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const scratch = fs.mkdtempSync(path.join(os.tmpdir(), "gpuworks-overlay-"));
after(() => fs.rmSync(scratch, { recursive: true, force: true }));

function write(root: string, file: string, text: string) {
  const target = path.join(root, file);
  fs.mkdirSync(path.dirname(target), { recursive: true });
  fs.writeFileSync(target, text);
}

test("an overlay may import the SDK and its own files; anything else is counted with file and line", () => {
  const root = path.join(scratch, "imports");
  write(root, "ui/src/extensions/sdk.ts", "export {};\n");
  write(root, "ui/src/extensions/index.ts", "import x from \"../navigation\";\n");
  write(root, "ui/src/extensions/audit/index.ts", [
    "import { Button, useState } from \"../sdk\";",
    "import type { ConsoleExtension } from \"../sdk.ts\";",
    "import AuditPage from \"./pages/AuditPage\";",
    "import \"./audit.css\";",
    "// import { api } from \"../../api\";",
    "import { api } from \"../../api\";",
    "export { navGroups } from \"../../navigation\";",
  ].join("\n"));
  write(root, "ui/src/extensions/audit/pages/AuditPage.tsx", [
    "import { PageHeader } from \"../../sdk\";",
    "import { helper } from \"../lib/helper\";",
    "import {",
    "  Table,",
    "} from \"antd\";",
    "import { other } from \"../../other/index\";",
    "const lazy = () => import(\"../../../pages/NodesPage\");",
    "import { sdk } from \"@/extensions/sdk\";",
    "import { kit } from \"@/ui-kit/Notice\";",
  ].join("\n"));
  const out = execFileSync("node", [path.join(repo, "scripts/ui_overlay_imports.mjs"), root], { encoding: "utf8" });
  assert.deepEqual(out.trim().split("\n"), [
    "ui/src/extensions/audit/index.ts:6: ../../api",
    "ui/src/extensions/audit/index.ts:7: ../../navigation",
    "ui/src/extensions/audit/pages/AuditPage.tsx:5: antd",
    "ui/src/extensions/audit/pages/AuditPage.tsx:6: ../../other/index",
    "ui/src/extensions/audit/pages/AuditPage.tsx:7: ../../../pages/NodesPage",
    "ui/src/extensions/audit/pages/AuditPage.tsx:9: @/ui-kit/Notice",
  ]);
});

test("the community tree has no overlay, so the budget measures 0", () => {
  const out = execFileSync("node", [path.join(repo, "scripts/ui_overlay_imports.mjs")], { encoding: "utf8", cwd: repo });
  assert.equal(out, "");
});

test("a community UI build refuses checked-out overlays unless the build goes elsewhere", () => {
  const root = path.join(scratch, "guard");
  write(root, "scripts/ui_overlay_guard.sh", fs.readFileSync(path.join(repo, "scripts/ui_overlay_guard.sh"), "utf8"));
  write(root, "ui/src/extensions/index.ts", "export {};\n");
  const guard = (env: Record<string, string> = {}) => {
    const clean = { ...process.env };
    delete clean.GPUWORKS_UI_OUT_DIR;
    return spawnSync("sh", [path.join(root, "scripts/ui_overlay_guard.sh")], { encoding: "utf8", env: { ...clean, ...env } });
  };
  assert.equal(guard().status, 0, "registry files alone are the community tree");
  write(root, "ui/src/extensions/audit/index.ts", "export default { id: \"audit\" };\n");
  const refused = guard();
  assert.equal(refused.status, 1);
  assert.match(refused.stderr, /ui\/src\/extensions\/audit/);
  assert.match(refused.stderr, /GPUWORKS_UI_OUT_DIR/);
  assert.equal(guard({ GPUWORKS_UI_OUT_DIR: "/tmp/overlay-console" }).status, 0);
});

function i18nCheck(env: Record<string, string>) {
  return spawnSync("node", [path.join(repo, "scripts/i18n-check.mjs")], { encoding: "utf8", env: { ...process.env, ...env } });
}

test("i18n-check validates an extra locale root against the core languages and resolves extra codes with it", () => {
  const good = path.join(scratch, "locales-good");
  write(good, "en/audit.json", JSON.stringify({ nav: { audit: "Audit log {{count}}" }, AUDIT_EXPORT_FAILED: "Export failed" }));
  write(good, "zh/audit.json", JSON.stringify({ nav: { audit: "审计日志 {{count}}" }, AUDIT_EXPORT_FAILED: "导出失败" }));
  const codes = path.join(scratch, "messages");
  write(codes, "audit/codes.go", "package audit\n\nconst ErrorExportFailed = \"AUDIT_EXPORT_FAILED\"\n");
  const passed = i18nCheck({ GPUWORKS_I18N_EXTRA_LOCALES: good, GPUWORKS_I18N_EXTRA_MESSAGES: codes });
  assert.equal(passed.status, 0, passed.stderr);

  const missing = i18nCheck({ GPUWORKS_I18N_EXTRA_MESSAGES: codes });
  assert.equal(missing.status, 1);
  assert.match(missing.stderr, /product code AUDIT_EXPORT_FAILED has no en locale text/);

  const bad = path.join(scratch, "locales-bad");
  write(bad, "en/audit.json", JSON.stringify({ a: "One {{n}}", b: "Two" }));
  write(bad, "zh/audit.json", JSON.stringify({ a: "一", c: "三" }));
  write(bad, "en/common.json", JSON.stringify({ navigation: { logs: "Logs again" } }));
  write(bad, "zh/common.json", JSON.stringify({ navigation: { logs: "日志" } }));
  const failed = i18nCheck({ GPUWORKS_I18N_EXTRA_LOCALES: bad });
  assert.equal(failed.status, 1);
  assert.match(failed.stderr, /zh\/audit\.json:a: placeholders differ from en/);
  assert.match(failed.stderr, /zh\/audit\.json:b: missing key/);
  assert.match(failed.stderr, /zh\/audit\.json:c: orphan key/);
  assert.match(failed.stderr, /en\/common\.json:navigation\.logs: repeats a core key/);

  const noEnglish = path.join(scratch, "locales-zh-only");
  write(noEnglish, "zh/audit.json", "{}");
  assert.match(i18nCheck({ GPUWORKS_I18N_EXTRA_LOCALES: noEnglish }).stderr, /extra locale root has no en directory/);
  assert.match(i18nCheck({ GPUWORKS_I18N_EXTRA_MESSAGES: path.join(scratch, "nowhere") }).stderr, /extra messages directory does not exist/);
});

test("overlay tests live in ui/tests/ext-<id>/: git ignores them, the import budget skips them and the test scripts run them", () => {
  const ignored = (file: string) => spawnSync("git", ["check-ignore", "-q", file], { cwd: repo }).status === 0;
  assert.equal(ignored("ui/tests/ext-audit/auditPage.test.tsx"), true);
  assert.equal(ignored("ui/tests/ext-audit/fixtures/status.json"), true);
  assert.equal(ignored("ui/tests/consoleExtensionsGuards.test.ts"), false, "the console's own tests stay tracked");
  assert.equal(ignored("ui/tests/extended.test.tsx"), false, "only ext-<id> directories are ignored");

  // A test imports node:test, testing-library and the console's sources; outside
  // src/extensions/<id>/ none of that counts against the overlay import budget.
  const root = path.join(scratch, "overlay-tests");
  write(root, "ui/src/extensions/sdk.ts", "export {};\n");
  write(root, "ui/src/extensions/audit/index.ts", "import { Button } from \"@/extensions/sdk\";\n");
  write(root, "ui/tests/ext-audit/auditPage.test.tsx", [
    "import test from \"node:test\";",
    "import { render } from \"@testing-library/react\";",
    "import { api } from \"../../src/api\";",
  ].join("\n"));
  assert.equal(execFileSync("node", [path.join(repo, "scripts/ui_overlay_imports.mjs"), root], { encoding: "utf8" }), "");

  // npm test and the coverage run take the copied tests through UI_EXTRA_TESTS;
  // coverage still counts every source file, the overlay's included.
  const scripts = JSON.parse(fs.readFileSync(path.join(repo, "ui/package.json"), "utf8")).scripts as Record<string, string>;
  assert.match(scripts.test, /\$\{UI_EXTRA_TESTS:-\}/);
  assert.match(scripts["test:coverage"], /\$\{UI_EXTRA_TESTS:-\}/);
  assert.match(scripts["test:coverage"], /c8 --all --include='src\/\*\*/);
});
