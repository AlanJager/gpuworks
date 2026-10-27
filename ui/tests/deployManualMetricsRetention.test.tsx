import { dom, createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import type { EngineProfile, EvaluateResponse } from "../src/api.ts";

const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { Form } = await import("antd");
const { api } = await import("../src/api.ts");
const useEvaluation = (await import("../src/pages/deploy/useDeployEvaluation.ts")).default;
const RuntimeCommand = (await import("../src/pages/deploy/DeployRuntimeCommand.tsx")).default;
const FeatureChoice = (await import("../src/pages/deploy/DeployFeatureChoice.tsx")).default;
const { EngineDecidesContext, effectiveEngineDecidesParamKeys } = await import("../src/pages/deploy/deployParamApplicability.ts");
const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");
const { i18n, locale } = await createI18n(["deploy"]);
const profiles = JSON.parse(fs.readFileSync(new URL("./fixtures/engines.json", import.meta.url), "utf8")) as EngineProfile[];
const vllm = profiles.find(profile => profile.name === "vllm")!;
const profile = { ...vllm, param_specs: vllm.param_specs?.filter(spec => ["enable_request_metrics", "max_model_len", "dtype"].includes(spec.key)) };
const originalEvaluate = api.evaluate;
let form: import("antd").FormInstance;
function Host({ metrics }: { metrics?: false }) {
  [form] = Form.useForm();
  const values = Form.useWatch([], { form, preserve: true }) || {};
  const evaluation = useEvaluation({
    form, signal: values,
    buildPayload: () => form.getFieldValue("engine") ? buildPayload(form.getFieldsValue(true), profile).payload : null,
    translate: (key, options) => i18n.t(key, options),
  });
  return <EngineDecidesContext.Provider value={effectiveEngineDecidesParamKeys(evaluation.result)}>
    <Form form={form} initialValues={{ engine: "vllm", enable_request_metrics: metrics }}>
      <FeatureChoice spec={profile.param_specs!.find(spec => spec.key === "enable_request_metrics")!} disabled={values.command_mode === "manual"} />
      <RuntimeCommand result={evaluation.result} error={evaluation.error} loading={evaluation.evaluating} />
      <output data-testid="evaluation">{JSON.stringify({ pending: evaluation.evaluating, result: evaluation.result, error: evaluation.error })}</output>
    </Form>
  </EngineDecidesContext.Provider>;
}
afterEach(() => { cleanup(); api.evaluate = originalEvaluate; });
after(() => dom.window.close());
const state = () => JSON.parse(screen.getByTestId("evaluation").textContent!);
const launch = { ok: true, entrypoint: ["vllm", "serve"], argv: ["/model"] };

for (const metrics of [undefined, false] as const) {
  test(`manual evaluation and explicit discard preserve request metrics ${metrics === undefined ? "unset" : "false"}`, async () => {
    // Replays the observed structured/manual response difference in readiness
    // 20261026T150253Z-ccfd126f. The server is not under test; the real hook's
    // request -> response -> Form boundary and UI controls are.
    const requests: Record<string, any>[] = [];
    let sawManual = false;
    api.evaluate = async payload => {
      const request = structuredClone(payload) as Record<string, any>;
      requests.push(request);
      if (request.container_overrides?.command_mode === "manual") {
        sawManual = true;
        return { ...launch, argv: ["manual-preview"], form_values: { enable_request_metrics: true } } as EvaluateResponse;
      }
      return {
        ...launch,
        engine_decides_param_keys: ["enable_request_metrics"],
        form_values: {
          ...(sawManual ? { dtype: "float16" } : { max_model_len: 4096 }),
          ...(metrics === false ? { enable_request_metrics: false } : {}),
        },
      } as EvaluateResponse;
    };
    const user = userEvent.setup();
    render(<I18nextProvider i18n={i18n}><Host metrics={metrics} /></I18nextProvider>);
    await waitFor(() => {
      assert.equal(state().pending, false);
      assert.equal(form.getFieldValue("max_model_len"), 4096);
    });
    const before = buildPayload(form.getFieldsValue(true), profile).payload;
    assert.equal((before.quick_params as Record<string, unknown>).enable_request_metrics, metrics);
    const labels = locale.deploy.runtimeCommand;
    await user.click(screen.getByRole("button", { name: new RegExp(labels.edit) }));
    await user.click(screen.getByRole("button", { name: labels.enter, exact: true }));
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "vllm serve /model\n# retained draft edit" } });
    await waitFor(() => {
      assert.equal(state().pending, false);
      assert.deepEqual(state().result.argv, ["manual-preview"], "manual preview still updates");
    });
    const manualMetrics = form.getFieldValue("enable_request_metrics");
    const manualLength = form.getFieldValue("max_model_len");
    const returnStart = requests.length;
    await user.click(screen.getByRole("button", { name: new RegExp(labels.return) }));
    await user.click(screen.getByRole("button", { name: labels.discardAndReturn, exact: true }));
    await waitFor(() => assert.ok(requests.slice(returnStart).some(request => request.container_overrides?.command_mode !== "manual")));
    const returned = requests.slice(returnStart).find(request => request.container_overrides?.command_mode !== "manual")!;
    assert.deepEqual(returned, before, "explicit discard preserves the original structured request, including unset/false metrics");
    assert.equal(manualMetrics, metrics, "manual evaluation must not change structured metrics intent");
    assert.equal(manualLength, 4096, "manual evaluation must not clear structured values");
    await waitFor(() => {
      assert.equal(state().pending, false);
      assert.equal(form.getFieldValue("dtype"), "float16", "structured response still applies after return");
      assert.equal(form.getFieldValue("max_model_len"), undefined, "last structured response keys survive manual mode for stale-key clearing");
    });
    assert.equal(screen.queryByRole("textbox"), null);
  });
}
