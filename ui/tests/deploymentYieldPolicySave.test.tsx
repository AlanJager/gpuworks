import assert from "node:assert/strict";
import { test } from "node:test";
import {
  APIError, api, cleanup, deployment, f, fireEvent, held, i18n, mount, openEditor, p, presets, radio, saveButton, screen, serverAnswer, spin, state, stub, summaryOf, summaryText, userEvent, waitFor, withPolicy, within, y,
} from "./helpers/yieldPolicyHarness.tsx";

// What the yield policy editor saves and how it answers: the request as literal
// JSON, success, a save in flight, the ranges, the server's refusals, the
// deployments that cannot yield or sleep, a read-only user and the keyboard.

test("saving shows success, reloads the deployment, restarts the editor from what is saved and updates the summary", async () => {
  stub();
  const user = mount();
  const editor = await openEditor(user);
  await user.click(radio(editor, y.preset.name.shared));
  await user.click(saveButton(editor));
  await waitFor(() => assert.equal(state.sent.length, 1));
  await waitFor(() => assert.equal(state.changed, 1));
  assert.ok((await screen.findAllByText(y.saved)).length > 0);
  await waitFor(() => assert.ok((screen.getByRole("region", { name: p.title }).textContent ?? "").includes(summaryOf(y.preset.name.shared))));
  const again = within(await screen.findByRole("region", { name: y.title }));
  assert.equal(radio(again, y.preset.name.shared).checked, true);
  assert.equal(saveButton(again).disabled, true, "what is saved is not a change");
});

test("a save in flight locks the form and shows it is working", async () => {
  stub();
  let finish: () => void = () => {};
  api.setYieldPolicy = (id, body) => new Promise((resolve) => {
    state.sent.push([id, body]);
    finish = () => { const answer = serverAnswer(body); state.current = { ...state.current, yield_policy: answer }; resolve(answer); };
  });
  const user = mount();
  const editor = await openEditor(user);
  await user.click(radio(editor, y.preset.name.online));
  await user.click(saveButton(editor));
  await waitFor(() => assert.equal(state.sent.length, 1));
  assert.ok(saveButton(editor).className.includes("ant-btn-loading") || saveButton(editor).disabled);
  assert.equal(radio(editor, y.preset.name.offline).disabled, true);
  assert.equal(spin(editor, f.idle_sec)?.disabled, true);
  assert.equal(state.changed, 0, "nothing is saved yet");
  finish();
  await waitFor(() => assert.equal(state.changed, 1));
});

test("a value out of its range or a cleared one is flagged and cannot be saved", async () => {
  stub();
  const user = mount(withPolicy(presets.shared));
  const editor = await openEditor(user);
  const ranges: Array<[string, string, number, number]> = [
    [f.idle_sec, "29", 30, 604800],
    [f.min_serve_sec, "3601", 0, 3600],
    [f.unfinished_wait_sec, "0", 1, 600],
    [f.hold_max_sec, "1801", 10, 1800],
  ];
  for (const [label, bad, minimum, maximum] of ranges) {
    const input = spin(editor, label) as HTMLInputElement;
    const good = input.value;
    fireEvent.change(input, { target: { value: bad } });
    editor.getByText(i18n.t("deployments:yieldPolicy.error.out_of_range", { field: label, minimum, maximum }));
    assert.equal(input.getAttribute("aria-invalid"), "true", label);
    assert.equal(saveButton(editor).disabled, true, label);
    fireEvent.change(input, { target: { value: good } });
    assert.equal(editor.queryByRole("alert"), null, label);
  }
  const idle = spin(editor, f.idle_sec) as HTMLInputElement;
  fireEvent.change(idle, { target: { value: "" } });
  editor.getByText(i18n.t("deployments:yieldPolicy.error.required", { field: f.idle_sec }));
  assert.equal(saveButton(editor).disabled, true);
  await user.click(saveButton(editor));
  assert.deepEqual(state.sent, []);
  // The boundaries are accepted.
  fireEvent.change(idle, { target: { value: "30" } });
  assert.equal(saveButton(editor).disabled, false);
  fireEvent.change(idle, { target: { value: "604800" } });
  assert.equal(saveButton(editor).disabled, false);
  // A field that does not apply is not checked.
  await user.click(radio(editor, y.preset.name.offline));
  assert.equal(spin(editor, f.idle_sec), null);
});

test("the server's refusals are shown in the user's language and nothing is marked saved", async () => {
  stub();
  const refusals: Array<[APIError, string]> = [
    [
      new APIError("YIELD_POLICY_INVALID", { field: "hold_max_sec", reason: "out_of_range", minimum: 10, maximum: 1800 }, "out of range", 400),
      i18n.t("deployments:yieldPolicy.error.out_of_range", { field: f.hold_max_sec, minimum: 10, maximum: 1800 }),
    ],
    [
      new APIError("YIELD_POLICY_INVALID", { field: "unfinished", reason: "invalid_value" }, "bad value", 400),
      i18n.t("deployments:yieldPolicy.error.invalid_value", { field: f.unfinished }),
    ],
    [
      new APIError("YIELD_POLICY_INVALID", { field: "idle_sec", reason: "required" }, "required", 400),
      i18n.t("deployments:yieldPolicy.error.required", { field: f.idle_sec }),
    ],
    [
      new APIError("YIELD_POLICY_IDLE_SHARED", { deployment: "other-chat" }, "another idle-policy deployment shares its GPUs", 409),
      i18n.t("common:apiError.YIELD_POLICY_IDLE_SHARED", { deployment: "other-chat" }),
    ],
    [new APIError("YIELD_POLICY_SHAPE", {}, "single-node only", 400), i18n.t("common:apiError.YIELD_POLICY_SHAPE")],
    [new APIError("SLEEP_NOT_ENABLED", {}, "sleep mode was not enabled", 400), i18n.t("common:apiError.SLEEP_NOT_ENABLED")],
  ];
  for (const [error, detail] of refusals) {
    api.setYieldPolicy = async () => { throw error; };
    const user = mount();
    const editor = await openEditor(user);
    await user.click(radio(editor, y.preset.name.online));
    await user.click(saveButton(editor));
    const alert = await screen.findByText(i18n.t("deployments:yieldPolicy.saveFailed", { detail }));
    assert.ok(alert, error.code);
    assert.equal(state.changed, 0, error.code);
    // The form stays as it was, ready to be corrected.
    assert.equal(radio(editor, y.preset.name.online).checked, true);
    assert.ok((await summaryText()).includes(summaryOf(y.preset.name.resident)), "the summary still shows what is saved");
    // Changing anything clears the refusal.
    await userEvent.setup().click(radio(editor, y.preset.name.offline));
    assert.equal(screen.queryByText(i18n.t("deployments:yieldPolicy.saveFailed", { detail })), null);
    cleanup();
  }
});

test("a refusal of the server that the editor did not expect still reads in the user's language (YIELD_POLICY_INVALID, unknown field)", async () => {
  stub();
  api.setYieldPolicy = async () => { throw new APIError("YIELD_POLICY_INVALID", { field: "something_new", reason: "unheard_of" }, "bad", 400); };
  const user = mount();
  const editor = await openEditor(user);
  await user.click(radio(editor, y.preset.name.online));
  await user.click(saveButton(editor));
  await screen.findByText(i18n.t("deployments:yieldPolicy.saveFailed", { detail: i18n.t("common:apiError.YIELD_POLICY_INVALID", { field: "something_new", reason: "unheard_of" }) }));
});

test("a deployment that cannot sleep says so and offers only the way back to resident", async () => {
  stub();
  const user = mount(deployment({ sleepable: false, yield_policy: held(presets.shared) }));
  const editor = await openEditor(user);
  editor.getByText(y.notSleepable);
  assert.equal(radio(editor, y.preset.name.shared).checked, true);
  for (const name of ["online", "shared", "offline"] as const) assert.equal(radio(editor, y.preset.name[name]).disabled, true, name);
  assert.equal(radio(editor, y.preset.name.resident).disabled, false);
  // The policy select offers never yield only.
  await user.click(editor.getByRole("combobox", { name: y.policyLabel }));
  const option = (policy: keyof typeof y.policy.name) => screen.getByText(`${y.policy.name[policy]} - ${y.policy.description[policy]}`).closest(".ant-select-item") as HTMLElement;
  for (const policy of ["idle", "timeslice", "anytime"] as const) assert.ok(option(policy).className.includes("ant-select-item-option-disabled"), policy);
  assert.ok(!option("never").className.includes("ant-select-item-option-disabled"));
  await user.keyboard("{Escape}");
  await user.click(radio(editor, y.preset.name.resident));
  await user.click(saveButton(editor));
  await waitFor(() => assert.equal(JSON.stringify(state.sent), '[[1,{"policy":"never","auto_sleep":false}]]'));
  cleanup();

  // Without a policy kept, it cannot turn automatic sleep on either.
  const user2 = mount(deployment({ sleepable: false, yield_policy: held({ policy: "never", auto_sleep: true, idle_sec: 600, sleep_level: "l1", hold_max_sec: 120 }) }));
  const editor2 = await openEditor(user2);
  const toggle = editor2.getByRole("switch", { name: f.auto_sleep }) as HTMLButtonElement;
  assert.equal(toggle.disabled, false, "a kept automatic sleep can be turned off");
  await user2.click(toggle);
  assert.equal(radio(editor2, y.preset.name.resident).checked, true);
  assert.equal((editor2.getByRole("switch", { name: f.auto_sleep }) as HTMLButtonElement).disabled, true, "and cannot be turned on again");
});

test("a multi-node or PD deployment can only never yield, but can sleep by itself", async () => {
  stub();
  for (const shape of [{ nnodes: 2 }, { pd: { mode: "ep", roles: [] } }]) {
    const user = mount(deployment(shape));
    const editor = await openEditor(user);
    editor.getByText(y.singleNodeOnly);
    for (const name of ["online", "shared", "offline"] as const) assert.equal(radio(editor, y.preset.name[name]).disabled, true, name);
    const toggle = editor.getByRole("switch", { name: f.auto_sleep }) as HTMLButtonElement;
    assert.equal(toggle.disabled, false);
    await user.click(toggle);
    await user.click(saveButton(editor));
    await waitFor(() => assert.equal(state.sent.length, 1));
    assert.equal(JSON.stringify(state.sent[0][1]), '{"policy":"never","auto_sleep":true,"idle_sec":300,"sleep_level":"auto","hold_max_sec":120}');
    state.sent.length = 0;
    cleanup();
  }
  // A single-node deployment that can sleep has no such notice.
  const user = mount();
  const editor = await openEditor(user);
  assert.equal(editor.queryByText(y.singleNodeOnly), null);
  assert.equal(editor.queryByText(y.notSleepable), null);
});

test("a read-only user sees the service level but cannot edit it", async () => {
  stub();
  state.role = "viewer";
  mount(withPolicy(presets.online));
  assert.ok((await summaryText()).includes(summaryOf(y.preset.name.online)));
  await waitFor(() => assert.equal(screen.queryByRole("button", { name: p.edit }), null));
});

test("the editor is operable from the keyboard: arrow keys on the presets, space on the switch, the select, then save", async () => {
  stub();
  const user = mount();
  const editor = await openEditor(user);
  // Arrow keys walk the presets; custom, being disabled, is skipped.
  radio(editor, y.preset.name.resident).focus();
  await user.keyboard("{ArrowDown}");
  assert.equal(radio(editor, y.preset.name.online).checked, true);
  assert.equal(spin(editor, f.idle_sec)?.value, "300");
  await user.keyboard("{ArrowDown}");
  assert.equal(radio(editor, y.preset.name.shared).checked, true);
  await user.keyboard("{ArrowUp}{ArrowUp}");
  assert.equal(radio(editor, y.preset.name.resident).checked, true);
  assert.equal(saveButton(editor).disabled, true);

  // Tab moves on to the policy select, which opens and picks with the keys.
  await user.tab();
  const policy = editor.getByRole("combobox", { name: y.policyLabel });
  assert.equal(document.activeElement, policy);
  // The select reads key codes (which jsdom does not fill in for typed keys), so its keys are state.sent as events.
  const press = (name: "ArrowDown" | "Enter") => fireEvent.keyDown(policy, { key: name, code: name, keyCode: name === "Enter" ? 13 : 40, which: name === "Enter" ? 13 : 40 });
  press("ArrowDown");
  await screen.findAllByRole("option", { hidden: true });
  press("ArrowDown");
  press("Enter");
  await waitFor(() => assert.equal(radio(editor, y.preset.name.online).checked, true));

  // Space and Enter flip the switch.
  const toggle = editor.getByRole("switch", { name: f.auto_sleep });
  toggle.focus();
  await user.keyboard(" ");
  assert.equal(toggle.getAttribute("aria-checked"), "true");
  await user.keyboard("{Enter}");
  assert.equal(toggle.getAttribute("aria-checked"), "false");
  assert.equal(radio(editor, y.preset.name.online).checked, true);

  // The number field takes typed digits, and the save button is reached and pressed with the keyboard.
  const hold = spin(editor, f.hold_max_sec) as HTMLInputElement;
  hold.focus();
  await user.keyboard("{Control>}a{/Control}200");
  assert.equal(radio(editor, y.preset.name.custom).checked, true);
  const save = saveButton(editor);
  save.focus();
  await user.keyboard("{Enter}");
  await waitFor(() => assert.equal(JSON.stringify(state.sent), '[[1,{"policy":"idle","auto_sleep":false,"idle_sec":300,"sleep_level":"auto","hold_max_sec":200}]]'));
  await waitFor(() => assert.equal(state.changed, 1));
});
