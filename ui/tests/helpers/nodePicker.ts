// Drives the node picker (ui/src/nodes/NodePicker.tsx) the way someone using
// it does: the combobox named by its label opens from the keyboard (ArrowDown;
// jsdom's bare click event carries no pointer press for Base UI), lists the nodes
// as options, marks one that cannot be chosen aria-disabled, and a multiple
// picker stays open after a pick. Testing Library is loaded on call: test
// files set up the DOM globals first.
import assert from "node:assert/strict";

const rtl = () => import("@testing-library/react");

const matches = (option: HTMLElement, name: string | RegExp) => {
  const text = option.textContent ?? "";
  return typeof name === "string" ? text === name : name.test(text);
};

/** The options of the open node picker, top to bottom. */
export function nodeOptions(): HTMLElement[] {
  return [...document.querySelectorAll<HTMLElement>('[role="option"][data-slot="combobox-item"]')];
}

/** The open picker's option that reads so (a RegExp matches part of it), if listed. */
export function nodeOption(name: string | RegExp): HTMLElement | undefined {
  return nodeOptions().find((option) => matches(option, name));
}

/** Whether a listed node cannot be chosen. */
export function unavailable(option: HTMLElement | undefined): boolean {
  assert.ok(option, "the node is listed");
  return option.getAttribute("aria-disabled") === "true";
}

/** Opens the picker unless it is open already. */
export async function openNodePicker(box: HTMLElement) {
  const { fireEvent, waitFor } = await rtl();
  if (box.getAttribute("aria-expanded") === "true") return;
  box.focus();
  fireEvent.keyDown(box, { key: "ArrowDown", code: "ArrowDown" });
  await waitFor(() => assert.equal(box.getAttribute("aria-expanded"), "true", "the picker opened"));
}

/** Opens the picker and waits until it lists the node that reads so. */
export async function findNodeOption(box: HTMLElement, name: string | RegExp): Promise<HTMLElement> {
  const { waitFor } = await rtl();
  await openNodePicker(box);
  return waitFor(() => {
    const option = nodeOption(name);
    assert.ok(option, `node ${String(name)} offered`);
    return option;
  });
}

/**
 * Picks each node by name, in order, then closes a list a multiple picker
 * keeps open, as the user moves on: while it is open the rest of the page is
 * inert to assistive technology.
 */
export async function pickNodes(box: HTMLElement, ...names: (string | RegExp)[]) {
  const { fireEvent } = await rtl();
  for (const name of names) fireEvent.click(await findNodeOption(box, name));
  await closeNodePicker(box);
}

/** The names the picker shows as chosen: chips for a multiple picker, the input's text for a single one. */
export function chosenNodes(box: HTMLElement): string[] {
  const chips = box.closest('[data-slot="combobox-chips"]');
  if (chips) return [...chips.querySelectorAll('[data-slot="combobox-chip"]')].map((chip) => chip.textContent ?? "");
  const value = (box as HTMLInputElement).value;
  return value ? [value] : [];
}

/** Closes the picker with Escape if it is open. */
export async function closeNodePicker(box: HTMLElement) {
  const { fireEvent, waitFor } = await rtl();
  if (box.getAttribute("aria-expanded") !== "true") return;
  fireEvent.keyDown(box, { key: "Escape", code: "Escape" });
  await waitFor(() => assert.notEqual(box.getAttribute("aria-expanded"), "true", "the picker closed"));
}
