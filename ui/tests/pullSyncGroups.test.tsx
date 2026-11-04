import { dom, createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { installPullFake } from "./helpers/pullFake.ts";

const { i18n, locale } = await createI18n(["settings", "nodes"]);
const { cleanup, render, screen, waitFor, within, fireEvent } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const SyncGroups = (await import("../src/pages/settings/pull/SyncGroups.tsx")).default;

const text = locale.settings.pull.groups as Record<string, string>;
const action = locale.settings.pull.action as Record<string, string>;
const saveName = new RegExp(`^(loading)?\\s*${action.save}$`);
const fill = (template: string, values: Record<string, string | number>) => i18n.t(template, values);

const fakes: { restore(): void }[] = [];
afterEach(() => {
  cleanup();
  while (fakes.length) fakes.pop()!.restore();
});

const GROUPS = [
  { id: 1, name: "default-all-nodes", node_requirements: {}, rate_limit_kib_s: 0 },
  { id: 2, name: "Site B", node_requirements: { site: ["idc-b"] }, rate_limit_kib_s: 204800 },
];

function setup(groups: Parameters<typeof installPullFake>[1] = { groups: GROUPS }) {
  const fake = installPullFake(api as unknown as Record<string, unknown>, groups);
  fakes.push(fake);
  render(<I18nextProvider i18n={i18n}><SyncGroups /></I18nextProvider>);
  return { fake, user: userEvent.setup({ document: dom.window.document }) };
}
const rows = () => within(screen.getByRole("region", { name: text.tableLabel })).getAllByRole("row").slice(1);
const rowText = (row: HTMLElement) => row.textContent ?? "";

test("the seeded group is an ordinary one, and the rate limit shows in MB/s", async () => {
  setup();
  await waitFor(() => assert.equal(rows().length, 2));
  const [seeded, siteB] = rows().map(rowText);
  assert.ok(seeded.includes("default-all-nodes"), seeded);
  assert.ok(seeded.includes(locale.settings.pull.rules.allNodes) && seeded.includes(text.unlimited));
  assert.ok(seeded.includes("6"), "all six nodes of the fleet");
  assert.equal(document.querySelectorAll(".ant-tag").length, 2, "no tag beyond the two selector tags: nothing marks a group as special");
  assert.ok(siteB.includes("Site B") && siteB.includes("site=idc-b") && siteB.includes("200 MB/s"), siteB);
  assert.ok(screen.getByText(text.rules), "the union / no-group semantics are explained");
});

test("loading, empty and failed states", async () => {
  const fake = installPullFake(api as unknown as Record<string, unknown>, { groups: [] });
  fakes.push(fake);
  fake.failNext("syncGroups", new Error("down"));
  render(<I18nextProvider i18n={i18n}><SyncGroups /></I18nextProvider>);
  assert.ok(document.querySelector(".ant-spin-spinning"));
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText(fill("pull.loadFailed", { ns: "settings", detail: "down" }));
  await user.click(screen.getByRole("button", { name: action.retry }));
  await screen.findByText(text.empty);
});

test("a new group converts the MB/s the form speaks into the KiB/s the API stores", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 2));
  await user.click(screen.getByRole("button", { name: new RegExp(text.new) }));
  const drawer = await screen.findByRole("dialog");
  await user.type(within(drawer).getByLabelText(text.name), "Cloud");
  const combobox = within(drawer).getByRole("combobox", { name: text.selector });
  await user.type(combobox, "gpuworks.ai/provider=runpod");
  fireEvent.keyDown(combobox, { key: "Enter", keyCode: 13, which: 13 });
  await within(drawer).findByText(fill("pull.selector.matches", { ns: "settings", count: 1, total: 6 }));
  const value = within(drawer).getByLabelText(text.rateValue) as HTMLInputElement;
  assert.equal(value.disabled, true, "no limit until the switch is on");
  await user.click(within(drawer).getByRole("switch", { name: text.rateLimit }));
  await user.type(value, "200");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await waitFor(() => assert.equal(rows().length, 3));
  assert.deepEqual(fake.calls.find((call) => call.method === "createSyncGroup")?.args[0], {
    name: "Cloud", node_requirements: { "gpuworks.ai/provider": ["runpod"] }, rate_limit_kib_s: 204800,
  });
});

test("a limit that is switched on but empty is refused before any request", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 2));
  await user.click(screen.getByRole("button", { name: new RegExp(text.new) }));
  const drawer = await screen.findByRole("dialog");
  await user.type(within(drawer).getByLabelText(text.name), "Cloud");
  await user.click(within(drawer).getByRole("switch", { name: text.rateLimit }));
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await within(drawer).findByText(text.rateRequired);
  assert.equal(fake.calls.filter((call) => call.method === "createSyncGroup").length, 0);
});

test("the server's refusal of an empty name shows at the name field", async () => {
  const { user } = setup();
  await waitFor(() => assert.equal(rows().length, 2));
  await user.click(screen.getByRole("button", { name: new RegExp(text.new) }));
  const drawer = await screen.findByRole("dialog");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await within(drawer).findByText(fill("pull.invalidField", { ns: "settings", field: locale.settings.pull.fields.name, detail: locale.common.pullSettingReason.name_required }));
});

test("duplicate create and rename remain at the localized name field without changing groups", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 2));
  await user.click(screen.getByRole("button", { name: new RegExp(text.new) }));
  let drawer = await screen.findByRole("dialog");
  await user.type(within(drawer).getByLabelText(text.name), " Site B ");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  const refusal = fill("pull.invalidField", { ns: "settings", field: locale.settings.pull.fields.name, detail: locale.common.pullSettingReason.name_duplicate });
  await within(drawer).findByText(refusal);
  assert.equal(fake.groups.length, 2);
  await user.click(within(drawer).getByRole("button", { name: action.cancel }));
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  await user.click(within(rows()[0]).getByRole("button", { name: action.edit }));
  drawer = await screen.findByRole("dialog");
  const input = within(drawer).getByLabelText(text.name);
  await user.clear(input);
  await user.type(input, "Site B");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await within(drawer).findByText(refusal);
  assert.equal(fake.groups[0].name, "default-all-nodes");
});

test("group IDs distinguish duplicate legacy rows and their delete confirmations", async () => {
  const { fake, user } = setup({ groups: [
    { ...GROUPS[0], id: 11, name: "same" }, { ...GROUPS[0], id: 12, name: "same" },
  ] });
  await waitFor(() => assert.equal(rows().length, 2));
  assert.ok(within(rows()[0]).getByText("#11"));
  assert.ok(within(rows()[1]).getByText("#12"));
  await user.click(screen.getByRole("button", { name: fill(text.deleteGroup, { name: "same", id: 12 }) }));
  await screen.findByText(fill(text.deleteConfirm, { name: "same", id: 12 }));
  const popover = document.querySelector(".ant-popconfirm:not(.ant-popover-hidden)") as HTMLElement;
  await user.click(within(popover).getByRole("button", { name: action.delete }));
  await waitFor(() => assert.equal(rows().length, 1));
  assert.deepEqual(fake.calls.find((call) => call.method === "deleteSyncGroup")?.args, [12]);
  assert.equal(fake.groups[0].id, 11);
});

test("editing the seeded group keeps its name, and an untouched limit round-trips exactly", async () => {
  const { fake, user } = setup({ groups: [GROUPS[0], { ...GROUPS[1], rate_limit_kib_s: 500 }] });
  await waitFor(() => assert.equal(rows().length, 2));
  await user.click(within(rows()[0]).getByRole("button", { name: action.edit }));
  let drawer = await screen.findByRole("dialog");
  const name = within(drawer).getByLabelText(text.name) as HTMLInputElement;
  assert.equal(name.value, "default-all-nodes");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  assert.deepEqual(fake.calls.find((call) => call.method === "updateSyncGroup")?.args, [1, { name: "default-all-nodes", node_requirements: {}, rate_limit_kib_s: 0 }]);

  await user.click(within(rows()[1]).getByRole("button", { name: action.edit }));
  drawer = await screen.findByRole("dialog");
  assert.equal((within(drawer).getByLabelText(text.rateValue) as HTMLInputElement).value, "0.49");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await waitFor(() => assert.equal(fake.calls.filter((call) => call.method === "updateSyncGroup").length, 2));
  assert.equal((fake.calls.filter((call) => call.method === "updateSyncGroup")[1].args[1] as { rate_limit_kib_s: number }).rate_limit_kib_s, 500);
});

test("deleting asks first, says how to get every node copying again, and then removes the group", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 2));
  await user.click(screen.getByRole("button", { name: fill(text.deleteGroup, { name: "default-all-nodes", id: 1 }) }));
  await screen.findByText(fill(text.deleteConfirm, { name: "default-all-nodes", id: 1 }));
  assert.ok(screen.getByText(text.deleteConfirmBody));
  await user.click(screen.getByRole("button", { name: action.cancel }));
  assert.equal(fake.calls.filter((call) => call.method === "deleteSyncGroup").length, 0);
  await user.click(screen.getByRole("button", { name: fill(text.deleteGroup, { name: "default-all-nodes", id: 1 }) }));
  const popover = document.querySelector(".ant-popconfirm:not(.ant-popover-hidden)") as HTMLElement;
  await user.click(within(popover).getByRole("button", { name: action.delete }));
  await waitFor(() => assert.equal(rows().length, 1));
  assert.deepEqual(fake.calls.find((call) => call.method === "deleteSyncGroup")?.args, [1]);
});

test("Escape closes the drawer and Enter in the name field saves", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 2));
  await user.click(screen.getByRole("button", { name: new RegExp(text.new) }));
  let drawer = await screen.findByRole("dialog");
  await user.type(within(drawer).getByLabelText(text.name), "Lab");
  fireEvent.keyDown(within(drawer).getByLabelText(text.name), { key: "Escape", keyCode: 27 });
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  assert.equal(fake.calls.filter((call) => call.method === "createSyncGroup").length, 0);
  await user.click(screen.getByRole("button", { name: new RegExp(text.new) }));
  drawer = await screen.findByRole("dialog");
  await user.type(within(drawer).getByLabelText(text.name), "Lab{Enter}");
  await waitFor(() => assert.equal(fake.calls.filter((call) => call.method === "createSyncGroup").length, 1));
});
