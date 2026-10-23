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

const disableThinking: ExtraParameterPreset = {
  key: "disable_thinking",
  label: { en: "Disable Thinking", zh: "关闭思考" },
  applies_when: "qwen3",
  unavailable: {
    en: "Only a Qwen3 chat model on vLLM or SGLang honors this switch.",
    zh: "只有 vLLM 或 SGLang 上的 Qwen3 对话模型会响应这个开关。",
  },
  value: { chat_template_kwargs: { enable_thinking: false } },
};
const lowEffort: ExtraParameterPreset = {
  key: "low_reasoning_effort",
  label: { en: "Low Reasoning Effort", zh: "低推理强度" },
  value: { reasoning_effort: "low" },
};

function Harness({ thinkingToggle, extraJSON = "" }: { thinkingToggle: boolean; extraJSON?: string }) {
  const [value, setValue] = useState<ParameterState>({ ...initialParameterState([]), extraJSON });
  let request = "";
  try {
    request = JSON.stringify(buildChatBody("Qwen/Qwen3-0.6B", [{ role: "user", content: "hi" }], value, []));
  } catch (error) {
    request = (error as Error).message;
  }
  return (
    <I18nextProvider i18n={i18n}>
      <ParametersPanel
        definitions={[]}
        presets={[lowEffort, disableThinking]}
        thinkingToggle={thinkingToggle}
        value={value}
        onChange={setValue}
      />
      <pre data-testid="request">{request}</pre>
    </I18nextProvider>
  );
}

test("disable thinking merges chat_template_kwargs and keeps the other extra parameters", async () => {
  const user = userEvent.setup();
  render(<Harness thinkingToggle extraJSON={'{\n  "reasoning_effort": "low",\n  "chat_template_kwargs": {"existing": true}\n}'} />);
  await user.click(screen.getByRole("button", { name: "Low Reasoning Effort" }));
  await user.click(screen.getByRole("button", { name: "Disable Thinking" }));
  const body = JSON.parse(screen.getByTestId("request").textContent || "") as Record<string, unknown>;
  assert.equal(body.enable_thinking, undefined);
  assert.equal(body.reasoning_effort, "low");
  assert.deepEqual(body.chat_template_kwargs, { existing: true, enable_thinking: false });
  assert.equal(body.model, "Qwen/Qwen3-0.6B");
  assert.equal(body.stream, true);
});

test("disable thinking stays off for a model that does not honor the switch", async () => {
  const user = userEvent.setup();
  render(<Harness thinkingToggle={false} extraJSON='{"seed": 1}' />);
  const button = screen.getByRole("button", { name: /Disable Thinking/ }) as HTMLButtonElement;
  assert.equal(button.disabled, true);
  assert.match(button.getAttribute("aria-label") || "", /vLLM or SGLang/);
  await user.click(button);
  const body = JSON.parse(screen.getByTestId("request").textContent || "") as Record<string, unknown>;
  assert.equal(body.chat_template_kwargs, undefined);
  assert.equal(body.enable_thinking, undefined);
  assert.equal(body.seed, 1);
  assert.equal((document.getElementById("playground-extra-parameters") as HTMLTextAreaElement).value, '{"seed": 1}');
});

test("an invalid extra parameter document is left unchanged", async () => {
  const user = userEvent.setup();
  render(<Harness thinkingToggle extraJSON="[1, 2]" />);
  await user.click(screen.getByRole("button", { name: "Disable Thinking" }));
  assert.match(screen.getByRole("alert").textContent || "", /JSON object/);
  assert.equal((document.getElementById("playground-extra-parameters") as HTMLTextAreaElement).value, "[1, 2]");
});
