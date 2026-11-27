import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { copyFileSync, mkdirSync, mkdtempSync, rmSync, unlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

test("the UI and architecture gates enforce copy budgets", (t) => {
  const root = mkdtempSync(join(tmpdir(), "gpuworks-copy-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const dir = join(root, "ui/src/i18n/locales/zh");
  mkdirSync(dir, { recursive: true });
  const enDir = join(root, "ui/src/i18n/locales/en");
  mkdirSync(enDir, { recursive: true });
  writeFileSync(join(enDir, "common.json"), "{}");
  const page = join(root, "ui/src/Page.tsx");
  writeFileSync(page, "");
  mkdirSync(join(root, "scripts"));
  for (const file of ["check_architecture.sh", "check_ui_architecture.sh", "ui_copy_audit.mjs", "ui_overlay_imports.mjs", "ui_antd_inventory.mjs", "ui_locale_changes.mjs"]) {
    copyFileSync(new URL("../../scripts/" + file, import.meta.url), join(root, "scripts", file));
  }
  copyFileSync(new URL("../antd-inventory.json", import.meta.url), join(root, "ui/antd-inventory.json"));
  const setLongLimit = (limit: number) => writeFileSync(join(root, "scripts/arch-constraints.txt"),
    `ui_copy_long_strings\t${limit}\tmax\nui_copy_caveat_strings\t0\tmax\nui_copy_helper_strings\t0\tmax\nui_copy_message_strings\t0\tmax\nui_copy_sentences_strings\t0\tmax\nui_copy_clauses_strings\t0\tmax\nui_copy_command_strings\t0\tmax\nui_copy_en_strings\t0\tmax\nui_copy_inline_help_refs\t0\tmax\nui_common_locale_keys\t0\tmax\nee_ui_internal_imports\t0\tmax\n`);
  const run = (json: string, script = "check_ui_architecture.sh") => {
    writeFileSync(join(dir, "common.json"), json);
    return spawnSync("sh", [join("scripts", script)], { cwd: root, encoding: "utf8" });
  };
  setLongLimit(0);
  const ok = run(JSON.stringify({ title: "字".repeat(40), apiError: { X: "字".repeat(80) }, infoHelp: "字".repeat(200) }));
  assert.equal(ok.status, 0, ok.stderr);
  for (const [value, kind] of [["字".repeat(121), "long"], ["仅供参考", "caveat"], ["字".repeat(41), "helper"], ["一。二。三。", "sentences"]]) {
    const failure = run(JSON.stringify({ title: value }));
    assert.equal(failure.status, 1);
    assert.match(failure.stderr, new RegExp(kind + "\\tcommon:title"));
  }
  // Clauses chained with ； are sentences written to pass the sentence count;
  // one "problem；fix" pair and a colon that opens no chain are left alone.
  assert.equal(run(JSON.stringify({ title: "版本过旧；请先升级", label: "目标版本：{{version}}" })).status, 0);
  for (const value of ["色块按记账绘制：切片按上限计；黑线是实际占用。", "甲；乙；丙"]) {
    const failure = run(JSON.stringify({ title: value }));
    assert.equal(failure.status, 1);
    assert.match(failure.stderr, /clauses\tcommon:title/);
  }
  // A command the user types is a block's text (a *Command key), never part
  // of a sentence, not even inside a HelpTip.
  assert.equal(run(JSON.stringify({ uninstallCommand: "gpuworks-agent uninstall --state <目录>", note: "由 gpuworks agent 上报，经 sudo 写入" })).status, 0);
  for (const [key, value] of [["title", "请执行 gpuworks-agent uninstall"], ["infoHelp", "在主机上执行 sudo systemctl restart docker"], ["hint", "重新执行 kubectl apply"]]) {
    const failure = run(JSON.stringify({ [key]: value }));
    assert.equal(failure.status, 1);
    assert.match(failure.stderr, new RegExp("command\\tcommon:" + key));
  }
  const message = run(JSON.stringify({ apiError: { X: "字".repeat(81) } }));
  assert.equal(message.status, 1);
  assert.match(message.stderr, /message\tcommon:apiError\.X/);
  assert.doesNotMatch(message.stderr, /helper\t/);
  // A Help key keeps its 200 characters only inside a HelpTip.
  writeFileSync(page, '<HelpTip content={t("infoHelp")} />\n<Form.Item extra={t("infoHelp")} />\n<p className="muted">{t("infoHelp")}</p>\n<DetailSection title={t("title")} extra={<HelpTip content={t("infoHelp")} />}>\n');
  const inline = run(JSON.stringify({ infoHelp: "说明" }));
  assert.equal(inline.status, 1);
  assert.match(inline.stderr, /inlinehelp\tPage\.tsx:2/);
  assert.match(inline.stderr, /inlinehelp\tPage\.tsx:3/);
  assert.doesNotMatch(inline.stderr, /Page\.tsx:1/);
  assert.doesNotMatch(inline.stderr, /Page\.tsx:4/, "a HelpTip in a section title's extra is the HelpTip the rule asks for");
  writeFileSync(page, "");
  writeFileSync(join(enDir, "common.json"), JSON.stringify({ title: "x".repeat(241) }));
  const english = run(JSON.stringify({ title: "短标题" }));
  assert.equal(english.status, 1);
  assert.match(english.stderr, /en\tcommon:title/);
  // common only shrinks: a new shared key is over the budget, a product error
  // code's text (one per code, kept in step by i18n-check) is not counted.
  writeFileSync(join(enDir, "common.json"), JSON.stringify({ apiError: { X: "Failed" } }));
  assert.equal(run(JSON.stringify({ apiError: { X: "失败" } })).status, 0);
  writeFileSync(join(enDir, "common.json"), JSON.stringify({ nodeDrawerTitle: "Node" }));
  const grown = run(JSON.stringify({ nodeDrawerTitle: "节点" }));
  assert.equal(grown.status, 1);
  assert.match(grown.stdout, /ui_common_locale_keys 1\/0 \(max\)/);
  assert.match(grown.stderr, /common\.json grew: put a feature's copy in its own namespace file/);
  writeFileSync(join(enDir, "common.json"), "{}");
  assert.notEqual(run("{").status, 0);
  const legacy = run(JSON.stringify({ title: "字".repeat(121) }), "check_architecture.sh");
  assert.equal(legacy.status, 1);
  assert.match(legacy.stderr, /long\tcommon:title/);

  setLongLimit(1);
  const slack = run(JSON.stringify({ title: "短标题" }));
  assert.equal(slack.status, 0, slack.stderr);
  assert.match(slack.stderr, /WARNING: ui_copy_long_strings is below its budget \(0 < 1\)/);
});

// The --base glue reads each changed locale file at the base with git show,
// lists the languages from the locale directories and keeps the source files
// the change does not touch: drive it through a real repository.
test("ui-check --base skips the walk only for locale changes that add texts no page renders", (t) => {
  const root = mkdtempSync(join(tmpdir(), "gpuworks-locale-base-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const git = (...args: string[]) => {
    const r = spawnSync("git", ["-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", ...args], { cwd: root, encoding: "utf8" });
    assert.equal(r.status, 0, r.stderr);
    return r.stdout;
  };
  mkdirSync(join(root, "scripts"));
  for (const file of ["ui_antd_inventory.mjs", "ui_locale_changes.mjs"]) {
    copyFileSync(new URL("../../scripts/" + file, import.meta.url), join(root, "scripts", file));
  }
  const locales = join(root, "ui/src/i18n/locales");
  const write = (language: string, value: object) => {
    mkdirSync(join(locales, language), { recursive: true });
    writeFileSync(join(locales, language, "common.json"), JSON.stringify(value));
  };
  const base = { title: "T", apiError: { OLD: "old" } };
  write("en", base);
  write("zh", base);
  writeFileSync(join(root, "ui/src/main.tsx"), "");
  writeFileSync(join(root, "ui/src/App.tsx"), "");
  writeFileSync(join(root, "ui/src/navigation.tsx"), "");
  writeFileSync(join(root, "ui/src/Page.tsx"), 'export const x = t("apiError.OLD");\n');
  git("init", "-q");
  git("add", "-A");
  git("commit", "-q", "-m", "base");
  const affected = () => {
    const r = spawnSync("node", ["scripts/ui_antd_inventory.mjs", "--base", "HEAD"], { cwd: root, encoding: "utf8" });
    assert.equal(r.status, 0, r.stderr);
    return { pages: JSON.parse(r.stdout), log: r.stderr };
  };
  const reset = () => { write("en", base); write("zh", base); };

  // A new apiError text in every language: nothing to walk, and the log names it.
  write("en", { ...base, apiError: { OLD: "old", NEW: "new" } });
  write("zh", { ...base, apiError: { OLD: "old", NEW: "新" } });
  let got = affected();
  assert.deepEqual(got.pages, []);
  assert.match(got.log, /only adds texts no existing page renders: common:apiError\.NEW/);

  // Every other change walks every page, with the reason in the log.
  for (const [en, zh, reason] of [
    [{ ...base, title: "changed" }, base, /common:title changes/],
    [{ ...base, hint: "h" }, { ...base, hint: "h" }, /common:hint is not under a prefix/],
    [{ ...base, apiError: { OLD: "old", NEW: "new" } }, base, /common:apiError\.NEW is not added in zh/],
  ] as const) {
    write("en", en);
    write("zh", zh);
    got = affected();
    assert.deepEqual(got.pages, ["*"]);
    assert.match(got.log, reason);
    reset();
  }

  // A removed locale file reads as absent after the change.
  unlinkSync(join(locales, "zh", "common.json"));
  got = affected();
  assert.deepEqual(got.pages, ["*"]);
  assert.match(got.log, /zh\/common\.json is removed/);
  reset();

  // A source file the change does not touch that names the new key literally
  // would render it.
  writeFileSync(join(root, "ui/src/Page.tsx"), 'export const x = t("apiError.NEW");\n');
  git("commit", "-q", "-am", "page names NEW");
  write("en", { ...base, apiError: { OLD: "old", NEW: "new" } });
  write("zh", { ...base, apiError: { OLD: "old", NEW: "新" } });
  got = affected();
  assert.deepEqual(got.pages, ["*"]);
  assert.match(got.log, /ui\/src\/Page\.tsx already names common:apiError\.NEW/);
});
