import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n, locale } = await createI18n(["nodes", "common"], "en");
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const { MemoryRouter } = await import("react-router-dom");
const apiModule = await import("../src/api.ts");
const NodePullProxy = (await import("../src/components/NodePullProxy.tsx")).default;

afterEach(() => cleanup());

const text = locale.nodes.pullProxy as Record<string, string>;
type View = import("../src/api.ts").NodePullProxy;

function stub(impl: () => Promise<View>) {
  const original = { nodePullProxy: apiModule.api.nodePullProxy, me: apiModule.api.me };
  apiModule.api.me = async () => ({ role: "admin" }) as never;
  apiModule.api.nodePullProxy = impl as never;
  return () => Object.assign(apiModule.api, original);
}
const show = () => render(<MemoryRouter><I18nextProvider i18n={i18n}><NodePullProxy nodeUUID="node-1" /></I18nextProvider></MemoryRouter>);

test("the entry the node uses is shown with its addresses, user and node requirements", async () => {
  const restore = stub(async () => ({
    proxy: {
      id: 4, position: 1, http_proxy: "http://proxy.corp:3128", https_proxy: "http://proxy.corp:3129", no_proxy: "10.0.0.0/8",
      username: "svc-pull", has_password: true, control_plane: false, note: "", node_requirements: { site: ["idc-a", "idc-b"] }, matched_nodes: 2,
    },
    runtime: { driver: "nerdctl" },
  }));
  try {
    show();
    assert.ok(document.querySelector(".ant-skeleton"), "a loading placeholder shows first");
    await screen.findByText(text.entry.replace("{{n}}", "2"));
    assert.ok(screen.getByText("http://proxy.corp:3128"));
    assert.ok(screen.getByText("http://proxy.corp:3129"));
    assert.ok(screen.getByText("10.0.0.0/8"));
    assert.ok(screen.getByText(text.user.replace("{{user}}", "svc-pull")));
    assert.ok(screen.getByText("site=idc-a | idc-b"));
    assert.equal(document.body.textContent?.includes("password"), false, "no credential is shown");
    const link = screen.getByRole("link", { name: text.settingsLink });
    assert.equal(link.getAttribute("href"), "/settings#pull-settings");
    assert.ok(screen.getByText(locale.nodes.runtimeProxy.driver.replace("{{driver}}", "nerdctl")));
  } finally {
    restore();
  }
});

test("a node matching no entry says its agent uses its own environment", async () => {
  const restore = stub(async () => ({ proxy: null, runtime: { driver: "docker" } }));
  try {
    show();
    await screen.findByText(text.noneDetail);
    assert.ok(screen.getByText(text.none));
    assert.equal(screen.queryByText(text.httpProxy + ":"), null);
  } finally {
    restore();
  }
});

test("a failed load shows the error and nothing else", async () => {
  const restore = stub(async () => { throw new Error("node unreachable"); });
  try {
    show();
    await screen.findByText(text.loadFailed);
    await waitFor(() => assert.ok(screen.getByText("node unreachable")));
    assert.equal(screen.queryByText(text.noneDetail), null);
    assert.equal(document.querySelector(".ant-skeleton"), null);
  } finally {
    restore();
  }
});
