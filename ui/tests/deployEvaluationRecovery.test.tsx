import { dom } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";

const { act, cleanup, waitFor } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { mountEvaluationHarness, installEvaluate, quickParams, resetHarnessContext } =
  await import("./helpers/deployEvaluationProjection.tsx");
const original = api.evaluate;
afterEach(() => { cleanup(); resetHarnessContext(); api.evaluate = original; });
after(() => dom.window.close());

test("an early evaluation error does not orphan the previous template display fields", async () => {
  const requests = installEvaluate((request) => {
    if (request.served_name === "invalid") {
      // The real API can reject fields before materializing a launch, so this
      // accepted response has errors but no form_values display instruction.
      return { ok: false, errors: [{ field: "served_name", code: "INVALID_REQUEST", params: {} }] };
    }
    const quick = quickParams(request);
    const inherited = request.template_spec_name === "bf16"
      ? { dtype: "bfloat16" }
      : { dtype: "auto", max_model_len: 8192 };
    return { ok: true, form_values: { ...inherited, ...quick } };
  });
  const current = mountEvaluationHarness();
  await waitFor(() => {
    assert.equal(current().form.getFieldValue("max_model_len"), 8192);
    assert.equal(current().evaluation.evaluating, false);
  });
  act(() => current().form.setFieldValue("served_name", "invalid"));
  await waitFor(() => {
    assert.equal(current().evaluation.result?.ok, false);
    assert.equal(current().evaluation.evaluating, false);
  });
  act(() => current().form.setFieldsValue({
    served_name: "valid",
    template_id: 31,
    template_spec_source: "upstream",
    template_spec_name: "bf16",
  }));
  await waitFor(() => {
    assert.equal(current().evaluation.result?.form_values?.dtype, "bfloat16");
    assert.equal(current().evaluation.evaluating, false);
  });
  assert.equal(current().form.getFieldValue("max_model_len"), undefined);
  assert.equal(requests.filter(r => r.template_spec_name === "bf16")
    .some(r => Object.hasOwn(quickParams(r), "max_model_len")), false);
});
