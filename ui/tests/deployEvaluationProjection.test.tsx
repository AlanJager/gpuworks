import { createI18n, dom } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";

const { act, cleanup, render, screen, waitFor } =
  await import("@testing-library/react");
const { Button, Form, Input, InputNumber } = await import("antd");
const { I18nextProvider } = await import("react-i18next");
type FormInstance = import("antd").FormInstance;
const { api } = await import("../src/api.ts");
const useDeployEvaluation = (
  await import("../src/pages/deploy/useDeployEvaluation.ts")
).default;
const { buildPayload } = await import(
  "../src/pages/deploy/deployHelpers.ts"
);
const useDeploySubmit = (
  await import("../src/pages/deploy/useDeploySubmit.ts")
).default;
const { i18n } = await createI18n(["deploy", "common"]);

const originalEvaluate = api.evaluate;
const originalCreateDeployment = api.createDeployment;
const profile = {
  name: "vllm",
  quick_form_keys: ["dtype", "gpu_memory_utilization"],
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
  ],
} as never;

const {
  flowProfile,
  mountEvaluationHarness,
  installEvaluate,
  quickParams,
  clickButton,
  getHarnessContext,
  resetHarnessContext,
} = await import("./helpers/deployEvaluationProjection.tsx");

afterEach(() => {
  cleanup();
  resetHarnessContext();
  api.evaluate = originalEvaluate;
  api.createDeployment = originalCreateDeployment;
});
after(() => dom.window.close());

test("template evaluation does not inherit server display defaults as user quick params", async () => {
  const requests: Record<string, unknown>[] = [];
  api.evaluate = (async (payload: Record<string, unknown>) => {
    const request = structuredClone(payload);
    requests.push(request);
    const quick = (request.quick_params ?? {}) as Record<string, unknown>;
    if (request.template_spec_name !== "bf16") {
      return {
        ok: true,
        form_values: {
          dtype: "auto",
          gpu_memory_utilization: 0.9,
        },
      } as never;
    }
    // Match the server's precedence: a user quick param overrides the
    // selected template, while an unstated key comes from the template.
    return {
      ok: true,
      form_values: {
        dtype: Object.hasOwn(quick, "dtype") ? quick.dtype : "bfloat16",
        gpu_memory_utilization: Object.hasOwn(
          quick,
          "gpu_memory_utilization",
        )
          ? quick.gpu_memory_utilization
          : 0.75,
      },
    } as never;
  }) as typeof api.evaluate;

  let form: FormInstance | undefined;
  function Host() {
    const [instance] = Form.useForm();
    form = instance;
    const signal = Form.useWatch([], { form: instance, preserve: true });
    const evaluation = useDeployEvaluation({
      form: instance,
      signal,
      buildPayload: () =>
        buildPayload(instance.getFieldsValue(true), profile).payload,
      translate: (key) => key,
    });
    return (
      <Form
        form={instance}
        initialValues={{ model_id: 17, engine: "vllm", command_mode: "engine" }}
        onValuesChange={(changed) => evaluation.noteDraftChanged(changed)}
      >
        <Form.Item name="dtype" label="dtype">
          <Input aria-label="dtype" />
        </Form.Item>
        <Form.Item name="gpu_memory_utilization" label="GPU memory">
          <Input aria-label="GPU memory" />
        </Form.Item>
        <Button
          onClick={() =>
            instance.setFieldsValue({
              template_id: 31,
              template_spec_source: "upstream",
              template_spec_name: "bf16",
            })
          }
        >
          Select BF16 template
        </Button>
      </Form>
    );
  }

  render(<Host />);
  await waitFor(() => {
    assert.equal(
      (screen.getByLabelText("dtype") as HTMLInputElement).value,
      "auto",
    );
    assert.equal(
      (screen.getByLabelText("GPU memory") as HTMLInputElement).value,
      "0.9",
    );
    assert.ok(requests.length >= 1);
  });
  assert.deepEqual(
    requests[0].quick_params,
    undefined,
    "the initial draft leaves both parameters to the server",
  );

  await act(async () => {
    screen.getByRole("button", { name: "Select BF16 template" }).click();
  });
  await waitFor(() =>
    assert.ok(
      requests.some((request) => request.template_spec_name === "bf16"),
    ),
  );
  const templateRequest = requests.find(
    (request) => request.template_spec_name === "bf16",
  )!;
  const templateQuick = (templateRequest.quick_params ?? {}) as Record<
    string,
    unknown
  >;
  assert.equal(
    Object.hasOwn(templateQuick, "dtype"),
    false,
    "server-displayed dtype=auto is not reclassified as an explicit user choice",
  );
  assert.equal(
    Object.hasOwn(templateQuick, "gpu_memory_utilization"),
    false,
    "server-displayed memory=0.9 is not reclassified as an explicit user choice",
  );
  await waitFor(() => {
    assert.equal(
      (screen.getByLabelText("dtype") as HTMLInputElement).value,
      "bfloat16",
    );
    assert.equal(
      (screen.getByLabelText("GPU memory") as HTMLInputElement).value,
      "0.75",
    );
  });
  assert.equal(form?.getFieldValue("template_spec_name"), "bf16");
});

test("explicit user false and zero survive template projection", async () => {
  const requests = installEvaluate((request) => {
    const quick = quickParams(request);
    return {
      ok: true,
      form_values: {
        dtype: Object.hasOwn(quick, "dtype") ? quick.dtype : "auto",
        gpu_memory_utilization: Object.hasOwn(
          quick,
          "gpu_memory_utilization",
        )
          ? quick.gpu_memory_utilization
          : 0.9,
        enable_prefix_caching: Object.hasOwn(
          quick,
          "enable_prefix_caching",
        )
          ? quick.enable_prefix_caching
          : true,
        cpu_offload_gb: Object.hasOwn(quick, "cpu_offload_gb")
          ? quick.cpu_offload_gb
          : 4,
      },
    } as never;
  });
  mountEvaluationHarness();
  const user = (await import("@testing-library/user-event")).default.setup({
    document: dom.window.document,
  });
  await waitFor(() => {
    assert.equal(
      (screen.getByLabelText("GPU memory") as HTMLInputElement).value,
      "0.9",
    );
    assert.equal(
      screen.getByRole("switch", { name: "Prefix caching" }).getAttribute("aria-checked"),
      "true",
    );
    assert.equal(
      (screen.getByRole("spinbutton", { name: "CPU weight offload" }) as HTMLInputElement)
        .value,
      "4",
    );
  });

  await user.click(screen.getByRole("switch", { name: "Prefix caching" }));
  const cpuOffload = screen.getByRole("spinbutton", {
    name: "CPU weight offload",
  });
  await user.clear(cpuOffload);
  await user.type(cpuOffload, "0");
  await waitFor(() => {
    const last = requests.at(-1)!;
    assert.equal(quickParams(last).enable_prefix_caching, false);
    assert.equal(quickParams(last).cpu_offload_gb, 0);
  });

  await user.click(screen.getByRole("button", { name: "Select BF16 template" }));
  await waitFor(() =>
    assert.ok(requests.some((request) => request.template_spec_name === "bf16")),
  );
  const templateQuick = quickParams(
    requests.find((request) => request.template_spec_name === "bf16")!,
  );
  assert.equal(templateQuick.enable_prefix_caching, false);
  assert.equal(templateQuick.cpu_offload_gb, 0);
  assert.equal(Object.hasOwn(templateQuick, "dtype"), false);
  assert.equal(Object.hasOwn(templateQuick, "gpu_memory_utilization"), false);
});

test("a programmatic value change is projected while unchanged evaluation values stay inherited", async () => {
  const requests = installEvaluate((request) => {
    const quick = quickParams(request);
    return {
      ok: true,
      form_values: {
        dtype: Object.hasOwn(quick, "dtype") ? quick.dtype : "auto",
        gpu_memory_utilization: Object.hasOwn(
          quick,
          "gpu_memory_utilization",
        )
          ? quick.gpu_memory_utilization
          : 0.9,
      },
    } as never;
  });
  mountEvaluationHarness();
  const current = getHarnessContext;
  await waitFor(() =>
    assert.equal(
      (screen.getByLabelText("GPU memory") as HTMLInputElement).value,
      "0.9",
    ),
  );

  act(() => current().form.setFieldValue("gpu_memory_utilization", 0.84));
  await waitFor(() =>
    assert.equal(quickParams(requests.at(-1)!).gpu_memory_utilization, 0.84),
  );
  act(() => {
    current().evaluation.noteDraftChanged({ dtype: "auto" });
    current().form.setFieldValue("dtype", "auto");
  });
  await clickButton("Select BF16 template");
  await waitFor(() =>
    assert.ok(requests.some((request) => request.template_spec_name === "bf16")),
  );
  const templateQuick = quickParams(
    requests.find((request) => request.template_spec_name === "bf16")!,
  );
  assert.equal(templateQuick.gpu_memory_utilization, 0.84);
  assert.equal(templateQuick.dtype, "auto", "same-value explicit edits remain intentional");
});

test("an explicit quick-param unset stays empty after the next evaluation reply", async () => {
  const requests = installEvaluate((request) => {
    const unset = (request.quick_params_unset ?? []) as string[];
    if (unset.includes("dtype")) {
      return {
        ok: true,
        form_values: { gpu_memory_utilization: 0.9 },
      } as never;
    }
    return {
      ok: true,
      form_values: { dtype: "auto", gpu_memory_utilization: 0.9 },
    } as never;
  });
  mountEvaluationHarness();
  const current = getHarnessContext;
  const user = (await import("@testing-library/user-event")).default.setup({
    document: dom.window.document,
  });
  await waitFor(() =>
    assert.equal(
      (screen.getByLabelText("dtype") as HTMLInputElement).value,
      "auto",
    ),
  );
  await user.clear(screen.getByLabelText("dtype"));
  await waitFor(() => {
    const last = requests.at(-1)!;
    assert.ok(
      ((last.quick_params_unset ?? []) as string[]).includes("dtype"),
    );
    assert.equal(
      (screen.getByLabelText("dtype") as HTMLInputElement).value,
      "",
    );
  });
  await waitFor(() => assert.equal(current().evaluation.evaluating, false));
  await clickButton("Select BF16 template");
  await waitFor(() => {
    const templateRequest = requests.find((request) => request.template_spec_name === "bf16");
    assert.ok(templateRequest);
    assert.ok(((templateRequest.quick_params_unset ?? []) as string[]).includes("dtype"));
    assert.equal((screen.getByLabelText("dtype") as HTMLInputElement).value, "");
  });
});

test("editing residual args preserves a parameter already moved into its field", async () => {
  const requests = installEvaluate((request) => {
    const args = String(request.extra_args ?? "");
    const quick = quickParams(request);
    if (args.includes("--max-model-len 8192")) {
      return {
        ok: true,
        moved: [
          {
            key: "max_model_len",
            flag: "--max-model-len",
            value: 8192,
            source: "extra_args",
          },
        ],
        form_values: { max_model_len: 8192, extra_args: "--foo" },
      } as never;
    }
    return {
      ok: true,
      form_values: {
        max_model_len: Object.hasOwn(quick, "max_model_len")
          ? quick.max_model_len
          : 8192,
        extra_args: args,
      },
    } as never;
  });
  mountEvaluationHarness({
    initialValues: {
      model_id: 17,
      engine: "vllm",
      command_mode: "engine",
      extra_args: "--max-model-len 8192 --foo",
    },
  });
  const user = (await import("@testing-library/user-event")).default.setup({
    document: dom.window.document,
  });
  await waitFor(() => {
    assert.equal(
      (screen.getByRole("spinbutton", { name: "Maximum context" }) as HTMLInputElement)
        .value,
      "8192",
    );
    assert.equal(
      (screen.getByRole("textbox", { name: "Custom arguments" }) as HTMLTextAreaElement)
        .value,
      "--foo",
    );
  });
  const args = screen.getByRole("textbox", { name: "Custom arguments" });
  await user.type(args, " --bar");
  await waitFor(() => {
    const last = requests.at(-1)!;
    assert.equal(last.extra_args, "--foo --bar");
    assert.equal(quickParams(last).max_model_len, 8192);
  });
});

test("a response for an earlier template cannot overwrite the latest form", async () => {
  type Pending = {
    request: Record<string, unknown>;
    resolve: (response: unknown) => void;
  };
  const requests: Record<string, unknown>[] = [];
  const pendingBySpec = new Map<string, Pending[]>();
  const deferredCount = new Map<string, number>();
  api.evaluate = (async (payload: Record<string, unknown>) => {
    const request = structuredClone(payload);
    requests.push(request);
    const spec = String(request.template_spec_name ?? "");
    if (!spec) {
      return {
        ok: true,
        form_values: { dtype: "auto", gpu_memory_utilization: 0.9 },
      } as never;
    }
    const count = (deferredCount.get(spec) ?? 0) + 1;
    deferredCount.set(spec, count);
    if (count > 1) {
      const selected = spec === "bf16"
        ? { dtype: "bfloat16", gpu_memory_utilization: 0.75 }
        : { dtype: "float16", gpu_memory_utilization: 0.8 };
      return { ok: true, form_values: selected } as never;
    }
    return new Promise((resolve) => {
      const pending = pendingBySpec.get(spec) ?? [];
      pending.push({ request, resolve });
      pendingBySpec.set(spec, pending);
    });
  }) as typeof api.evaluate;
  mountEvaluationHarness();
  const user = (await import("@testing-library/user-event")).default.setup({
    document: dom.window.document,
  });
  await waitFor(() =>
    assert.equal(
      (screen.getByLabelText("dtype") as HTMLInputElement).value,
      "auto",
    ),
  );

  await user.click(screen.getByRole("button", { name: "Select BF16 template" }));
  await waitFor(() => assert.equal(pendingBySpec.get("bf16")?.length, 1));
  await user.click(screen.getByRole("button", { name: "Select FP16 template" }));
  await waitFor(() => assert.equal(pendingBySpec.get("fp16")?.length, 1));

  await act(async () => {
    pendingBySpec.get("fp16")![0].resolve({
      ok: true,
      form_values: { dtype: "float16", gpu_memory_utilization: 0.8 },
    });
  });
  await waitFor(() => {
    assert.equal(
      (screen.getByLabelText("dtype") as HTMLInputElement).value,
      "float16",
    );
    assert.equal(
      (screen.getByLabelText("GPU memory") as HTMLInputElement).value,
      "0.8",
    );
  });
  await waitFor(() =>
    assert.equal(
      requests.filter((request) => request.template_spec_name === "fp16").length,
      2,
    ),
  );
  await act(async () => {
    pendingBySpec.get("bf16")![0].resolve({
      ok: true,
      form_values: { dtype: "bfloat16", gpu_memory_utilization: 0.75 },
    });
  });
  assert.equal(
    (screen.getByLabelText("dtype") as HTMLInputElement).value,
    "float16",
  );
  assert.equal(
    (screen.getByLabelText("GPU memory") as HTMLInputElement).value,
    "0.8",
  );
  assert.equal(
    requests.filter((request) => request.template_spec_name === "fp16").length,
    2,
    "the winning display may cause one echo evaluation, which also settles",
  );
});

test("create serializes all effective values displayed by evaluate", async () => {
  const requests = installEvaluate(() => ({
    ok: true,
    effective_params: {},
    image: "vllm:test",
    form_values: { dtype: "auto", gpu_memory_utilization: 0.9 },
  }));
  const created: Record<string, unknown>[] = [];
  api.createDeployment = (async (payload: Record<string, unknown>) => {
    created.push(structuredClone(payload));
    return { id: 91 } as never;
  }) as typeof api.createDeployment;

  function CreateHarness() {
    const [instance] = Form.useForm();
    const signal = Form.useWatch([], { form: instance, preserve: true });
    const evaluation = useDeployEvaluation({
      form: instance,
      signal,
      buildPayload: () =>
        buildPayload(instance.getFieldsValue(true), flowProfile).payload,
      translate: (key) => key,
    });
    const { submit } = useDeploySubmit({
      form: instance,
      nodes: [{ uuid: "node-1" }] as never,
      engineProfile: flowProfile,
      selectedModel: { id: 17, input_modalities: [] } as never,
      evalResult: evaluation.result,
      evalErr: evaluation.error,
      evaluating: evaluation.evaluating,
      onCreated: () => {},
    });
    return (
      <Form
        form={instance}
        initialValues={{
          model_id: 17,
          name: "create-projection",
          engine: "vllm",
          command_mode: "engine",
          node_selection_mode: "manual",
          node_uuid: "node-1",
        }}
        onValuesChange={(changed) => evaluation.noteDraftChanged(changed)}
      >
        <Form.Item name="dtype" label="dtype">
          <Input aria-label="dtype" />
        </Form.Item>
        <Form.Item name="gpu_memory_utilization" label="GPU memory">
          <InputNumber aria-label="GPU memory" />
        </Form.Item>
        <Button disabled={evaluation.evaluating} onClick={() => void submit()}>Create deployment</Button>
      </Form>
    );
  }
  render(
    <I18nextProvider i18n={i18n}>
      <CreateHarness />
    </I18nextProvider>,
  );
  await waitFor(() => {
    assert.equal(
      (screen.getByLabelText("dtype") as HTMLInputElement).value,
      "auto",
    );
    assert.equal(
      (screen.getByLabelText("GPU memory") as HTMLInputElement).value,
      "0.9",
    );
    assert.ok(requests.length >= 1);
  });
  const user = (await import("@testing-library/user-event")).default.setup({
    document: dom.window.document,
  });
  await waitFor(() => assert.equal(
    (screen.getByRole("button", { name: "Create deployment" }) as HTMLButtonElement).disabled,
    false,
  ));
  await user.click(screen.getByRole("button", { name: "Create deployment" }));
  await waitFor(() => assert.equal(created.length, 1));
  assert.deepEqual(created[0].quick_params, {
    dtype: "auto",
    gpu_memory_utilization: 0.9,
  });
});
