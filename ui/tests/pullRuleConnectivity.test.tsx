// The "test connectivity" section of the model-site rule drawer: the unsaved
// draft and 1-5 chosen online nodes go to POST /pull/source-rules/test and the
// answer is shown per node and address.
import { dom, createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { installPullFake } from "./helpers/pullFake.ts";
import { installFleet } from "./helpers/fakeFleet.ts";

const { i18n, locale } = await createI18n(["settings", "nodes", "common"]);
const { cleanup, render, screen, waitFor, within, fireEvent } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const { APIError } = await import("../src/apiError.ts");
const { api } = await import("../src/api.ts");
const PullSourceRules = (await import("../src/pages/settings/pull/PullSourceRules.tsx")).default;

const text = locale.settings.pull.rules as Record<string, string>;
const test_ = locale.settings.pull.rules.test as Record<string, string>;
const fakes: { restore(): void }[] = [];
afterEach(() => {
  cleanup();
  while (fakes.length) fakes.pop()!.restore();
});

const NODES = [
  { uuid: "n1", name: "gpu-a-01", state: "online" },
  { uuid: "n2", name: "gpu-a-02", state: "online" },
  { uuid: "n3", name: "gpu-a-03", state: "online" },
  { uuid: "n4", name: "gpu-a-04", state: "online" },
  { uuid: "n5", name: "gpu-a-05", state: "online" },
  { uuid: "n6", name: "gpu-a-06", state: "online" },
  { uuid: "off", name: "gpu-off", state: "offline" },
];
const SITE = { id: 1, match: "https://huggingface.co", endpoints: ["https://hf-mirror.corp.example"], allow_upstream: true, note: "", node_requirements: {} };
const IMAGE = { id: 2, match: "docker.io", endpoints: ["mirror.example"], allow_upstream: true, note: "", node_requirements: {} };

async function openDrawer(ruleMatch: string) {
  installFleet(api as unknown as Record<string, unknown>, NODES as never);
  const fake = installPullFake(api as unknown as Record<string, unknown>, { rules: [SITE, IMAGE] });
  fakes.push(fake);
  render(<I18nextProvider i18n={i18n}><PullSourceRules /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  const region = await screen.findByRole("region", { name: text.tableLabel });
  await waitFor(() => assert.equal(within(region).getAllByRole("row").length, 3));
  const row = within(region).getAllByRole("row").find((candidate) => candidate.textContent?.includes(ruleMatch))!;
  await user.click(within(row).getByRole("button", { name: locale.settings.pull.action.edit }));
  return { fake, user, drawer: await screen.findByRole("dialog") };
}

async function pickNodes(drawer: HTMLElement, names: string[]) {
  const picker = within(drawer).getByRole("combobox", { name: test_.nodes });
  for (const name of names) {
    fireEvent.mouseDown(picker);
    const option = await screen.findByText(name, { selector: ".ant-select-item-option-content" });
    fireEvent.click(option);
  }
}
const runButton = (drawer: HTMLElement) => within(drawer).getByRole("button", { name: new RegExp(`^(loading)?\\s*${test_.run}$`) }) as HTMLButtonElement;

test("an image rule has no connectivity test; a model site rule has", async () => {
  const image = await openDrawer("docker.io");
  assert.equal(within(image.drawer).queryByRole("button", { name: test_.run }), null);
  cleanup();
  fakes.pop()!.restore();
  const site = await openDrawer("huggingface.co");
  assert.ok(runButton(site.drawer));
  assert.equal(runButton(site.drawer).disabled, true, "no node is chosen yet");
  assert.equal((within(site.drawer).getByLabelText(test_.repo) as HTMLInputElement).value, "Qwen/Qwen3-0.6B");
});

test("the draft, not the saved rule, and the chosen nodes are tested; the table shows each node and address", async () => {
  const { fake, user, drawer } = await openDrawer("huggingface.co");
  fake.probe = (node, base) => {
    if (node.name === "gpu-a-02") return base.includes("corp.example") ? { base, status: "unreachable", latency_ms: 0, error: "dial tcp: i/o timeout" } : { base, status: "auth_required", code: 401, latency_ms: 340 };
    return base.includes("corp.example") ? { base, status: "http_error", code: 404, latency_ms: 80 } : { base, status: "ok", code: 200, latency_ms: 210 };
  };
  // Edit the draft without saving it: a second endpoint is added.
  await user.click(within(drawer).getByRole("button", { name: text.addEndpoint }));
  await user.type(within(drawer).getByLabelText(fillN(text.endpointN, 2)), " https://second.example/ ");
  await pickNodes(drawer, ["gpu-a-01", "gpu-a-02"]);
  fireEvent.change(within(drawer).getByLabelText(test_.repo), { target: { value: "org/gated-model" } });
  await user.click(runButton(drawer));
  const table = await within(drawer).findByRole("table");
  const rows = () => within(table).getAllByRole("row").slice(1).map((row) => [...row.querySelectorAll("td")].map((cell) => cell.textContent));
  await waitFor(() => assert.equal(rows().length, 6));
  assert.deepEqual(rows()[0], ["gpu-a-01", "https://hf-mirror.corp.example", "HTTP 404", "80 ms"]);
  const flat = rows().map((cells) => cells.join("|")).join("\n");
  assert.ok(flat.includes(`gpu-a-01|https://huggingface.co|${test_.ok}|210 ms`));
  assert.ok(flat.includes(`gpu-a-02|https://hf-mirror.corp.example|${test_.unreachable}|`));
  assert.ok(flat.includes(`gpu-a-02|https://huggingface.co|${test_.authRequired}|340 ms`), "401/403 reads as a missing credential, not as a failure");
  // One request per node, all carrying the same draft.
  const requests = fake.calls.filter((call) => call.method === "testPullSourceRule").map((call) => call.args[0] as { node_uuid: string });
  const draft = { match: "https://huggingface.co", endpoints: ["https://hf-mirror.corp.example", "https://second.example/"], allow_upstream: true, repo: "org/gated-model" };
  assert.deepEqual(
    [...requests].sort((a, b) => a.node_uuid.localeCompare(b.node_uuid)),
    [{ ...draft, node_uuid: "n1" }, { ...draft, node_uuid: "n2" }],
  );
  assert.equal(fake.calls.some((call) => call.method === "updatePullSourceRule"), false, "testing saves nothing");
});

const fillN = (template: string, n: number) => template.replace("{{n}}", String(n));

test("each node's answer shows as it arrives while the others are still testing", async () => {
  const { drawer } = await openDrawer("huggingface.co");
  await pickNodes(drawer, ["gpu-a-01", "gpu-a-02"]);
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  const inner = api.testPullSourceRule;
  api.testPullSourceRule = (async (body: { node_uuid: string }) => {
    if (body.node_uuid === "n2") await gate;
    return inner(body as never);
  }) as typeof api.testPullSourceRule;
  try {
    fireEvent.click(runButton(drawer));
    const table = await within(drawer).findByRole("table");
    await waitFor(() => assert.equal(table.textContent?.includes("https://huggingface.co"), true));
    const pendingRow = within(table).getByText(test_.pending).closest("tr")!;
    assert.equal(pendingRow.querySelector("td")?.textContent, "gpu-a-02");
    assert.equal(!!drawer.querySelector(".ant-btn-loading"), true, "the run is in flight until every node answered");
    release();
    await waitFor(() => assert.equal(within(table).queryByText(test_.pending) === null, true));
    assert.equal(!!drawer.querySelector(".ant-btn-loading"), false);
  } finally {
    api.testPullSourceRule = inner;
  }
});

test("an offline node is reported on its own row", async () => {
  const { fake, drawer } = await openDrawer("huggingface.co");
  await pickNodes(drawer, ["gpu-a-01", "gpu-a-02"]);
  fake.failNext("testPullSourceRule", new APIError("NODE_OFFLINE", undefined, "node is offline", 409));
  fireEvent.click(runButton(drawer));
  const table = await within(drawer).findByRole("table");
  await within(table).findByText(/离线|offline/i);
  await within(table).findByText("https://huggingface.co");
});

test("a node that cannot run the test shows why instead of addresses", async () => {
  const { fake, drawer } = await openDrawer("huggingface.co");
  fake.probe = () => undefined;
  await pickNodes(drawer, ["gpu-a-01"]);
  fireEvent.click(runButton(drawer));
  await within(drawer).findByText(/unknown rpc method source\.probe/);
});

test("the run is shown as loading and a refused draft is explained at the section", async () => {
  const { fake, drawer } = await openDrawer("huggingface.co");
  await pickNodes(drawer, ["gpu-a-01"]);
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  const inner = api.testPullSourceRule;
  api.testPullSourceRule = (async (body: never) => { await gate; return inner(body); }) as typeof api.testPullSourceRule;
  fireEvent.click(runButton(drawer));
  await waitFor(() => assert.ok(drawer.querySelector(".ant-btn-loading"), "the test is in flight"));
  release();
  await within(drawer).findByRole("table");
  fake.failNext("testPullSourceRule", new APIError("PULL_SETTING_INVALID", { field: "endpoints", detail: "a rule with no endpoint must allow upstream" }, "a rule with no endpoint must allow upstream", 400));
  fireEvent.click(runButton(drawer));
  await within(drawer).findByText(/a rule with no endpoint must allow upstream/);
  assert.equal(within(drawer).queryByRole("table"), null, "a refused run leaves no stale result");
});

test("an offline node reports itself and at most five nodes can be chosen", async () => {
  const { drawer } = await openDrawer("huggingface.co");
  await pickNodes(drawer, ["gpu-a-01", "gpu-a-02", "gpu-a-03", "gpu-a-04", "gpu-a-05"]);
  const picker = within(drawer).getByRole("combobox", { name: test_.nodes });
  fireEvent.mouseDown(picker);
  const sixth = await screen.findByText("gpu-a-06", { selector: ".ant-select-item-option-content" });
  assert.equal(sixth.closest(".ant-select-item")?.getAttribute("aria-selected"), "false");
  fireEvent.click(sixth);
  assert.equal(drawer.querySelectorAll(".ant-select-selection-item").length, 5, "a sixth node is refused");
  assert.equal(screen.queryByText("gpu-off", { selector: ".ant-select-item-option-content" }), null, "offline nodes are not offered");
});
