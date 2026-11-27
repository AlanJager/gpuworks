import assert from "node:assert/strict";
import { test } from "node:test";
import { localeChangeImpact } from "../../scripts/ui_locale_changes.mjs";

// The browser check walks every page for a locale change unless the change
// only adds product error texts (ADR 0086, a deliberate narrowing). Each case
// below is one way an added or changed text reaches an existing page by a
// lookup the module can see, so it must still walk every page; the first case
// is the one that may skip.

const before = { apiError: { IMAGE_IN_USE: "In use", LOGIN_THROTTLED: "Slow down" }, actions: { save: "Save" } };
const withCode = (code, text = "New") => ({ ...before, apiError: { ...before.apiError, [code]: text } });
const both = (after, afterZh = after, base = before) => [
  { language: "en", namespace: "common", before: base, after },
  { language: "zh", namespace: "common", before: base, after: afterZh },
];
const impact = (changes, sources = []) => localeChangeImpact({ languages: ["en", "zh"], changes, sources });

test("a new apiError text in every language that no source names renders nowhere yet", () => {
  const got = impact(both(withCode("BRAND_NEW")));
  assert.equal(got.inert, true, got.reason);
  assert.deepEqual(got.added, ["common:apiError.BRAND_NEW"]);
});

test("changing an existing text can reflow every page that shows it", () => {
  const got = impact(both(withCode("IMAGE_IN_USE", "Still in use by a deployment")));
  assert.equal(got.inert, false);
  assert.match(got.reason, /IMAGE_IN_USE changes/);
});

test("removing a text changes what its pages fall back to", () => {
  const { IMAGE_IN_USE, ...rest } = before.apiError;
  const got = impact(both({ ...before, apiError: rest }));
  assert.equal(got.inert, false);
  assert.match(got.reason, /IMAGE_IN_USE is removed/);
});

test("removing a whole locale file walks every page", () => {
  const got = impact([{ language: "en", namespace: "common", before, after: null }, both(withCode("BRAND_NEW"))[1]]);
  assert.equal(got.inert, false);
  assert.match(got.reason, /common\.json is removed/);
});

test("a text added in one language only changes what the others fall back to", () => {
  const got = impact([both(withCode("BRAND_NEW"))[0]]);
  assert.equal(got.inert, false);
  assert.match(got.reason, /not added in zh/);
});

test("a text added under a prefix no two-way check guards may already be looked up through a variable", () => {
  const got = impact(both({ ...before, actions: { ...before.actions, brandNew: "New" } }));
  assert.equal(got.inert, false);
  assert.match(got.reason, /actions\.brandNew is not under a prefix/);
});

test("apiError is guarded only in common: the same prefix elsewhere is not", () => {
  const got = impact([
    { language: "en", namespace: "deploy", before: {}, after: { apiError: { BRAND_NEW: "x" } } },
    { language: "zh", namespace: "deploy", before: {}, after: { apiError: { BRAND_NEW: "x" } } },
  ]);
  assert.equal(got.inert, false);
  assert.match(got.reason, /deploy:apiError\.BRAND_NEW is not under a prefix/);
});

test("a plural variant of an existing text changes how that text renders", () => {
  const got = impact(both(withCode("IMAGE_IN_USE_one")));
  assert.equal(got.inert, false);
  assert.match(got.reason, /variant of the existing apiError\.IMAGE_IN_USE/);
});

test("the base of existing variants changes how a lookup without count resolves", () => {
  const plural = { ...before, apiError: { ...before.apiError, QUEUED_one: "1 queued", QUEUED_other: "{{count}} queued" } };
  const got = impact(both({ ...plural, apiError: { ...plural.apiError, QUEUED: "Queued" } }, undefined, plural));
  assert.equal(got.inert, false);
  assert.match(got.reason, /base of the existing apiError\.QUEUED_/);
});

test("a source outside the change that already names the key renders the new text", () => {
  for (const text of [`t("apiError.BRAND_NEW")`, `t('apiError.BRAND_NEW')`, "t(`apiError.BRAND_NEW`)", `t("common:apiError.BRAND_NEW")`]) {
    const got = impact(both(withCode("BRAND_NEW")), [{ path: "ui/src/pages/LoginPage.tsx", text }]);
    assert.equal(got.inert, false, text);
    assert.match(got.reason, /LoginPage\.tsx already names common:apiError\.BRAND_NEW/);
  }
});

test("a dynamic apiError lookup does not count as naming the key", () => {
  const got = impact(both(withCode("BRAND_NEW")), [{ path: "ui/src/apiError.ts", text: "t(`apiError.${error.code}`, { defaultValue: error.code })" }]);
  assert.equal(got.inert, true, got.reason);
});

test("a key that merely starts like an existing one is not its variant", () => {
  const got = impact(both(withCode("IMAGE_INUSE_SOON")));
  assert.equal(got.inert, true, got.reason);
});
