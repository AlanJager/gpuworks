import test, { afterEach } from "node:test";
import assert from "node:assert/strict";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { cleanup, render, screen } = await import("@testing-library/react");
const { default: StorageProbePanel } = await import("../src/pages/storage/StorageProbePanel.tsx");
afterEach(cleanup);

for (const lng of ["en", "zh"] as const) {
 for (const ok of [true, false]) {
  test(`${lng}: ${ok ? "success" : "failure"} retains the probe node, target and received-at evidence`, async () => {
   const { i18n } = await createI18n(["storage"], lng);
   render(<I18nextProvider i18n={i18n}><StorageProbePanel result={{ node_uuid:"n1",node_name:"GPU 1",ok,readable:ok,writable:ok,target:"nfs.example:/models",checked_at:"2026-11-04T08:00:00Z",reason:ok?undefined:"mount_failed",message:ok?undefined:"mount refused" }} /></I18nextProvider>);
   assert.ok(screen.getByText("nfs.example:/models"));
   assert.ok(screen.getByText(/GPU 1/));
   assert.ok(screen.getByText(/2026-11-04/));
   assert.ok(screen.getByText(i18n.t("probe.scope",{ns:"storage"})));
   if(!ok) assert.ok(screen.getByText("mount refused"));
  });
 }
 test(`${lng}: older responses keep unknown provenance instead of inventing it`, async () => {
  const { i18n } = await createI18n(["storage"], lng);
  render(<I18nextProvider i18n={i18n}><StorageProbePanel result={{node_uuid:"n1",node_name:"GPU 1",ok:true,readable:true,writable:true}} /></I18nextProvider>);
  assert.ok(screen.getAllByText(i18n.t("probe.unknown",{ns:"storage"})).length>=2);
 });
}
