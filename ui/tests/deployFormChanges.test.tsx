import { dom } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";

// A card index names a card on one machine (GWKS-522): when the operator picks
// another node, the card choice made for the previous one starts over. A
// prefill that states the node and the cards together is a draft, not a node
// change, and keeps them.

const { render, cleanup } = await import("@testing-library/react");
const { Form } = await import("antd");
const useDeployFormChanges = (
  await import("../src/pages/deploy/useDeployFormChanges.ts")
).default;

afterEach(() => cleanup());
after(() => dom.window.close());

function mount(initial: Record<string, unknown>) {
  let form: import("antd").FormInstance;
  let onChange: (changed: Record<string, unknown>) => void = () => {};
  function Host() {
    [form] = Form.useForm();
    onChange = useDeployFormChanges({ form, templates: [], images: [] });
    return (
      <Form form={form} initialValues={initial}>
        <Form.Item name="node_uuid">
          <input />
        </Form.Item>
        <Form.Item name="gpu_indexes">
          <input />
        </Form.Item>
      </Form>
    );
  }
  render(<Host />);
  return {
    gpuIndexes: () => form.getFieldValue("gpu_indexes"),
    nodeUUID: () => form.getFieldValue("node_uuid"),
    // What antd does for a field the operator changes: the field takes the
    // new value first, then the form reports it to onValuesChange.
    change: (changed: Record<string, unknown>) => {
      form.setFieldsValue(changed);
      onChange(changed);
    },
  };
}

test("a changed node starts the card choice over", () => {
  const host = mount({ node_uuid: "card-7", gpu_indexes: [7], tp: 1 });
  assert.deepEqual(host.gpuIndexes(), [7]);
  host.change({ node_uuid: "one-card" });
  assert.equal(host.gpuIndexes(), undefined);
  assert.equal(host.nodeUUID(), "one-card");
});

test("a draft that states the node and its cards together keeps them", () => {
  const host = mount({ node_uuid: "card-7", gpu_indexes: [7], tp: 1 });
  host.change({ node_uuid: "card-7", gpu_indexes: [0, 1] });
  assert.deepEqual(host.gpuIndexes(), [0, 1]);
});

test("changing something else leaves the cards alone", () => {
  const host = mount({ node_uuid: "card-7", gpu_indexes: [7], tp: 1 });
  host.change({ tp: 2 });
  assert.deepEqual(host.gpuIndexes(), [7]);
});
