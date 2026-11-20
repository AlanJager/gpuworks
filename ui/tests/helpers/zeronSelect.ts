// Drives a Zeron Select (ui/src/components/ui/select.tsx) the way a keyboard
// user does: focus the trigger, ArrowDown opens the list with the first option
// highlighted, ArrowDown moves on, Enter picks. jsdom's bare click carries no
// pointer press for Base UI, so neither the trigger nor an option is clicked.
// Testing Library is loaded on call: test files set up the DOM globals first.
import assert from "node:assert/strict";

const rtl = () => import("@testing-library/react");
const keyboard = async (keys: string) => (await import("@testing-library/user-event")).default.setup().keyboard(keys);
const highlighted = () => document.activeElement?.getAttribute("role") === "option" ? document.activeElement as HTMLElement : null;

/** Opens the select and returns the labels it offers, top to bottom. */
export async function openSelect(trigger: HTMLElement): Promise<string[]> {
  const { screen, waitFor } = await rtl();
  trigger.focus();
  await keyboard("{ArrowDown}");
  await waitFor(() => assert.ok(highlighted(), "the list opens with an option highlighted"));
  return screen.getAllByRole("option").map((option) => option.textContent ?? "");
}

/** Picks the option that reads `label`; the select must offer it. */
export async function chooseOption(trigger: HTMLElement, label: string) {
  const { waitFor } = await rtl();
  const labels = await openSelect(trigger);
  assert.ok(labels.includes(label), `"${label}" is offered among ${JSON.stringify(labels)}`);
  for (let step = 0; step < labels.length && highlighted()?.textContent !== label; step++) {
    const before = highlighted();
    await keyboard("{ArrowDown}");
    await waitFor(() => assert.notEqual(highlighted(), before));
  }
  assert.equal(highlighted()?.textContent, label);
  await keyboard("{Enter}");
}
