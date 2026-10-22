import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import type { ImageCatalogEntry } from "../src/api.ts";

const { cleanup, render, screen, within } = await import("@testing-library/react");
const ImageNodeTable = (await import("../src/pages/images/ImageNodeTable.tsx")).default;

afterEach(cleanup);

const slug = (source: string) => source.replace(/[^a-z0-9-]/gi, "_") || "none";

for (const language of ["en", "zh"] as const) {
  test(`completed node copies display their recorded source without inventing a mirror host (${language})`, async () => {
    const { i18n } = await createI18n(["images"], language);
    const sources = [
      { source: "mirror", expected: "Mirror" },
      { source: "direct", expected: "canonical.example" },
      { source: "quay", expected: "Quay" },
      { source: "huaweicloud", expected: "Huawei Cloud" },
      // A node that walks the candidates reports the reference it pulled.
      { source: "swr.cn-southwest-2.myhuaweicloud.com/team/runner:v1", expected: "Huawei Cloud" },
      { source: "public.ecr.aws/team/runner:v1", expected: "public.ecr.aws" },
      // Present with no transfer on record (a replica pulled it at launch).
      { source: "", expected: language === "en" ? "Not recorded" : "未记录" },
      { source: "offline", expected: language === "en" ? "Local offline package" : "本地离线包" },
      { source: "url", expected: "URL" },
      { source: "url-relay", expected: language === "en" ? "URL · relayed" : "URL·中转" },
      { source: "peer", source_node_uuid: "peer-node", source_node_name: "peer-source",
        expected: language === "en" ? "Node distribution · peer-source" : "节点分发 · peer-source" },
    ];
    const version = {
      id: 7, series_id: 3, name: "runner", image_ref: "canonical.example/team/runner:v1",
      source: "custom", enabled: true, reference_count: 0,
      registry_sources: ["alternate.example/team/runner:v1", "other.example/team/runner:v1"],
      nodes: sources.map(({ expected: _expected, ...source }) => ({
        ...source, node_uuid: `node-${slug(source.source)}`, node_name: `host-${slug(source.source)}`,
        node_state: "online", state: "present",
      })),
    } as ImageCatalogEntry;
    render(React.createElement(I18nextProvider, { i18n }, React.createElement(ImageNodeTable, {
      selectedVersion: version, inspecting: null, availablePeer: () => false,
      modeFor: () => "direct", setMode: () => {}, pullVersion: () => {},
      probeImage: async () => {}, syncFromPeer: async () => {}, pullFromURL: async () => {},
      inspectVersion: () => {}, cancelTransfer: () => {}, removeFromNode: () => {},
      repullVersion: () => {},
    })));

    for (const source of sources) {
      const row = screen.getByRole("row", { name: new RegExp(`^host-${slug(source.source)} `) });
      within(row).getByText(source.expected, { exact: true });
      if (source.source === "mirror") {
        for (const unprovenHost of ["canonical.example", "alternate.example", "other.example"]) {
          assert.equal(within(row).queryByText(unprovenHost, { exact: true }), null,
            "the generic backend source cannot identify the registry that supplied this copy");
        }
      }
    }
  });
}

for (const language of ["en", "zh"] as const) {
  test(`a transfer in flight shows where its bytes come from right now (${language})`, async () => {
    const { i18n, locale } = await createI18n(["images"], language);
    const text = locale.images.nodeTable as Record<string, string>;
    const version = {
      id: 8, series_id: 3, name: "runner", image_ref: "canonical.example/team/runner:v1",
      source: "custom", enabled: true, reference_count: 0, registry_sources: [],
      nodes: [
        { node_uuid: "n-swr", node_name: "host-swr", node_state: "online", state: "pulling", source: "swr.cn-southwest-2.myhuaweicloud.com/team/runner:v1" },
        { node_uuid: "n-peer", node_name: "host-peer", node_state: "online", state: "syncing", source: "peer", source_node_uuid: "src", source_node_name: "src-host" },
        { node_uuid: "n-new", node_name: "host-new", node_state: "online", state: "pulling", source: "" },
      ],
    } as ImageCatalogEntry;
    render(React.createElement(I18nextProvider, { i18n }, React.createElement(ImageNodeTable, {
      selectedVersion: version, inspecting: null, availablePeer: () => false,
      modeFor: () => "direct", setMode: () => {}, pullVersion: () => {},
      probeImage: async () => {}, syncFromPeer: async () => {}, pullFromURL: async () => {},
      inspectVersion: () => {}, cancelTransfer: () => {}, removeFromNode: () => {},
      repullVersion: () => {},
    })));
    const row = (name: string) => screen.getByRole("row", { name: new RegExp(`^${name} `) });
    within(row("host-swr")).getByText(text.pullingFrom.replace("{{source}}", "Huawei Cloud"));
    within(row("host-peer")).getByText(text.peerFrom.replace("{{node}}", "src-host"));
    within(row("host-new")).getByText(text.pullingPending);
    for (const name of ["host-swr", "host-peer", "host-new"]) {
      assert.equal(within(row(name)).queryByRole("combobox"), null, `${name}: no source to choose mid-transfer`);
    }
  });
}
