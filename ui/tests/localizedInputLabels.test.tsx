import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import type { InputField } from "../src/inferenceApi.ts";
import { createI18n, dom } from "./helpers/dom.ts";

void dom;
const { cleanup, fireEvent, render, screen } = await import("@testing-library/react");
const { default: InputsForm } = await import("../src/pages/playground/InputsForm.tsx");
afterEach(() => cleanup());

for (const language of ["zh", "en"] as const) {
  test(`imported regional labels render in ${language} while request input names stay unchanged`, async () => {
    const { i18n } = await createI18n(["playground"], language);
    const fields = JSON.parse(`[
      {"name":"ref_audio","kind":"audio","encoding":"data_url","label":{"en":"Reference audio","zh-CN":"参考音频"}},
      {"name":"ref_text","kind":"text","label":{"en":"Reference text","zh_TW":"參考文字"}},
      {"name":"custom_note","kind":"text"}
    ]`) as InputField[];
    const changes: Record<string, unknown> = {};
    render(<I18nextProvider i18n={i18n}><InputsForm fields={fields} values={{}} onChange={(name, value) => { changes[name] = value; }} /></I18nextProvider>);
    assert.equal((screen.getByLabelText(language === "zh" ? "参考音频" : "Reference audio") as HTMLInputElement).type, "file");
    const transcript = screen.getByRole("textbox", { name: language === "zh" ? "參考文字" : "Reference text" });
    fireEvent.change(transcript, { target: { value: "Reference transcript" } });
    assert.deepEqual(changes, { ref_text: "Reference transcript" });
    assert.ok(screen.getByRole("textbox", { name: "custom_note" }));
  });
}
