import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { dom } from "./helpers/dom.ts";
import { communityRepos, i18n, installFetch, json, zhImages } from "./helpers/imagesHarness.ts";
import type { CommunityPick } from "../src/pages/images/ImageCommunityBrowser.tsx";

const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: ImageCommunityBrowser } = await import("../src/pages/images/ImageCommunityBrowser.tsx");

let restoreFetch = () => {};
afterEach(() => {
  cleanup();
  restoreFetch();
});

test("a shortened community tag remains readable and keyboard selection preserves its full reference", async () => {
  const tag = "nightly-20261030-cu130-vllm0.30.0-qwen3.8-27b-toolcall-fix-ubuntu22.04-linux-amd64";
  const repo = communityRepos.find((item) => item.repo === "vllm/vllm-openai")!;
  const stub = installFetch((call) => {
    if (call.path === "/api/v1/images/community/repos") return json(200, { items: [repo], total: 1 });
    if (call.path === "/api/v1/images/community/browse?repo=vllm%2Fvllm-openai") {
      return json(200, {
        repo: repo.repo, engine: repo.engine, framework: repo.framework,
        tags: [{ tag, engine_version: "0.30.0", rolling_tag: true, last_updated: "2026-10-30T08:30:00Z" }],
      });
    }
    return undefined;
  });
  restoreFetch = stub.restore;
  const picked: CommunityPick[] = [];
  render(<I18nextProvider i18n={i18n}><ImageCommunityBrowser onPick={(value) => picked.push(value)} /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  await user.click(await screen.findByRole("button", { name: repo.repo }));
  const button = await screen.findByRole("button", { name: tag });

  await user.hover(button);
  assert.equal((await screen.findByRole("tooltip")).textContent, tag);
  await user.unhover(button);
  await user.click(screen.getByRole("textbox", { name: zhImages.import.community.search }));
  await user.tab();
  assert.equal(document.activeElement, button);
  await waitFor(() => assert.equal(within(screen.getByRole("tooltip")).getByText(tag).textContent, tag));
  await user.keyboard("{Enter}");
  assert.deepEqual(picked, [{
    imageRef: `${repo.repo}:${tag}`, engine: "vllm", framework: "cuda", engineVersion: "0.30.0", rollingTag: true,
  }]);
});
