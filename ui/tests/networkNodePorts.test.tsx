import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import type { NetworkDetail } from "../src/networkTypes";

const { cleanup, render, screen, within } = await import("@testing-library/react");
const { RailsCell } = await import("../src/pages/networks/RDMAEndpoints.tsx");
const { i18n } = await createI18n(["networks"]);
afterEach(cleanup);

test("a long list of HCA ports retains every port, its remove action, and keeps add outside the scrolling list", () => {
  const detail: NetworkDetail = JSON.parse(readFileSync(new URL("./fixtures/network-detail.json", import.meta.url), "utf8"));
  const base = detail.nodes[0];
  const hcas = Array.from({ length: 64 }, (_, i) => ({
    ...base.hcas[0], name: `mlx5_${i}:1`, ip: i === 63 ? "" : `192.0.2.${i + 1}`,
  }));
  render(<I18nextProvider i18n={i18n}><RailsCell network={detail} node={{ ...base, hcas }} onChanged={() => {}} /></I18nextProvider>);
  const ports = screen.getByRole("list", { name: i18n.t("nodeTable.hcaIPs") });
  const entries = within(ports).getAllByRole("listitem");
  assert.equal(entries.length, 64);
  assert.ok(within(entries[63]).getByText("mlx5_63:1"));
  assert.ok(within(entries[63]).getByText("—"));
  assert.ok(within(entries[63]).getByRole("button", { name: "Remove mlx5_63:1" }));
  assert.equal(ports.contains(screen.getByRole("button", { name: new RegExp(i18n.t("rail.add")) })), false);
});
