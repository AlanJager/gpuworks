import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const en = await createI18n(["deploy"], "en");
const zh = await createI18n(["deploy"], "zh");
const { cleanup, render, screen } = await import("@testing-library/react");
const { Form } = await import("antd");
const { default: DeployAdvancedSection } = await import("../src/pages/deploy/DeployAdvancedSection.tsx");

afterEach(() => cleanup());

const mount = (i18n: typeof en.i18n) =>
  render(
    <I18nextProvider i18n={i18n}>
      <Form>
        <DeployAdvancedSection advancedSpecs={[]} manualCommand={false} templateArgs={[]} />
      </Form>
    </I18nextProvider>,
  );

// The custom-args box is split like a shell command line, so a JSON value
// typed with double quotes loses them. The field's help says to single-quote it
// and shows the form that works.
test("the custom-args field tells the user to single-quote JSON values", () => {
  mount(en.i18n);
  assert.ok(screen.getByText(/split like a shell command line/));
  assert.ok(screen.getByText(/in single quotes/));
  assert.ok(screen.getByText(/--compilation-config '\{"cudagraph_mode":"FULL_DECODE_ONLY"\}'/));
  assert.equal(screen.queryByText(/as written/), null);
});

test("the same hint is shipped in Chinese", () => {
  mount(zh.i18n);
  assert.ok(screen.getByText(/按 shell 命令行拆分/));
  assert.ok(screen.getByText(/单引号/));
  assert.ok(screen.getByText(/--compilation-config '\{"cudagraph_mode":"FULL_DECODE_ONLY"\}'/));
});
