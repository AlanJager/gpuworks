import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React, { useState } from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import type { ExtraParameterPreset, ParameterState } from "../src/pages/playground/types.ts";

const { i18n } = await createI18n(["playground"]);
const { cleanup, render, screen } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { buildChatBody } = await import("../src/pages/playground/api.ts");
const { initialParameterState } = await import("../src/pages/playground/ParametersPanel.tsx");
const ParametersPanel = (await import("../src/pages/playground/ParametersPanel.tsx")).default;

afterEach(() => cleanup());

// The server owns which chat-template field stops thinking, so the panel only
// reads the preset it is given: a value means the preset can be honored for the
// selected deployment, its absence means it cannot.
const lowEffort: ExtraParameterPreset = {
  key: "low_reasoning_effort",
  label: { en: "Low Reasoning Effort", zh: "低推理强度" },
  value: { reasoning_effort: "low" },
};
const qwenDisable: ExtraParameterPreset = {
  key: "disable_thinking",
  label: { en: "Disable Thinking", zh: "关闭思考" },
  applies_when: "qwen3",
  value: { chat_template_kwargs: { enable_thinking: false } },
};
const deepseekDisable: ExtraParameterPreset = {
  key: "disable_thinking",
  label: { en: "Disable Thinking", zh: "关闭思考" },
  applies_when: "deepseek-v4",
  value: { chat_template_kwargs: { thinking: false } },
};
// What the server serves for a model no declared family covers: the same
// button, no value, and the parameters the reader can add by hand.
const unavailableText = 'Add the parameter yourself under Extra Parameters, for example {"chat_template_kwargs":{"enable_thinking":false}}, {"chat_template_kwargs":{"thinking":false}}.';
const unsupportedDisable: ExtraParameterPreset = {
  key: "disable_thinking",
  label: { en: "Disable Thinking", zh: "关闭思考" },
  unavailable: { en: unavailableText, zh: "请在「额外参数」中自行加入。" },
};

function Harness({ presets, extraJSON = "", model = "served-model" }: { presets: ExtraParameterPreset[]; extraJSON?: string; model?: string }) {
  const [value, setValue] = useState<ParameterState>({ ...initialParameterState([]), extraJSON });
  let request = "";
  try {
    request = JSON.stringify(buildChatBody(model, [{ role: "user", content: "hi" }], value, []));
  } catch (error) {
    request = (error as Error).message;
  }
  return (
    <I18nextProvider i18n={i18n}>
      <ParametersPanel definitions={[]} presets={presets} value={value} onChange={setValue} />
      <pre data-testid="request">{request}</pre>
    </I18nextProvider>
  );
}

test("a declared family enables the switch and its preset carries the family key", async () => {
  for (const [preset, expected] of [[qwenDisable, { enable_thinking: false }], [deepseekDisable, { thinking: false }]] as const) {
    const user = userEvent.setup();
    const { unmount } = render(<Harness presets={[lowEffort, preset]} extraJSON={'{\n  "chat_template_kwargs": {"existing": true}\n}'} />);
    const button = screen.getByRole("button", { name: "Disable Thinking" }) as HTMLButtonElement;
    assert.equal(button.disabled, false);
    assert.equal(button.getAttribute("aria-label"), "Disable Thinking");
    assert.equal(button.getAttribute("title"), null);
    await user.click(button);
    const body = JSON.parse(screen.getByTestId("request").textContent || "") as Record<string, unknown>;
    // The switch is inside chat_template_kwargs (a top-level field is accepted
    // and ignored by vLLM) and the sibling template kwargs survive the merge.
    assert.deepEqual(body.chat_template_kwargs, { existing: true, ...expected });
    for (const key of Object.keys(expected)) assert.equal(body[key], undefined);
    unmount();
    cleanup();
  }
});

test("a model the server cannot cover keeps the switch off and names the parameter to add", async () => {
  const user = userEvent.setup();
  render(<Harness presets={[lowEffort, unsupportedDisable]} extraJSON='{"seed": 1}' />);
  const button = screen.getByRole("button", { name: /Disable Thinking/ }) as HTMLButtonElement;
  assert.equal(button.disabled, true);
  const label = button.getAttribute("aria-label") || "";
  // The text must not claim thinking cannot be stopped: it names both
  // chat-template parameters a user can send by hand.
  assert.match(label, /chat_template_kwargs/);
  assert.match(label, /"enable_thinking":false/);
  assert.match(label, /"thinking":false/);
  assert.doesNotMatch(label, /w(ill|ould) not stop|cannot/i);
  await user.click(button);
  const body = JSON.parse(screen.getByTestId("request").textContent || "") as Record<string, unknown>;
  assert.equal(body.chat_template_kwargs, undefined);
  assert.equal(body.seed, 1);
  assert.equal((document.getElementById("playground-extra-parameters") as HTMLTextAreaElement).value, '{"seed": 1}');
  // A preset that is a plain example is still applicable.
  assert.equal((screen.getByRole("button", { name: "Low Reasoning Effort" }) as HTMLButtonElement).disabled, false);
});

test("an invalid extra parameter document is left unchanged", async () => {
  const user = userEvent.setup();
  render(<Harness presets={[qwenDisable]} extraJSON="[1, 2]" />);
  await user.click(screen.getByRole("button", { name: "Disable Thinking" }));
  assert.match(screen.getByRole("alert").textContent || "", /JSON object/);
  assert.equal((document.getElementById("playground-extra-parameters") as HTMLTextAreaElement).value, "[1, 2]");
});
