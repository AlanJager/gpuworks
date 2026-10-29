import assert from "node:assert/strict";
import { test } from "node:test";
import {
  type Editor, choosePolicy, controls, cleanup, deployment, f, fireEvent, held, i18n, mount, openEditor, p, presets, radio, saveButton, screen, spin, state, stub, summaryOf, summaryText, userEvent, waitFor, withPolicy, within, y,
} from "./helpers/yieldPolicyHarness.tsx";

// The service level and sleep part of a deployment's scheduling policy card: when the card
// shows it, what its summary says, and the editor behind it - the preset radios,
// the yield policy select, the parameters that apply to each policy and the
// preset <-> custom derivation. Saving, the server's refusals, the restrictions
// and the keyboard are in deploymentYieldPolicySave.test.tsx.

test("the summary names each preset, and a deployment with no policy counts as resident", async () => {
  stub();
  for (const name of ["resident", "online", "shared", "offline"] as const) {
    mount(withPolicy(presets[name]));
    assert.ok((await summaryText()).includes(summaryOf(y.preset.name[name])), name);
    cleanup();
  }
  mount(deployment());
  assert.ok((await summaryText()).includes(summaryOf(y.preset.name.resident)));
});

test("the part shows for a deployment that can sleep, and for any that kept a policy besides resident", async () => {
  stub();
  mount(deployment({ sleepable: false }));
  await screen.findByRole("region", { name: p.title });
  assert.ok(!(await summaryText()).includes(y.summaryLabel));
  cleanup();
  // A multi-node or PD deployment that can sleep can still sleep by itself.
  mount(deployment({ nnodes: 2 }));
  assert.ok((await summaryText()).includes(summaryOf(y.preset.name.resident)));
  cleanup();
  mount(deployment({ pd: { mode: "ep", roles: [] } }));
  assert.ok((await summaryText()).includes(summaryOf(y.preset.name.resident)));
  cleanup();
  // One that kept a policy from when it could still shows it, to go back.
  mount(deployment({ sleepable: false, yield_policy: held(presets.offline) }));
  assert.ok((await summaryText()).includes(summaryOf(y.preset.name.offline)));
  cleanup();
  mount(deployment({ sleepable: false, yield_policy: held({ policy: "never", auto_sleep: true, idle_sec: 600, sleep_level: "l1", hold_max_sec: 120 }) }));
  assert.ok((await summaryText()).includes(summaryOf(i18n.t("deployments:yieldPolicy.summary.customDigest", {
    policy: y.policy.name.never,
    digest: [i18n.t("deployments:yieldPolicy.summary.idle", { value: "10 min" }), y.summary.level.l1, y.summary.autoSleep].join(y.summary.separator),
  }))));
});

test("the editor offers the four presets and custom, the policy and the switch; custom is a result, never a choice", async () => {
  stub();
  const user = mount();
  const editor = await openEditor(user);
  assert.equal(editor.getAllByRole("radio").length, 5, "four presets and custom");
  for (const name of ["resident", "online", "shared", "offline"] as const) {
    assert.equal(radio(editor, y.preset.name[name]).disabled, false, name);
  }
  assert.equal(radio(editor, y.preset.name.custom).disabled, true, "custom cannot be clicked");
  assert.equal(radio(editor, y.preset.name.resident).checked, true);
  assert.equal(radio(editor, y.preset.name.custom).checked, false);
  editor.getByText(y.preset.description.resident);
  assert.equal(saveButton(editor).disabled, true, "nothing changed yet");
  await user.click(radio(editor, y.preset.name.custom));
  assert.equal(radio(editor, y.preset.name.resident).checked, true, "clicking custom changes nothing");
});

test("which controls show, for every policy with automatic sleep off and on", async () => {
  stub();
  const base = { sleep_level: "auto", hold_max_sec: 120, idle_sec: 300 } as const;
  const cases: Array<{ policy: "never" | "idle" | "timeslice" | "anytime"; autoSleep: boolean; want: ReturnType<typeof controls> }> = [];
  for (const policy of ["never", "idle", "timeslice", "anytime"] as const) {
    for (const autoSleep of [false, true]) {
      const yields = policy !== "never";
      const idle = policy === "idle" || policy === "timeslice" || autoSleep;
      const unfinished = policy === "timeslice" || policy === "anytime";
      cases.push({
        policy, autoSleep,
        want: {
          idle, idleDash: !idle, autoSleep: true, level: yields || autoSleep, minServe: policy === "timeslice",
          unfinished, wait: unfinished, hold: yields || autoSleep,
        },
      });
    }
  }
  for (const { policy, autoSleep, want } of cases) {
    const stored = {
      policy, auto_sleep: autoSleep, ...base,
      ...(policy === "timeslice" ? { min_serve_sec: 120 } : {}),
      ...(policy === "timeslice" || policy === "anytime" ? { unfinished: "wait", unfinished_wait_sec: 30 } : {}),
    } as Omit<YieldPolicy, "preset">;
    const user = mount(withPolicy(stored));
    const editor = await openEditor(user);
    assert.deepEqual(controls(editor), want, `${policy} / auto sleep ${autoSleep}`);
    cleanup();
  }
});

test("never yield without automatic sleep reads a dash for the idle time, and turning automatic sleep on gives it a field", async () => {
  stub();
  const user = mount();
  const editor = await openEditor(user);
  const idle = within(editor.getByRole("group", { name: f.idle_sec }));
  idle.getByText(y.none);
  assert.equal(spin(editor, f.idle_sec), null);
  assert.equal(editor.queryByRole("combobox", { name: f.sleep_level }), null);
  assert.equal(spin(editor, f.hold_max_sec), null);
  await user.click(editor.getByRole("switch", { name: f.auto_sleep }));
  assert.equal(spin(editor, f.idle_sec)?.value, "300");
  assert.equal(within(editor.getByRole("group", { name: f.idle_sec })).queryByText(y.none), null);
  editor.getByRole("combobox", { name: f.sleep_level });
  assert.equal(spin(editor, f.hold_max_sec)?.value, "120");
  // It is no longer a preset.
  assert.equal(radio(editor, y.preset.name.custom).checked, true);
  assert.equal(radio(editor, y.preset.name.resident).checked, false);
  editor.getByText(y.preset.description.custom);
  await user.click(editor.getByRole("switch", { name: f.auto_sleep }));
  assert.equal(radio(editor, y.preset.name.resident).checked, true, "off again, it is resident again");
});

test("choosing a preset fills the whole parameter set", async () => {
  stub();
  const user = mount();
  const editor = await openEditor(user);
  await user.click(radio(editor, y.preset.name.shared));
  assert.equal(radio(editor, y.preset.name.shared).checked, true);
  assert.equal(spin(editor, f.idle_sec)?.value, "60");
  assert.equal(spin(editor, f.min_serve_sec)?.value, "120");
  assert.equal(spin(editor, f.unfinished_wait_sec)?.value, "30");
  assert.equal(spin(editor, f.hold_max_sec)?.value, "600");
  assert.equal(radio(editor, y.unfinished.wait).checked, true);
  editor.getByText(y.preset.description.shared);
  // The policy select shows the policy of the preset.
  assert.ok(within(editor.getByRole("group", { name: y.policyLabel })).getByText(y.policy.name.timeslice));

  await user.click(radio(editor, y.preset.name.offline));
  assert.equal(spin(editor, f.idle_sec), null);
  assert.equal(spin(editor, f.min_serve_sec), null);
  assert.equal(spin(editor, f.hold_max_sec)?.value, "1800");
  assert.equal(radio(editor, y.unfinished.abort).checked, true);
  assert.equal(spin(editor, f.unfinished_wait_sec), null, "abort has no wait");

  await user.click(radio(editor, y.preset.name.online));
  assert.equal(spin(editor, f.idle_sec)?.value, "300");
  assert.equal(spin(editor, f.hold_max_sec)?.value, "120");
  assert.equal(editor.queryByRole("radiogroup", { name: f.unfinished }), null);

  // Choosing a preset after editing resets what was edited.
  fireEvent.change(spin(editor, f.hold_max_sec) as HTMLInputElement, { target: { value: "999" } });
  assert.equal(radio(editor, y.preset.name.custom).checked, true);
  await user.click(radio(editor, y.preset.name.online));
  assert.equal(spin(editor, f.hold_max_sec)?.value, "120");
  assert.equal(radio(editor, y.preset.name.online).checked, true);
});

test("changing a parameter of a preset makes the deployment custom, and changing it back makes it the preset again", async () => {
  stub();
  const user = mount(withPolicy(presets.online));
  const editor = await openEditor(user);
  assert.equal(radio(editor, y.preset.name.online).checked, true);
  const idle = spin(editor, f.idle_sec) as HTMLInputElement;
  fireEvent.change(idle, { target: { value: "301" } });
  assert.equal(radio(editor, y.preset.name.custom).checked, true);
  assert.equal(radio(editor, y.preset.name.online).checked, false);
  assert.equal(saveButton(editor).disabled, false);
  fireEvent.change(idle, { target: { value: "300" } });
  assert.equal(radio(editor, y.preset.name.online).checked, true);
  assert.equal(radio(editor, y.preset.name.custom).checked, false);
  assert.equal(saveButton(editor).disabled, true, "back to what is saved, nothing to save");

  // The sleep level is a parameter too.
  await user.click(editor.getByRole("combobox", { name: f.sleep_level }));
  await user.click(await screen.findByText(y.sleepLevel.l1));
  assert.equal(radio(editor, y.preset.name.custom).checked, true);
  await user.click(editor.getByRole("combobox", { name: f.sleep_level }));
  await user.click(await screen.findByText(y.sleepLevel.auto, { selector: ".ant-select-item-option-content" }));
  assert.equal(radio(editor, y.preset.name.online).checked, true);
});

test("choosing another policy fills that policy's defaults, which are its preset", async () => {
  stub();
  const user = mount();
  const editor = await openEditor(user);
  await choosePolicy(user, editor, "idle");
  assert.equal(radio(editor, y.preset.name.online).checked, true);
  assert.deepEqual(controls(editor), { idle: true, idleDash: false, autoSleep: true, level: true, minServe: false, unfinished: false, wait: false, hold: true });
  await choosePolicy(user, editor, "timeslice");
  assert.equal(radio(editor, y.preset.name.shared).checked, true);
  assert.equal(controls(editor).minServe, true);
  await choosePolicy(user, editor, "anytime");
  assert.equal(radio(editor, y.preset.name.offline).checked, true);
  await choosePolicy(user, editor, "never");
  assert.equal(radio(editor, y.preset.name.resident).checked, true);
  assert.equal(saveButton(editor).disabled, true);
  // Automatic sleep survives a change of policy.
  await user.click(editor.getByRole("switch", { name: f.auto_sleep }));
  await choosePolicy(user, editor, "idle");
  assert.equal((editor.getByRole("switch", { name: f.auto_sleep }) as HTMLButtonElement).getAttribute("aria-checked"), "true");
  assert.equal(radio(editor, y.preset.name.custom).checked, true);
});

test("the unfinished-request handling: abort, wait with a limit, wait without one and its warning", async () => {
  stub();
  const user = mount(withPolicy(presets.shared));
  const editor = await openEditor(user);
  const group = editor.getByRole("radiogroup", { name: f.unfinished });
  assert.equal(within(group).getAllByRole("radio").length, 3);
  assert.equal(radio(editor, y.unfinished.wait).checked, true);
  assert.equal(spin(editor, f.unfinished_wait_sec)?.value, "30");
  assert.equal(editor.queryByText(y.unfinished.waitUnboundedWarning), null);

  await user.click(radio(editor, y.unfinished.abort));
  assert.equal(spin(editor, f.unfinished_wait_sec), null);
  assert.equal(radio(editor, y.preset.name.custom).checked, true);

  await user.click(radio(editor, y.unfinished.wait_unbounded));
  editor.getByText(y.unfinished.waitUnboundedWarning);
  assert.equal(spin(editor, f.unfinished_wait_sec), null, "no limit, no seconds");
  await user.click(saveButton(editor));
  await waitFor(() => assert.equal(
    JSON.stringify(state.sent),
    '[[1,{"policy":"timeslice","auto_sleep":false,"idle_sec":60,"sleep_level":"auto","min_serve_sec":120,"unfinished":"wait_unbounded","hold_max_sec":600}]]',
  ));

  // The editor restarts from what is saved; a limited wait is back to 30 seconds.
  const again = within(await screen.findByRole("region", { name: y.title }));
  assert.equal(radio(again, y.unfinished.wait_unbounded).checked, true);
  await user.click(radio(again, y.unfinished.wait));
  assert.equal(spin(again, f.unfinished_wait_sec)?.value, "30");
  assert.equal(again.queryByText(y.unfinished.waitUnboundedWarning), null);
});

test("what is saved is the policy with the parameters that apply, as literal JSON", async () => {
  stub();
  const cases: Array<[string, (user: ReturnType<typeof userEvent.setup>, editor: Editor) => Promise<void>, string]> = [
    ["online preset", async (user, editor) => { await user.click(radio(editor, y.preset.name.online)); },
      '{"policy":"idle","auto_sleep":false,"idle_sec":300,"sleep_level":"auto","hold_max_sec":120}'],
    ["shared preset", async (user, editor) => { await user.click(radio(editor, y.preset.name.shared)); },
      '{"policy":"timeslice","auto_sleep":false,"idle_sec":60,"sleep_level":"auto","min_serve_sec":120,"unfinished":"wait","unfinished_wait_sec":30,"hold_max_sec":600}'],
    ["offline preset", async (user, editor) => { await user.click(radio(editor, y.preset.name.offline)); },
      '{"policy":"anytime","auto_sleep":false,"sleep_level":"auto","unfinished":"abort","hold_max_sec":1800}'],
    ["never with automatic sleep", async (user, editor) => {
      await user.click(editor.getByRole("switch", { name: f.auto_sleep }));
      fireEvent.change(spin(editor, f.idle_sec) as HTMLInputElement, { target: { value: "600" } });
      await user.click(editor.getByRole("combobox", { name: f.sleep_level }));
      await user.click(await screen.findByText(y.sleepLevel.l1));
    }, '{"policy":"never","auto_sleep":true,"idle_sec":600,"sleep_level":"l1","hold_max_sec":120}'],
    ["timeslice with edited numbers", async (user, editor) => {
      await user.click(radio(editor, y.preset.name.shared));
      fireEvent.change(spin(editor, f.idle_sec) as HTMLInputElement, { target: { value: "45" } });
      fireEvent.change(spin(editor, f.min_serve_sec) as HTMLInputElement, { target: { value: "0" } });
      fireEvent.change(spin(editor, f.unfinished_wait_sec) as HTMLInputElement, { target: { value: "600" } });
      fireEvent.change(spin(editor, f.hold_max_sec) as HTMLInputElement, { target: { value: "10" } });
    }, '{"policy":"timeslice","auto_sleep":false,"idle_sec":45,"sleep_level":"auto","min_serve_sec":0,"unfinished":"wait","unfinished_wait_sec":600,"hold_max_sec":10}'],
    ["parameters that stop applying are left out", async (user, editor) => {
      await user.click(radio(editor, y.preset.name.shared));
      fireEvent.change(spin(editor, f.min_serve_sec) as HTMLInputElement, { target: { value: "5" } });
      await choosePolicy(user, editor, "anytime");
    }, '{"policy":"anytime","auto_sleep":false,"sleep_level":"auto","unfinished":"abort","hold_max_sec":1800}'],
  ];
  for (const [name, act, want] of cases) {
    stub();
    const user = mount();
    const editor = await openEditor(user);
    await act(user, editor);
    await user.click(saveButton(editor));
    await waitFor(() => assert.equal(state.sent.length, 1), { timeout: 3000 });
    assert.equal(JSON.stringify(state.sent[0][1]), want, name);
    assert.equal(state.sent[0][0], 1);
    cleanup();
  }
});
