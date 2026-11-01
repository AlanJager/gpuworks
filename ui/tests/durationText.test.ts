import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";

import { createInstance } from "i18next";

import { durationText } from "../src/pages/durationText.ts";
import { paramList, paramNumber, paramText } from "../src/pages/eventParams.ts";

const bundle = (lang: string, ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lang}/${ns}.json`, import.meta.url), "utf8"));

// A t of another namespace (the scheduling center's) words a span with the
// same keys: durationText names its namespace itself.
async function schedulingT(lang: "en" | "zh") {
  const i18n = createInstance();
  await i18n.init({
    lng: lang,
    resources: { [lang]: { deployments: bundle(lang, "deployments"), scheduling: bundle(lang, "scheduling") } },
    interpolation: { escapeValue: false },
  });
  return i18n.getFixedT(lang, "scheduling");
}

test("a span reads rounded, exact or elapsed, in both languages", async () => {
  const en = await schedulingT("en");
  const rounded = (s: number) => durationText(en, s);
  assert.deepEqual([rounded(45.4), rounded(90), rounded(600), rounded(7200), rounded(172800), rounded(5400)], ["45 s", "2 min", "10 min", "2 h", "2 d", "90 min"]);
  const exact = (s: number) => durationText(en, s, "exact");
  assert.deepEqual([exact(30), exact(90), exact(300), exact(7200), exact(172800)], ["30 s", "90 s", "5 min", "2 h", "2 d"]);
  const elapsed = (s: number) => durationText(en, s, "elapsed");
  assert.deepEqual([elapsed(30), elapsed(600), elapsed(7200), elapsed(5430)], ["30 s", "10 min", "2 h 0 min", "1 h 31 min"]);
  const zh = await schedulingT("zh");
  assert.deepEqual([durationText(zh, 600), durationText(zh, 86400, "exact"), durationText(zh, 3900, "elapsed")], ["10 分钟", "1 天", "1 小时 5 分钟"]);
});

test("event params are read by kind, and anything else reads as empty", () => {
  const event = { id: 1, kind: "automation", code: "AUTO_SLEEP", created_at: "", params: { s: " a ", n: 3, bad: Number.NaN, l: ["x", 2, "y"], o: {} } };
  assert.equal(paramText(event, "s"), "a");
  assert.equal(paramText(event, "n"), "");
  assert.equal(paramNumber(event, "n"), 3);
  assert.equal(paramNumber(event, "bad"), undefined);
  assert.equal(paramNumber(event, "s"), undefined);
  assert.deepEqual(paramList(event, "l"), ["x", "y"]);
  assert.deepEqual(paramList(event, "o"), []);
  const bare = { id: 2, kind: "automation", code: "AUTO_WAKE", created_at: "" };
  assert.equal(paramText(bare, "s"), "");
  assert.equal(paramNumber(bare, "n"), undefined);
  assert.deepEqual(paramList(bare, "l"), []);
});
