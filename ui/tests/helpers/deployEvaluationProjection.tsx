import React from "react";
import { dom } from "./dom.ts";

const { render, screen } = await import("@testing-library/react");
type FormInstance = import("antd").FormInstance;
const { Button, Form, Input, InputNumber, Switch } = await import("antd");
const { I18nextProvider } = await import("react-i18next");
const { api } = await import("../../src/api.ts");
const useDeployEvaluation = (
  await import("../../src/pages/deploy/useDeployEvaluation.ts")
).default;
const { buildPayload } = await import("../../src/pages/deploy/deployHelpers.ts");
const { i18n } = await (await import("./dom.ts")).createI18n(["deploy", "common"]);

export const flowProfile = {
  name: "vllm",
  quick_form_keys: [
    "dtype",
    "gpu_memory_utilization",
    "enable_prefix_caching",
    "cpu_offload_gb",
    "max_model_len",
  ],
  param_specs: [
    {
      key: "dtype",
      flag: "--dtype",
      type: "enum",
      control: "select",
      enum: ["auto", "bfloat16", "float16"],
      engines: ["vllm"],
    },
    {
      key: "gpu_memory_utilization",
      flag: "--gpu-memory-utilization",
      type: "number",
      control: "slider",
      default: 0.9,
      min: 0.7,
      max: 0.98,
      step: 0.01,
      engines: ["vllm"],
    },
    {
      key: "enable_prefix_caching",
      flag: "--enable-prefix-caching",
      flag_off: "--no-enable-prefix-caching",
      type: "boolean",
      control: "switch",
      default: true,
      engines: ["vllm"],
    },
    {
      key: "cpu_offload_gb",
      flag: "--cpu-offload-gb",
      type: "number",
      control: "number",
      min: 0,
      step: 1,
      engines: ["vllm"],
    },
    {
      key: "max_model_len",
      flag: "--max-model-len",
      type: "integer",
      control: "number",
      min: 1,
      engines: ["vllm"],
    },
  ],
} as never;

type HarnessContext = {
  form: FormInstance;
  evaluation: ReturnType<typeof useDeployEvaluation>;
};

let harnessContext: HarnessContext | undefined;

function templateIdentity(name: string) {
  return {
    template_id: 31,
    template_spec_source: "upstream",
    template_spec_name: name,
  };
}

function EvaluationHarness({
  initialValues = { model_id: 17, engine: "vllm", command_mode: "engine" },
  create,
}: {
  initialValues?: Record<string, unknown>;
  create?: (payload: Record<string, unknown>) => void;
}) {
  const [instance] = Form.useForm();
  const signal = Form.useWatch([], { form: instance, preserve: true });
  const evaluation = useDeployEvaluation({
    form: instance,
    signal,
    buildPayload: () =>
      buildPayload(instance.getFieldsValue(true), flowProfile).payload,
    translate: (key) => key,
  });
  harnessContext = { form: instance, evaluation };
  return (
    <Form
      form={instance}
      initialValues={initialValues}
      onValuesChange={(changed) => evaluation.noteDraftChanged(changed)}
    >
      <Form.Item name="dtype" label="dtype">
        <Input aria-label="dtype" />
      </Form.Item>
      <Form.Item name="gpu_memory_utilization" label="GPU memory">
        <InputNumber aria-label="GPU memory" />
      </Form.Item>
      <Form.Item name="enable_prefix_caching" label="Prefix caching" valuePropName="checked">
        <Switch aria-label="Prefix caching" />
      </Form.Item>
      <Form.Item name="cpu_offload_gb" label="CPU weight offload">
        <InputNumber aria-label="CPU weight offload" min={0} />
      </Form.Item>
      <Form.Item name="max_model_len" label="Maximum context">
        <InputNumber aria-label="Maximum context" min={1} />
      </Form.Item>
      <Form.Item name="extra_args" label="Custom arguments">
        <Input.TextArea aria-label="Custom arguments" />
      </Form.Item>
      <Button onClick={() => instance.setFieldsValue(templateIdentity("bf16"))}>
        Select BF16 template
      </Button>
      <Button onClick={() => instance.setFieldsValue(templateIdentity("fp16"))}>
        Select FP16 template
      </Button>
      {create && (
        <Button
          onClick={() =>
            create(buildPayload(instance.getFieldsValue(true), flowProfile).payload)
          }
        >
          Create snapshot
        </Button>
      )}
    </Form>
  );
}

export function mountEvaluationHarness(
  options: {
    initialValues?: Record<string, unknown>;
    create?: (payload: Record<string, unknown>) => void;
  } = {},
) {
  render(
    <I18nextProvider i18n={i18n}>
      <EvaluationHarness {...options} />
    </I18nextProvider>,
  );
  return () => harnessContext!;
}

export function installEvaluate(
  answer: (request: Record<string, unknown>) => Promise<unknown> | unknown,
) {
  const requests: Record<string, unknown>[] = [];
  api.evaluate = (async (payload: Record<string, unknown>) => {
    const request = structuredClone(payload);
    requests.push(request);
    return answer(request);
  }) as typeof api.evaluate;
  return requests;
}

export function quickParams(request: Record<string, unknown>) {
  return (request.quick_params ?? {}) as Record<string, unknown>;
}

export async function clickButton(name: string) {
  const user = (await import("@testing-library/user-event")).default.setup({
    document: dom.window.document,
  });
  await user.click(screen.getByRole("button", { name }));
}

export function getHarnessContext() { return harnessContext!; }
export function resetHarnessContext() { harnessContext = undefined; }
