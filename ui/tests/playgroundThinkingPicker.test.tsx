// The Playground page takes the extra-parameter presets from the selected
// model, because whether Disable Thinking can be honored depends on the
// deployment the server resolved (engine and model family). This test drives
// the page the way a user does: pick a model, open the parameters panel, read
// the preset button.
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { chatCatalog, dom, locale, originalFetch, renderChat, serveStreams } from "./helpers/playgroundChat.tsx";

const { cleanup, fireEvent, screen, waitFor } = await import("@testing-library/react");
const pane = locale.playground.pane;
const base = chatCatalog().models[0];

const lowEffort = { key: "low_reasoning_effort", label: { en: "Low Reasoning Effort", zh: "低推理强度" }, value: { reasoning_effort: "low" } };
const supported = {
  ...base, id: "deepseek-v4", label: "deepseek-v4",
  extra_parameter_presets: [
    lowEffort,
    { key: "disable_thinking", label: { en: "Disable Thinking", zh: "关闭思考" }, value: { chat_template_kwargs: { thinking: false } }, applies_when: "deepseek-v4" },
  ],
};
const unsupported = {
  ...base, id: "qwen2", label: "qwen2",
  extra_parameter_presets: [
    lowEffort,
    {
      key: "disable_thinking", label: { en: "Disable Thinking", zh: "关闭思考" },
      unavailable: { en: 'Add the parameter yourself under Extra Parameters, for example {"chat_template_kwargs":{"thinking":false}}.' },
    },
  ],
};

afterEach(() => {
  cleanup();
  dom.window.sessionStorage.clear();
  dom.window.localStorage.clear();
  globalThis.fetch = originalFetch;
});

async function openParameters() {
  fireEvent.click(screen.getByRole("button", { name: new RegExp(pane.parameters) }));
  await screen.findByText(locale.playground.parameters.extra);
}

async function chooseModel(label: string) {
  const selector = screen.getByRole("combobox", { name: pane.model }).closest(".ant-select")!;
  fireEvent.mouseDown(selector.querySelector(".ant-select-selector")!);
  await waitFor(() => assert.ok(document.querySelector(`.ant-select-dropdown [title="${label}"]`)));
  fireEvent.click(document.querySelector(`.ant-select-dropdown [title="${label}"]`)!);
}

test("the switch follows the selected model and writes that model's chat-template field", async () => {
  serveStreams([]);
  await renderChat(chatCatalog({ models: [supported, unsupported] }), { path: "/playground/chat?model=deepseek-v4" });
  await openParameters();

  const supportedButton = screen.getByRole("button", { name: "Disable Thinking" }) as HTMLButtonElement;
  assert.equal(supportedButton.disabled, false);
  fireEvent.click(supportedButton);
  const textarea = document.getElementById("playground-extra-parameters") as HTMLTextAreaElement;
  assert.deepEqual(JSON.parse(textarea.value), { chat_template_kwargs: { thinking: false } });

  // The same page, another model: the server declared no family for it, so the
  // button is disabled and explains what to add by hand instead of claiming
  // thinking cannot be stopped.
  await chooseModel("qwen2");
  const unsupportedButton = await waitFor(() => {
    const button = screen.getByRole("button", { name: /Disable Thinking/ }) as HTMLButtonElement;
    assert.equal(button.disabled, true);
    return button;
  });
  assert.match(unsupportedButton.getAttribute("aria-label") || "", /"thinking":false/);
  assert.doesNotMatch(unsupportedButton.getAttribute("aria-label") || "", /cannot stop|will not stop|would not stop/i);

  // Switching back restores the model that can honor it, with its own patch.
  await chooseModel("deepseek-v4");
  await waitFor(() => assert.equal((screen.getByRole("button", { name: "Disable Thinking" }) as HTMLButtonElement).disabled, false));
});
