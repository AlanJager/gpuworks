import { dom, createI18n } from "./helpers/dom.ts";
import { installFleet } from "./helpers/fakeFleet.ts";
import { engineProfiles } from "./helpers/engineFixture.ts";
import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";

// The expert resource step gives a multi-node group the same placement choice
// as the quick form: the platform picks the nodes (a read-only preview of its
// pick) unless the deployer switches to naming them.

const { render, cleanup, screen, fireEvent, within } = await import("@testing-library/react");
const { Form } = await import("antd");
const { api } = await import("../src/api.ts");
const Resource = (await import("../src/pages/deploy/DeployResourceStep.tsx")).default;
const { i18n, locale } = await createI18n(["common", "deploy", "nodes", "images"]);
const profile = engineProfiles.find((row) => row.name === "vllm")!;
const original = { ...api };
afterEach(() => { cleanup(); Object.assign(api, original); });
after(() => dom.window.close());

function mount(values: Record<string, unknown>, result: Record<string, unknown> | null = null) {
  installFleet(api as unknown as Record<string, unknown>, [{ uuid: "n1", name: "node-1", state: "online", gpu_count: 2, management_address: "10.0.0.1:9443" }]);
  let instance: import("antd").FormInstance;
  function Host() {
    [instance] = Form.useForm();
    return <Form form={instance} initialValues={{ node_selection_mode: "auto", ...values }}>
      <Resource profile={profile} result={result as never} />
    </Form>;
  }
  render(<I18nextProvider i18n={i18n}><Host /></I18nextProvider>);
  return () => instance;
}

const r = locale.deploy.resource;

test("a multi-node group opens on automatic: no node list, the server's pick as a preview", () => {
  mount({ nnodes: 2 }, {
    ok: true, auto_placed: true,
    rank_commands: [
      { node_uuid: "n1", node_name: "node-1", node_rank: 0, role: "leader" },
      { node_uuid: "n2", node_name: "node-2", node_rank: 1, role: "follower" },
    ],
  });
  assert.ok(screen.getByText(r.groupPlacement));
  assert.equal(screen.queryByText(r.distributedNodes), null);
  assert.equal(screen.queryByText(r.masterAddress), null, "the leader's address comes from the node the platform picks");
  const preview = within(screen.getByTestId("auto-group-preview"));
  assert.ok(preview.getByText("node-1"));
  assert.ok(preview.getByText("node-2"));
  assert.ok(screen.getByText(locale.deploy.workbench.node.previewTitle));
});

test("switching the group to manual brings back the node list and the leader address", () => {
  const form = mount({ nnodes: 2 });
  const switcher = screen.getByText(r.groupPlacement).closest(".ant-form-item")!;
  fireEvent.click(within(switcher as HTMLElement).getByText(r.manualSelect));
  assert.equal(form().getFieldValue("units_placement"), "manual");
  assert.ok(screen.getByText(r.distributedNodes));
  assert.ok(screen.getByText(r.masterAddress));
});

test("a single node has no group placement", () => {
  mount({ nnodes: 1 });
  assert.equal(screen.queryByText(r.groupPlacement), null);
});
