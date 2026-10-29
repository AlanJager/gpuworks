import assert from "node:assert/strict";
import { test } from "node:test";
import {
  api, choosePolicy, cleanup, controls, defaults, deployment, dictionary, f, fireEvent, i18n, locale, mount, openEditor, p, saveButton, screen, spin, state, stub, stubDictionary, summaryOf, summaryText, waitFor, withPolicy, within, y,
} from "./helpers/yieldPolicyHarness.tsx";

// The yield policy part of a deployment's scheduling policy card: when the card
// shows it, what its summary says, and the editor behind it - the one choice of
// yield policy, the parameters that apply to each policy, the defaults and
// bounds that come from the server's dictionary, "modified" and "restore
// defaults". Saving, the server's refusals, the restrictions and the keyboard
// are in deploymentYieldPolicySave.test.tsx.

const modified = (editor: Awaited<ReturnType<typeof openEditor>>) => editor.queryAllByText(y.modified);

test("the summary is one line: the policy and what matters, and a deployment with no policy never yields automatically", async () => {
  stub();
  const cases: Array<[keyof typeof defaults, string]> = [
    ["never", "Never yield automatically"],
    ["idle", "Yield when idle · yields after 1 min idle"],
    ["timeslice", "Yield on a turn · idle 30 s / up to 2 min per turn"],
    ["anytime", "Yield anytime · idle 30 s"],
  ];
  for (const [policy, line] of cases) {
    mount(withPolicy(defaults[policy]));
    assert.ok((await summaryText()).includes(summaryOf(line)), policy);
    cleanup();
  }
  mount(deployment());
  assert.ok((await summaryText()).includes(summaryOf("Never yield automatically")));
  assert.equal(state.dictionaryReads, 0, "the summary needs no dictionary");
});

test("the part shows for a deployment that can sleep, and for any that kept a policy besides never", async () => {
  stub();
  mount(deployment({ sleepable: false }));
  await screen.findByRole("region", { name: p.title });
  assert.ok(!(await summaryText()).includes(y.summaryLabel));
  cleanup();
  // A multi-node or PD deployment that can sleep shows it, with never.
  mount(deployment({ nnodes: 2 }));
  assert.ok((await summaryText()).includes(summaryOf("Never yield automatically")));
  cleanup();
  mount(deployment({ pd: { mode: "ep", roles: [] } }));
  assert.ok((await summaryText()).includes(summaryOf("Never yield automatically")));
  cleanup();
  // One that kept a policy from when it could still shows it, to go back.
  mount(deployment({ sleepable: false, yield_policy: defaults.anytime }));
  assert.ok((await summaryText()).includes(summaryOf("Yield anytime · idle 30 s")));
});

test("the editor is one choice of yield policy, in the dictionary's rank order, and its parameters: no presets, no custom, no automatic sleep", async () => {
  stub();
  const user = mount();
  const editor = await openEditor(user);
  assert.equal(editor.queryAllByRole("radio").length, 0, "no preset radios");
  assert.equal(editor.queryByRole("switch"), null, "no automatic sleep switch");
  assert.equal(editor.queryByText(/custom/i), null);
  editor.getByText(y.policy.description.never);
  assert.equal(saveButton(editor).disabled, true, "nothing changed yet");
  assert.equal(editor.queryByRole("button", { name: y.restoreDefaults }), null, "never has no parameters to restore");

  await user.click(editor.getByRole("combobox", { name: y.policyLabel }));
  const options = (await screen.findAllByRole("option", { hidden: true })).map((option) => option.getAttribute("aria-label") ?? option.textContent);
  assert.deepEqual(options, dictionary.policies.map(({ policy }) => `${y.policy.name[policy]} - ${y.policy.description[policy]}`));
  assert.deepEqual(dictionary.policies.map(({ policy }) => policy), ["never", "idle", "timeslice", "anytime"]);
});

test("the editor waits for the dictionary: a loading state, then the controls; a failure says so and retrying loads it", async () => {
  stub();
  stubDictionary("pending");
  const user = mount();
  await user.click(await screen.findByRole("button", { name: p.edit }));
  await user.click(await screen.findByRole("tab", { name: y.title }));
  const region = within(await screen.findByRole("region", { name: y.title }));
  region.getByRole("status", { name: y.dictionary.loading });
  assert.equal(region.queryByRole("combobox", { name: y.policyLabel }), null, "no editing before the options are known");
  assert.equal(region.queryByText(locale.deployments.automation.save), null);
  cleanup();

  stubDictionary("error");
  const user2 = mount();
  await user2.click(await screen.findByRole("button", { name: p.edit }));
  await user2.click(await screen.findByRole("tab", { name: y.title }));
  const failed = within(await screen.findByRole("region", { name: y.title }));
  await failed.findByText(i18n.t("deployments:yieldPolicy.dictionary.failed", { detail: "dictionary unavailable" }));
  assert.equal(failed.queryByRole("combobox", { name: y.policyLabel }), null);
  state.dictionaryMode = "ok";
  await user2.click(failed.getByRole("button", { name: y.dictionary.retry }));
  await failed.findByRole("combobox", { name: y.policyLabel });
  assert.equal(failed.queryByText(i18n.t("deployments:yieldPolicy.dictionary.failed", { detail: "dictionary unavailable" })), null);
});

test("which controls show, for every policy", async () => {
  stub();
  const want = {
    never: { idle: false, maxServe: false, unfinished: false, wait: false, level: false, hold: false },
    idle: { idle: true, maxServe: false, unfinished: false, wait: false, level: true, hold: true },
    timeslice: { idle: true, maxServe: true, unfinished: true, wait: true, level: true, hold: true },
    // anytime holds abort: the wait time does not apply.
    anytime: { idle: true, maxServe: false, unfinished: true, wait: false, level: true, hold: true },
  };
  for (const policy of ["never", "idle", "timeslice", "anytime"] as const) {
    const user = mount(withPolicy(defaults[policy]));
    const editor = await openEditor(user);
    assert.deepEqual(controls(editor), want[policy], policy);
    cleanup();
  }
});

test("choosing a policy fills in its defaults from the dictionary, and a policy's description shows under the select", async () => {
  stub();
  const user = mount();
  const editor = await openEditor(user);
  await choosePolicy(user, editor, "idle");
  editor.getByText(y.policy.description.idle);
  assert.equal(spin(editor, f.idle_sec)?.value, "60");
  assert.equal(spin(editor, f.hold_max_sec)?.value, "300");
  assert.ok(within(editor.getByRole("group", { name: y.policyLabel })).getAllByText(y.policy.name.idle).length > 0);
  assert.equal(modified(editor).length, 0, "defaults are not modifications");
  assert.equal(saveButton(editor).disabled, false);

  await choosePolicy(user, editor, "timeslice");
  assert.equal(spin(editor, f.idle_sec)?.value, "30");
  assert.equal(spin(editor, f.max_serve_sec)?.value, "120");
  assert.equal(spin(editor, f.unfinished_wait_sec)?.value, "30");
  assert.equal(spin(editor, f.hold_max_sec)?.value, "600");
  assert.equal((editor.getByRole("radio", { name: y.unfinished.wait }) as HTMLInputElement).checked, true);
  assert.equal(modified(editor).length, 0);

  await choosePolicy(user, editor, "anytime");
  assert.equal(spin(editor, f.idle_sec)?.value, "30");
  assert.equal(spin(editor, f.max_serve_sec), null);
  assert.equal(spin(editor, f.hold_max_sec)?.value, "3600");
  assert.equal((editor.getByRole("radio", { name: y.unfinished.abort }) as HTMLInputElement).checked, true);
  assert.equal(spin(editor, f.unfinished_wait_sec), null, "abort has no wait");

  await choosePolicy(user, editor, "never");
  editor.getByText(y.policy.description.never);
  assert.equal(saveButton(editor).disabled, true, "back on what is saved");
  // Choosing again after editing resets what was edited: the defaults come back.
  await choosePolicy(user, editor, "idle");
  fireEvent.change(spin(editor, f.hold_max_sec) as HTMLInputElement, { target: { value: "999" } });
  await choosePolicy(user, editor, "timeslice");
  await choosePolicy(user, editor, "idle");
  assert.equal(spin(editor, f.hold_max_sec)?.value, "300");
});

test("the defaults are the dictionary's: another answer, other numbers, nothing kept in the page", async () => {
  stub();
  const other = structuredClone(dictionary);
  other.policies[1].defaults.idle_sec = 99;
  other.policies[1].defaults.hold_max_sec = 77;
  other.bounds.idle_sec = { minimum: 20, maximum: 500 };
  api.yieldPolicies = async () => structuredClone(other);
  const user = mount();
  const editor = await openEditor(user);
  await choosePolicy(user, editor, "idle");
  assert.equal(spin(editor, f.idle_sec)?.value, "99");
  assert.equal(spin(editor, f.hold_max_sec)?.value, "77");
  editor.getByText(i18n.t("deployments:yieldPolicy.range", { minimum: 20, maximum: 500 }));
  fireEvent.change(spin(editor, f.idle_sec) as HTMLInputElement, { target: { value: "501" } });
  editor.getByText(i18n.t("deployments:yieldPolicy.error.out_of_range", { field: f.idle_sec, minimum: 20, maximum: 500 }));
});

test("each changed parameter is tagged as modified, and restore defaults resets all of them", async () => {
  stub();
  const user = mount(withPolicy(defaults.timeslice));
  const editor = await openEditor(user);
  assert.equal(modified(editor).length, 0);
  assert.equal((editor.getByRole("button", { name: y.restoreDefaults }) as HTMLButtonElement).disabled, true, "nothing to restore");

  fireEvent.change(spin(editor, f.idle_sec) as HTMLInputElement, { target: { value: "31" } });
  assert.equal(modified(editor).length, 1);
  within(editor.getByRole("group", { name: f.idle_sec })).getByText(y.modified);
  fireEvent.change(spin(editor, f.max_serve_sec) as HTMLInputElement, { target: { value: "121" } });
  fireEvent.change(spin(editor, f.hold_max_sec) as HTMLInputElement, { target: { value: "601" } });
  await user.click(editor.getByRole("combobox", { name: f.sleep_level }));
  await user.click(await screen.findByText(y.sleepLevel.l2));
  await user.click(editor.getByRole("radio", { name: y.unfinished.abort }));
  assert.equal(modified(editor).length, 5, "idle, max serve, hold, sleep level, unfinished");
  within(editor.getByRole("group", { name: f.sleep_level })).getByText(y.modified);
  within(editor.getByRole("group", { name: f.unfinished })).getByText(y.modified);

  // Putting a value back removes its tag.
  fireEvent.change(spin(editor, f.idle_sec) as HTMLInputElement, { target: { value: "30" } });
  assert.equal(modified(editor).length, 4);

  // The wait time is tagged when it differs while waiting.
  await user.click(editor.getByRole("radio", { name: y.unfinished.wait }));
  fireEvent.change(spin(editor, f.unfinished_wait_sec) as HTMLInputElement, { target: { value: "45" } });
  assert.equal(modified(editor).length, 4, "unfinished is back to the default; the wait time, max serve, hold and level differ");
  within(editor.getByRole("group", { name: f.unfinished })).getByText(y.modified);

  await user.click(editor.getByRole("button", { name: y.restoreDefaults }));
  assert.equal(modified(editor).length, 0);
  assert.equal(spin(editor, f.idle_sec)?.value, "30");
  assert.equal(spin(editor, f.max_serve_sec)?.value, "120");
  assert.equal(spin(editor, f.unfinished_wait_sec)?.value, "30");
  assert.equal(spin(editor, f.hold_max_sec)?.value, "600");
  assert.equal(editor.getByRole("combobox", { name: f.sleep_level }).closest(".ant-select")?.textContent?.includes(y.sleepLevel.auto), true);
  assert.equal(saveButton(editor).disabled, true, "restored to what is saved");
  assert.equal((editor.getByRole("button", { name: y.restoreDefaults }) as HTMLButtonElement).disabled, true);
});

test("restore defaults on a saved non-default policy returns to the defaults and makes it savable", async () => {
  stub();
  const user = mount(withPolicy({ policy: "idle", idle_sec: 120, sleep_level: "l1", hold_max_sec: 30 }));
  const editor = await openEditor(user);
  assert.equal(modified(editor).length, 3);
  assert.equal(saveButton(editor).disabled, true);
  await user.click(editor.getByRole("button", { name: y.restoreDefaults }));
  assert.equal(spin(editor, f.idle_sec)?.value, "60");
  assert.equal(spin(editor, f.hold_max_sec)?.value, "300");
  assert.equal(modified(editor).length, 0);
  assert.equal(saveButton(editor).disabled, false, "the defaults differ from what is saved");
  await user.click(saveButton(editor));
  await waitFor(() => assert.deepEqual(state.sent, [[1, defaults.idle]]));
});

test("the unfinished-request handling: abort or wait, and the wait time shows only while waiting", async () => {
  stub();
  const user = mount(withPolicy(defaults.timeslice));
  const editor = await openEditor(user);
  const group = editor.getByRole("radiogroup", { name: f.unfinished });
  assert.deepEqual(within(group).getAllByRole("radio").map((radio) => radio.closest("label")?.textContent), [y.unfinished.abort, y.unfinished.wait]);
  assert.equal(y.unfinished.abort, "Cut off at once");
  assert.equal((editor.getByRole("radio", { name: y.unfinished.wait }) as HTMLInputElement).checked, true);
  assert.equal(spin(editor, f.unfinished_wait_sec)?.value, "30");

  await user.click(editor.getByRole("radio", { name: y.unfinished.abort }));
  assert.equal(spin(editor, f.unfinished_wait_sec), null);
  await user.click(editor.getByRole("radio", { name: y.unfinished.wait }));
  assert.equal(spin(editor, f.unfinished_wait_sec)?.value, "30", "back to the policy's default wait");

  // anytime starts on abort; turning to wait brings the wait time with the default.
  await choosePolicy(user, editor, "anytime");
  assert.equal(spin(editor, f.unfinished_wait_sec), null);
  await user.click(editor.getByRole("radio", { name: y.unfinished.wait }));
  assert.equal(spin(editor, f.unfinished_wait_sec)?.value, "30");
  assert.equal(modified(editor).length, 1, "only unfinished differs");
});

test("the fields are labelled in the user's language, with their help", async () => {
  stub();
  const editor = await openEditor(mount(withPolicy(defaults.timeslice)));
  for (const field of ["idle_sec", "max_serve_sec", "unfinished", "sleep_level", "hold_max_sec"] as const) editor.getByRole("group", { name: f[field] });
  editor.getByText(y.help.idle_sec);
  editor.getByText(y.help.max_serve_sec);
  editor.getByText(y.help.hold_max_sec);
  editor.getByText(i18n.t("deployments:yieldPolicy.range", { minimum: 15, maximum: 120 }));
  editor.getByText(i18n.t("deployments:yieldPolicy.range", { minimum: 30, maximum: 86400 }));
  editor.getByText(i18n.t("deployments:yieldPolicy.range", { minimum: 10, maximum: 3600 }));
  editor.getByText(i18n.t("deployments:yieldPolicy.range", { minimum: 1, maximum: 600 }));
  // The fields are listed in the order the operator thinks of them.
  const order = editor.getAllByRole("group").map((group) => group.getAttribute("aria-label"));
  assert.deepEqual(order.filter((name) => [f.idle_sec, f.max_serve_sec, f.unfinished, f.sleep_level, f.hold_max_sec].includes(name ?? "")), [f.idle_sec, f.max_serve_sec, f.unfinished, f.sleep_level, f.hold_max_sec]);
});
