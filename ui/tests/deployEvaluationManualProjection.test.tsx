import { dom } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";

const { act, cleanup, screen, waitFor } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const {
  getHarnessContext,
  installEvaluate,
  mountEvaluationHarness,
  quickParams,
  resetHarnessContext,
} = await import("./helpers/deployEvaluationProjection.tsx");

const originalEvaluate = api.evaluate;
afterEach(() => {
  cleanup();
  resetHarnessContext();
  api.evaluate = originalEvaluate;
});
after(() => dom.window.close());

test("manual preview values do not become structured quick-param overrides", async () => {
  const requests = installEvaluate((request) => {
    const overrides = request.container_overrides as
      | { command_mode?: string }
      | undefined;
    if (overrides?.command_mode === "manual") {
      return {
        ok: true,
        form_values: { dtype: "float16", gpu_memory_utilization: 0.8 },
      };
    }
    const quick = quickParams(request);
    const isTemplate = request.template_spec_name === "bf16";
    return {
      ok: true,
      form_values: {
        dtype: Object.hasOwn(quick, "dtype")
          ? quick.dtype
          : isTemplate ? "bfloat16" : "auto",
        gpu_memory_utilization: Object.hasOwn(quick, "gpu_memory_utilization")
          ? quick.gpu_memory_utilization
          : isTemplate ? 0.75 : 0.9,
      },
    };
  });

  mountEvaluationHarness({
    initialValues: { model_id: 17, engine: "vllm", command_mode: "engine" },
  });
  await waitFor(() => {
    assert.ok(requests.some((request) => {
      const overrides = request.container_overrides as
        | { command_mode?: string }
        | undefined;
      return overrides?.command_mode !== "manual";
    }));
    assert.equal(getHarnessContext().evaluation.evaluating, false);
    assert.equal((screen.getByLabelText("dtype") as HTMLInputElement).value, "auto");
    assert.equal((screen.getByLabelText("GPU memory") as HTMLInputElement).value, "0.9");
  });

  act(() => {
    getHarnessContext().form.setFieldValue("command_mode", "manual");
  });
  await waitFor(() => {
    assert.ok(requests.some((request) => {
      const overrides = request.container_overrides as
        | { command_mode?: string }
        | undefined;
      return overrides?.command_mode === "manual";
    }));
    assert.equal(getHarnessContext().evaluation.evaluating, false);
    assert.equal((screen.getByLabelText("dtype") as HTMLInputElement).value, "auto");
    assert.equal((screen.getByLabelText("GPU memory") as HTMLInputElement).value, "0.9");
  });
  act(() => {
    getHarnessContext().form.setFieldValue("command_mode", "engine");
  });
  const user = (await import("@testing-library/user-event")).default.setup({
    document: dom.window.document,
  });
  await user.click(screen.getByRole("button", { name: "Select BF16 template" }));
  await waitFor(() => {
    const templateRequest = requests.find((request) => request.template_spec_name === "bf16");
    assert.ok(templateRequest);
    assert.equal(Object.hasOwn(quickParams(templateRequest), "dtype"), false);
    assert.equal((screen.getByLabelText("dtype") as HTMLInputElement).value, "bfloat16");
  });
});
