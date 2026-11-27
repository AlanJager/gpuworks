import React from "react";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { createI18n } from "./helpers/dom.ts";
import { I18nextProvider } from "react-i18next";
import en from "../src/i18n/locales/en/common.json";

const { cleanup, render, screen } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: LanguageSwitcher } = await import("../src/components/LanguageSwitcher.tsx");
const { FeedbackHost, message } = await import("../src/ui-kit/feedback");

afterEach(() => {
  cleanup();
  message.destroy();
});

for (const light of [false, true]) {
  test(`language menu switches both directions with translated button labels (light=${light})`, async () => {
    const { i18n, locale } = await createI18n([], "zh");
    i18n.addResourceBundle("en", "common", en);
    const user = userEvent.setup();
    render(<I18nextProvider i18n={i18n}><LanguageSwitcher light={light} /></I18nextProvider>);
    await user.click(screen.getByRole("button", { name: locale.common.language.switch }));
    await user.click(await screen.findByRole("menuitemradio", { name: "English" }));
    const english = await screen.findByRole("button", { name: en.language.switch });
    assert.equal(english.textContent, "EN");
    assert.equal(i18n.resolvedLanguage, "en");
    await user.click(english);
    await user.click(await screen.findByRole("menuitemradio", { name: "简体中文" }));
    const chinese = await screen.findByRole("button", { name: locale.common.language.switch });
    assert.equal(chinese.textContent, "中文");
    assert.equal(i18n.resolvedLanguage, "zh");
  });
}

test("the language switches only after its messages have loaded", async () => {
  const { i18n, locale } = await createI18n([], "zh");
  i18n.addResourceBundle("en", "common", en);
  let finish: () => void = () => {};
  const asked: string[] = [];
  const loadLanguage = (language: string) => {
    asked.push(language);
    return new Promise<void>((resolve) => { finish = resolve; });
  };
  const user = userEvent.setup();
  render(<I18nextProvider i18n={i18n}><LanguageSwitcher loadLanguage={loadLanguage} /></I18nextProvider>);
  await user.click(screen.getByRole("button", { name: locale.common.language.switch }));
  await user.click(await screen.findByRole("menuitemradio", { name: "English" }));
  assert.deepEqual(asked, ["en"]);
  assert.equal(i18n.resolvedLanguage, "zh", "the console stays in its language while the messages load");
  finish();
  const english = await screen.findByRole("button", { name: en.language.switch });
  assert.equal(english.textContent, "EN");
  assert.equal(i18n.resolvedLanguage, "en");
});

test("a language that cannot load keeps the current language and says so", async () => {
  const { i18n, locale } = await createI18n(["common", "language"], "zh");
  i18n.addResourceBundle("en", "common", en);
  const loadLanguage = () => Promise.reject(new Error("Failed to fetch dynamically imported module"));
  const user = userEvent.setup();
  render(<I18nextProvider i18n={i18n}><FeedbackHost /><LanguageSwitcher loadLanguage={loadLanguage} /></I18nextProvider>);
  await user.click(screen.getByRole("button", { name: locale.common.language.switch }));
  await user.click(await screen.findByRole("menuitemradio", { name: "English" }));
  await screen.findByText(locale.language.loadFailed);
  assert.equal(i18n.resolvedLanguage, "zh");
  assert.equal(screen.getByRole("button", { name: locale.common.language.switch }).textContent, "中文");
});
