import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n, locale } = await createI18n(["common"], "en");
const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { api } = await import("../src/api.ts");
const { installFleet } = await import("./helpers/fakeFleet.ts");
const { openNodePicker } = await import("./helpers/nodePicker.ts");
const { default: NodePicker } = await import("../src/nodes/NodePicker.tsx");
type NodeT = import("../src/api.ts").Node;

const original = { nodes: api.nodes, nodesPage: api.nodesPage, node: api.node, nodeFacets: api.nodeFacets };
afterEach(() => { cleanup(); Object.assign(api, original); });

const fleet = [
  { uuid: "n1", name: "gpu-a", state: "online" },
  { uuid: "n2", name: "gpu-b", state: "online" },
  { uuid: "n3", name: "cpu-c", state: "offline" },
];
const picker = locale.common.picker;

function show(props: React.ComponentProps<typeof NodePicker>) {
  return render(<I18nextProvider i18n={i18n}><NodePicker aria-label="Node" {...props} /></I18nextProvider>);
}
const options = () => screen.queryAllByRole("option").map((option) => option.textContent);

test("typing searches the server for nodes instead of filtering a listed fleet", async () => {
  const fake = installFleet(api as never, fleet);
  show({ filter: { state: ["online"] } });
  const box = screen.getByRole("combobox", { name: "Node" });
  await openNodePicker(box);
  await waitFor(() => assert.deepEqual(options(), ["gpu-a", "gpu-b"]));
  assert.deepEqual(fake.pages.at(-1)?.state, ["online"], "the filter goes to the server");
  fireEvent.change(box, { target: { value: "gpu-b" } });
  await waitFor(() => assert.deepEqual(options(), ["gpu-b"]));
  assert.equal(fake.pages.at(-1)?.q, "gpu-b");
});

test("a single pick reports the uuid with its node, and clearing reports nothing chosen", async () => {
  installFleet(api as never, fleet);
  const changes: Array<[unknown, string[]]> = [];
  const view = show({ allowClear: true, onChange: (value, nodes) => changes.push([value, nodes.map((node: NodeT) => node.name)]) });
  await openNodePicker(screen.getByRole("combobox", { name: "Node" }));
  fireEvent.click(await screen.findByRole("option", { name: "gpu-a" }));
  assert.deepEqual(changes.at(-1), ["n1", ["gpu-a"]]);
  view.rerender(<I18nextProvider i18n={i18n}><NodePicker aria-label="Node" allowClear value="n1" onChange={(value, nodes) => changes.push([value, nodes.map((node) => node.name)])} /></I18nextProvider>);
  await waitFor(() => assert.equal((screen.getByRole("combobox", { name: "Node" }) as HTMLInputElement).value, "gpu-a"));
  fireEvent.click(screen.getByRole("button", { name: picker.clear }));
  assert.deepEqual(changes.at(-1), [undefined, []]);
});

test("a chosen uuid that names no node is still shown so it can be removed", async () => {
  installFleet(api as never, fleet);
  const changes: unknown[] = [];
  show({ mode: "multiple", value: ["gone"], unknownLabel: (uuid) => `missing ${uuid}`, onChange: (value) => changes.push(value) });
  assert.ok(await screen.findByText("missing gone"));
  fireEvent.click(screen.getByRole("button", { name: picker.remove.replace("{{name}}", "missing gone") }));
  assert.deepEqual(changes.at(-1), []);
});

test("past maxCount the other nodes cannot be added, and disabled or titled options say so", async () => {
  installFleet(api as never, fleet);
  show({ mode: "multiple", value: ["n1"], maxCount: 1, optionTitle: (node) => `title ${node.name}`, optionDisabled: (node) => node.state === "offline" });
  await openNodePicker(screen.getByRole("combobox", { name: "Node" }));
  const b = await screen.findByRole("option", { name: "gpu-b" });
  assert.equal(b.getAttribute("aria-disabled"), "true", "the count is reached");
  assert.equal(b.getAttribute("title"), "title gpu-b");
  assert.notEqual(screen.getByRole("option", { name: "gpu-a" }).getAttribute("aria-disabled"), "true", "a chosen node stays removable");
  assert.equal(screen.getByRole("option", { name: "cpu-c" }).getAttribute("aria-disabled"), "true");
});

test("an empty search says there is nothing, in the caller's words when given", async () => {
  installFleet(api as never, []);
  show({ notFoundContent: "No online node" });
  await openNodePicker(screen.getByRole("combobox", { name: "Node" }));
  assert.ok(await screen.findByText("No online node"));
});

test("the form's id and validity land on the input, and the ref focuses it", async () => {
  installFleet(api as never, fleet);
  const ref = React.createRef<HTMLInputElement>();
  render(<I18nextProvider i18n={i18n}><label htmlFor="pick">Target</label><NodePicker ref={ref} id="pick" aria-invalid aria-describedby="pick-error" /></I18nextProvider>);
  const box = screen.getByRole("combobox", { name: "Target" });
  assert.equal(box.getAttribute("aria-invalid"), "true");
  assert.equal(box.getAttribute("aria-describedby"), "pick-error");
  ref.current!.focus();
  assert.equal(document.activeElement, box);
});

test("a new search over listed nodes says it is still loading until the server answers", async () => {
  const fake = installFleet(api as never, fleet);
  show({});
  const box = screen.getByRole("combobox", { name: "Node" });
  await openNodePicker(box);
  await waitFor(() => assert.deepEqual(options(), ["gpu-a", "gpu-b", "cpu-c"]));
  let answer!: () => void;
  const listed = api.nodesPage;
  api.nodesPage = (query) => new Promise((resolve) => { answer = () => resolve(listed(query)); });
  fireEvent.change(box, { target: { value: "gpu-b" } });
  assert.ok(await screen.findByText(picker.loading), "the open list shows the request is running");
  assert.deepEqual(options(), ["gpu-a", "gpu-b", "cpu-c"], "the earlier results stay listed meanwhile");
  answer();
  await waitFor(() => assert.deepEqual(options(), ["gpu-b"]));
  assert.equal(screen.queryByText(picker.loading), null);
  assert.equal(fake.pages.at(-1)?.q, "gpu-b");
});
