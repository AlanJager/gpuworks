import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n } = await createI18n(["deploy"], "en");
const { cleanup, render, screen } = await import("@testing-library/react");
const { default: DeployNoOperationsNotice } = await import("../src/pages/deploy/DeployNoOperationsNotice.tsx");
type Model = import("../src/api.ts").Model;

afterEach(() => cleanup());

type Operations = Array<{ type: import("../src/inferenceApi.ts").OperationType }>;
const mount = (model?: Model | null, templateOperations?: Operations) => render(<I18nextProvider i18n={i18n}><DeployNoOperationsNotice model={model} templateOperations={templateOperations} /></I18nextProvider>);

// A model with no declared operation deploys into a deployment the Playground
// does not list and the health check cannot ask; the form says so before it
// is deployed (T73).
test("a model that declares no operation is flagged before it is deployed", () => {
  mount({ id: 1, name: "plain", operations: [] } as unknown as Model);
  assert.equal(screen.getByRole("alert").textContent?.includes("This deployment will expose no inference API"), true);
  assert.ok(screen.getByText(/will not appear in the Playground/));
});

test("a model with an operation, or no model yet, shows nothing", () => {
  mount({ id: 2, name: "translate", operations: [{ type: "openai.completions", endpoint: "/v1/completions" }] } as unknown as Model);
  assert.equal(screen.queryByRole("alert"), null);
  cleanup();
  mount(null);
  assert.equal(screen.queryByRole("alert"), null);
});

// A model registered for a template deploy has no operations yet; the
// template spec's are written to it when the deployment is created, so the
// form must not warn then - only when the spec declares none either.
test("a template spec that declares operations covers a model that has none yet", () => {
  const fresh = { id: 4, name: "pfnet/plamo-2-translate", operations: [] } as unknown as Model;
  mount(fresh, [{ type: "openai.completions" }]);
  assert.equal(screen.queryByRole("alert"), null);
  cleanup();
  mount(fresh, []);
  assert.ok(screen.getByRole("alert"));
});
