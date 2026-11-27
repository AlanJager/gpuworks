import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n } = await createI18n([]);
const { cleanup, render, screen } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { PasswordInput } = await import("../src/ui-kit/PasswordInput.tsx");

afterEach(() => cleanup());

const mount = (props: React.ComponentProps<typeof PasswordInput> = {}, ref?: React.Ref<HTMLInputElement>) =>
  render(<I18nextProvider i18n={i18n}><label>Password<PasswordInput ref={ref} {...props} /></label></I18nextProvider>);

test("the password is hidden until the viewer asks, and hidden again on a second press", async () => {
  const user = userEvent.setup();
  mount({ autoComplete: "new-password" });
  const input = screen.getByLabelText("Password") as HTMLInputElement;
  assert.equal(input.type, "password");
  assert.equal(input.autocomplete, "new-password");

  const show = screen.getByRole("button", { name: "Show password" });
  assert.equal(show.getAttribute("aria-pressed"), "false");
  await user.type(input, "s3cret-pass");
  await user.click(show);
  assert.equal(input.type, "text");
  assert.equal(input.value, "s3cret-pass", "showing keeps what was typed");
  const hide = screen.getByRole("button", { name: "Hide password" });
  assert.equal(hide.getAttribute("aria-pressed"), "true");

  hide.focus();
  await user.keyboard("{Enter}");
  assert.equal(input.type, "password");
  assert.ok(screen.getByRole("button", { name: "Show password" }));
});

test("a disabled password field cannot be revealed, and its ref is the input", () => {
  const ref = React.createRef<HTMLInputElement>();
  mount({ disabled: true }, ref);
  assert.equal((screen.getByRole("button", { name: "Show password" }) as HTMLButtonElement).disabled, true);
  assert.equal(ref.current, screen.getByLabelText("Password"));
  assert.equal(ref.current?.disabled, true);
});

// GWKS-540: a revealed secret is plain text to the browser, so spellcheck
// (enhanced spellcheck uploads it), autocapitalisation and autocorrection stay
// off whether it is hidden or shown, unless the caller asks otherwise.
const writingAids = (input: HTMLInputElement) => ({
  spellcheck: input.getAttribute("spellcheck"),
  autocapitalize: input.getAttribute("autocapitalize"),
  autocorrect: input.getAttribute("autocorrect"),
});

test("the browser's writing aids stay off while the password is hidden and once it is shown", async () => {
  const user = userEvent.setup();
  mount();
  const input = screen.getByLabelText("Password") as HTMLInputElement;
  const off = { spellcheck: "false", autocapitalize: "off", autocorrect: "off" };
  assert.equal(input.type, "password");
  assert.deepEqual(writingAids(input), off);

  await user.click(screen.getByRole("button", { name: "Show password" }));
  assert.equal(input.type, "text");
  assert.deepEqual(writingAids(input), off);
});

test("a caller can turn the writing aids back on", () => {
  mount({ spellCheck: true, autoCapitalize: "on", autoCorrect: "on" });
  const input = screen.getByLabelText("Password") as HTMLInputElement;
  assert.deepEqual(writingAids(input), { spellcheck: "true", autocapitalize: "on", autocorrect: "on" });
});
