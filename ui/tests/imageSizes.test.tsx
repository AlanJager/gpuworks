import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { dom } from "./helpers/dom.ts";
import {
  catalogRoutes,
  i18n,
  installFetch,
  json,
  systemVersion,
  zhImages,
  type Handler,
} from "./helpers/imagesHarness.ts";
import { I18nextProvider } from "react-i18next";

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { message } = await import("antd");
const { default: ImagesPage } = await import("../src/pages/ImagesPage.tsx");

const z = zhImages;
let restoreFetch = () => {};
afterEach(() => {
  cleanup();
  message.destroy();
  restoreFetch();
});

function renderPage(...handlers: Handler[]) {
  const stub = installFetch(async (call) => {
    for (const handler of handlers) {
      const res = await handler(call);
      if (res) return res;
    }
    return catalogRoutes(call);
  });
  restoreFetch = stub.restore;
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/images"]}><ImagesPage /></MemoryRouter></I18nextProvider>);
  return { calls: stub.calls, user: userEvent.setup({ document: dom.window.document }) };
}


function summary(key: string) {
  return screen.getByText(z.page.summary[key]).closest(".ant-card") as HTMLElement;
}

for (const scenario of [
  {
    name: "unknown catalog and local sizes",
    versions: [{ ...systemVersion, size_bytes: undefined, present_nodes: 2, local_runtime_size_bytes: undefined, local_size_known_nodes: undefined }],
    catalog: "—", local: "—", missing: 2,
  },
  {
    name: "measured local subtotal never multiplies archive size",
    versions: [{ ...systemVersion, size_bytes: 1024, present_nodes: 3, local_runtime_size_bytes: 8192, local_size_known_nodes: 2 }],
    catalog: "1 KiB", local: "8 KiB", missing: 1,
  },
  {
    name: "measured zero is known",
    versions: [{ ...systemVersion, size_bytes: undefined, present_nodes: 1, local_runtime_size_bytes: 0, local_size_known_nodes: 1 }],
    catalog: "—", local: "0 B", missing: 0,
  },
  { name: "empty catalog is zero", versions: [], catalog: "0 B", local: "0 B", missing: 0 },
]) {
  test(scenario.name, async () => {
    renderPage((call) => call.path === "/api/v1/images/versions" ? json(200, { items: scenario.versions }) : undefined);
    await waitFor(() => assert.ok(summary("localSize").textContent?.includes(scenario.local)));
    await waitFor(() => assert.ok(summary("catalogSize").textContent?.includes(scenario.catalog)));
    if (scenario.missing) {
      await waitFor(() => assert.ok(summary("localSize").textContent?.includes(
        i18n.t("page.summary.unknownSizes", { ns: "images", count: scenario.missing }),
      )));
    }
  });
}

test("pending and failed version summaries do not claim zero", async () => {
  let resolve!: (response: Response) => void;
  renderPage((call) => call.path === "/api/v1/images/versions"
    ? new Promise<Response>((done) => { resolve = done; }) : undefined);
  await waitFor(() => assert.ok(resolve));
  assert.ok(summary("localSize").textContent?.includes("—"));
  assert.ok(summary("catalogSize").textContent?.includes("—"));
  resolve(json(500, { error: "inventory unavailable" }));
  await waitFor(() => assert.ok(screen.getByRole("alert").textContent));
  assert.ok(summary("localSize").textContent?.includes("—"));
});
