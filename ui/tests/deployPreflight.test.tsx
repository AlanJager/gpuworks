import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// The preflight RPC's orchestration is gone from the form: the multi-node
// summary arrives on the evaluate response and renders read-only. These cases
// render the published fixture shapes and assert what the deployer sees.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", {
  url: "http://localhost/",
});
for (const name of [
  "window",
  "document",
  "navigator",
  "HTMLElement",
  "HTMLInputElement",
  "SVGElement",
  "Element",
  "Node",
  "MutationObserver",
  "ShadowRoot",
] as const) {
  Object.defineProperty(globalThis, name, {
    value: dom.window[name],
    configurable: true,
  });
}
Object.defineProperty(globalThis, "getComputedStyle", {
  value: (element: Element) => dom.window.getComputedStyle(element),
  configurable: true,
});
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: () => ({
    matches: false,
    addListener() {},
    removeListener() {},
    addEventListener() {},
    removeEventListener() {},
    dispatchEvent() {
      return false;
    },
  }),
});
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
Object.defineProperty(globalThis, "ResizeObserver", {
  value: ResizeObserverStub,
  configurable: true,
});

const deployLocale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/deploy.json", import.meta.url),
    "utf8",
  ),
);
const commonLocale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/common.json", import.meta.url),
    "utf8",
  ),
);
const i18n = createInstance();
await i18n
  .use(initReactI18next)
  .init({
    lng: "en",
    fallbackLng: "en",
    resources: { en: { deploy: deployLocale, common: commonLocale } },
    defaultNS: "common",
    interpolation: { escapeValue: false },
  });

const { cleanup, render, screen, waitFor } =
  await import("@testing-library/react");
const { Form } = await import("antd");
const DeployResourceStep = (
  await import("../src/pages/deploy/DeployResourceStep.tsx")
).default;

const multiNodeFixture = JSON.parse(
  fs.readFileSync(
    new URL("./fixtures/evaluate/multi_node.json", import.meta.url),
    "utf8",
  ),
) as {
  preflight?: {
    ok: boolean;
    checks?: Array<{ code?: string; message?: string }>;
  };
};
assert.equal(
  multiNodeFixture.preflight?.ok,
  true,
  "the captured multi-node draft preflights clean",
);
assert.equal(
  (multiNodeFixture.preflight?.checks ?? []).length,
  0,
  "the captured clean summary carries no checks",
);

function PreflightHost({
  preflight,
}: {
  preflight?: {
    ok: boolean;
    checks?: Array<{ code?: string; message?: string }>;
  };
}) {
  return (
    <I18nextProvider i18n={i18n}>
      <Form layout="vertical" initialValues={{ nnodes: 2 }}>
        <DeployResourceStep nodes={[]} preflight={preflight} result={null} />
      </Form>
    </I18nextProvider>
  );
}

afterEach(cleanup);
after(() => {
  closeChannels();
  dom.window.close();
});

test("a clean multi-node draft renders the passing preflight tag", async () => {
  render(<PreflightHost preflight={multiNodeFixture.preflight} />);
  await waitFor(() =>
    assert.ok(screen.getByText(deployLocale.resource.preflightPassed)),
  );
});

test("a failing summary names its checks", async () => {
  render(
    <PreflightHost
      preflight={{
        ok: false,
        checks: [
          {
            code: "PREFLIGHT_DUPLICATE_NODE",
            message: "duplicate node node-a",
          },
        ],
      }}
    />,
  );
  await waitFor(() =>
    assert.ok(screen.getByText(deployLocale.resource.preflightFailed)),
  );
  assert.ok(
    screen.getByText("duplicate node node-a"),
    "the concrete rule that failed is visible",
  );
});

test("a draft without a preflight answer renders no verdict", async () => {
  render(<PreflightHost />);
  assert.equal(screen.queryByText(deployLocale.resource.preflightPassed), null);
  assert.equal(screen.queryByText(deployLocale.resource.preflightFailed), null);
});
