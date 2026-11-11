import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { dom } from "./helpers/dom.ts";
import { communityRepos, i18n, installFetch, json, zhImages } from "./helpers/imagesHarness.ts";
import type { CommunityPick } from "../src/pages/images/ImageCommunityBrowser.tsx";

const { cleanup, render, screen, waitFor, within, fireEvent } = await import("@testing-library/react");
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
  assert.equal(document.activeElement === button, true);
  await waitFor(() => assert.equal(within(screen.getByRole("tooltip")).getByText(tag).textContent, tag));
  await user.keyboard("{Enter}");
  assert.deepEqual(picked, [{
    imageRef: `${repo.repo}:${tag}`, engine: "vllm", framework: "cuda", engineVersion: "0.30.0", frameworkVersion: "", arch: "", rollingTag: true,
  }]);
});

test("browsing a private community repository grants only the host explicitly checked in the form", async () => {
  let allowed = false;
  const stub = installFetch((call) => {
    if (call.path === "/api/v1/auth/me") return json(200, { id: 1, username: "admin", role: "admin" });
    if (call.path === "/api/v1/images/community/repos") return json(200, { items: [], total: 0 });
    if (call.path === "/api/v1/server/outbound-policy/check") {
      const { hosts } = JSON.parse(call.body || "{}");
      return json(200, hosts.map((host: string) => ({ host, status: host === "registry.internal" && !allowed ? "needs_allow" : "allowed" })));
    }
    if (call.path === "/api/v1/server/outbound-policy" && call.method === "GET") return json(200, { restrict_private_hosts: true, allow_private_hosts: ["existing.internal"], editable: true, revision: "r1" });
    if (call.path === "/api/v1/server/outbound-policy" && call.method === "PUT") {
      assert.deepEqual(JSON.parse(call.body || "{}"), { restrict_private_hosts: true, allow_private_hosts: ["existing.internal", "registry.internal"], revision: "r1" });
      allowed = true;
      return json(200, { restrict_private_hosts: true, allow_private_hosts: ["existing.internal", "registry.internal"], editable: true, revision: "r2" });
    }
    if (call.path === "/api/v1/images/community/browse?repo=registry.internal%3A5443%2Fteam%2Frunner") {
      return allowed ? json(200, { repo: "registry.internal:5443/team/runner", tags: [{ tag: "verified" }] }) : json(400, { error: "outbound URL is not permitted" });
    }
    return undefined;
  });
  restoreFetch = stub.restore;
  render(<I18nextProvider i18n={i18n}><ImageCommunityBrowser onPick={() => {}} /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  await user.type(screen.getByRole("textbox", { name: zhImages.import.community.otherRepo }), "registry.internal:5443/team/runner");
  const checkbox = await screen.findByRole("checkbox", { name: /registry\.internal/ });
  assert.ok(stub.calls.some((call) => call.path.endsWith("/outbound-policy/check") && call.body === JSON.stringify({ hosts: ["registry.internal"] })));
  await user.click(screen.getByRole("button", { name: zhImages.import.community.browse }));
  await screen.findByRole("alert");
  assert.equal(stub.calls.some((call) => call.method === "PUT"), false, "unchecked browsing cannot write permissions");
  checkbox.focus();
  await user.keyboard("[Space]");
  await user.click(screen.getByRole("button", { name: zhImages.import.community.browse }));
  await screen.findByRole("button", { name: "verified" });
  assert.equal(stub.calls.filter((call) => call.method === "PUT").length, 1);
});

test("community access checks follow the server's case-sensitive Docker alias normalization", async () => {
  const stub = installFetch((call) => {
    if (call.path === "/api/v1/auth/me") return json(200, { id: 1, username: "admin", role: "admin" });
    if (call.path === "/api/v1/images/community/repos") return json(200, { items: [], total: 0 });
    if (call.path === "/api/v1/server/outbound-policy") return json(200, { restrict_private_hosts: true, allow_private_hosts: [], editable: true, revision: "r1" });
    if (call.path.endsWith("/outbound-policy/check")) return json(200, JSON.parse(call.body!).hosts.map((host: string) => ({ host, status: "allowed" })));
    return undefined;
  });
  restoreFetch = stub.restore;
  render(<I18nextProvider i18n={i18n}><ImageCommunityBrowser onPick={() => {}} /></I18nextProvider>);
  // community.splitHost only rewrites the exact lower-case alias. Otherwise
  // this explicit registry is read with the OCI API at index.docker.io.
  fireEvent.change(screen.getByRole("textbox", { name: zhImages.import.community.otherRepo }), { target: { value: "INDEX.DOCKER.IO/team/runner" } });
  await waitFor(() => assert.ok(stub.calls.some((call) => call.path.endsWith("/outbound-policy/check"))));
  const check = stub.calls.find((call) => call.path.endsWith("/outbound-policy/check"))!;
  assert.deepEqual(JSON.parse(check.body!), { hosts: ["index.docker.io"] });
  assert.equal(stub.calls.some((call) => call.method === "PUT"), false);
});

test("a configured community repository authorizes its server-projected API host", async () => {
  let allowed = false;
  const stub = installFetch((call) => {
    if (call.path === "/api/v1/auth/me") return json(200, { id: 1, username: "admin", role: "admin" });
    if (call.path === "/api/v1/images/community/repos") return json(200, { items: [{ repo: "registry.corp/team/runner", registry: "oci", engine: "vllm", framework: "cuda", access_host: "tags.corp" }], total: 1 });
    if (call.path === "/api/v1/server/outbound-policy" && call.method === "GET") return json(200, { restrict_private_hosts: true, allow_private_hosts: ["existing.internal"], editable: true, revision: "r1" });
    if (call.path.endsWith("/outbound-policy/check")) return json(200, JSON.parse(call.body!).hosts.map((host: string) => ({ host, status: allowed && host === "tags.corp" ? "allowed" : "needs_allow" })));
    if (call.path === "/api/v1/server/outbound-policy" && call.method === "PUT") {
      assert.deepEqual(JSON.parse(call.body!), { restrict_private_hosts: true, allow_private_hosts: ["existing.internal", "tags.corp"], revision: "r1" });
      allowed = true;
      return json(200, { restrict_private_hosts: true, allow_private_hosts: ["existing.internal", "tags.corp"], editable: true, revision: "r2" });
    }
    if (call.path === "/api/v1/images/community/browse?repo=registry.corp%2Fteam%2Frunner") return allowed ? json(200, { repo: "registry.corp/team/runner", access_host: "tags.corp", tags: [{ tag: "configured" }] }) : json(403, { error: "outbound URL is not permitted" });
    return undefined;
  });
  restoreFetch = stub.restore;
  render(<I18nextProvider i18n={i18n}><ImageCommunityBrowser onPick={() => {}} /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByRole("button", { name: "registry.corp/team/runner" });
  fireEvent.change(screen.getByRole("textbox", { name: zhImages.import.community.otherRepo }), { target: { value: "registry.corp/team/runner" } });
  await user.click(await screen.findByRole("checkbox", { name: /tags\.corp/ }));
  assert.equal(screen.queryByRole("checkbox", { name: /registry\.corp/ }), null);
  await user.click(screen.getByRole("button", { name: zhImages.import.community.browse }));
  await screen.findByRole("button", { name: "configured" });
  assert.equal(stub.calls.filter((call) => call.method === "PUT").length, 1);
});


test("community access waits for repository metadata before offering an authorization host", async () => {
  let releaseRepos!: (response: Response) => void;
  const pendingRepos = new Promise<Response>((resolve) => { releaseRepos = resolve; });
  let allowed = false;
  const stub = installFetch((call) => {
    if (call.path === "/api/v1/auth/me") return json(200, { id: 1, username: "admin", role: "admin" });
    if (call.path === "/api/v1/images/community/repos") return pendingRepos;
    if (call.path === "/api/v1/server/outbound-policy" && call.method === "GET") return json(200, { restrict_private_hosts: true, allow_private_hosts: [], editable: true, revision: "r1" });
    if (call.path.endsWith("/outbound-policy/check")) return json(200, JSON.parse(call.body!).hosts.map((host: string) => ({ host, status: allowed && host === "tags.corp" ? "allowed" : "needs_allow" })));
    if (call.path === "/api/v1/server/outbound-policy" && call.method === "PUT") {
      assert.deepEqual(JSON.parse(call.body!), { restrict_private_hosts: true, allow_private_hosts: ["tags.corp"], revision: "r1" });
      allowed = true;
      return json(200, { restrict_private_hosts: true, allow_private_hosts: ["tags.corp"], editable: true, revision: "r2" });
    }
    if (call.path === "/api/v1/images/community/browse?repo=registry.corp%2Fteam%2Frunner") return allowed ? json(200, { repo: "registry.corp/team/runner", tags: [{ tag: "ready" }] }) : json(403, { error: "outbound URL is not permitted" });
    return undefined;
  });
  restoreFetch = stub.restore;
  render(<I18nextProvider i18n={i18n}><ImageCommunityBrowser onPick={() => {}} /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  try {
    fireEvent.change(screen.getByRole("textbox", { name: zhImages.import.community.otherRepo }), { target: { value: "registry.corp/team/runner" } });
    await screen.findByRole("link", { name: i18n.t("outboundAccess.settings", { ns: "common" }) });
    assert.equal(screen.queryByRole("checkbox", { name: /registry\.corp/ }), null, "a pending catalog cannot offer a guessed registry host");
    await user.click(screen.getByRole("button", { name: zhImages.import.community.browse }));
    await screen.findByRole("alert");
    assert.equal(stub.calls.some((call) => call.path.endsWith("/outbound-policy/check")), false);
    assert.equal(stub.calls.some((call) => call.method === "PUT"), false);
    releaseRepos(json(200, { items: [{ repo: "registry.corp/team/runner", engine: "vllm", framework: "cuda", access_host: "tags.corp" }], total: 1 }));
    await user.click(await screen.findByRole("checkbox", { name: /tags\.corp/ }));
    await user.click(screen.getByRole("button", { name: zhImages.import.community.browse }));
    await screen.findByRole("button", { name: "ready" });
    assert.equal(stub.calls.filter((call) => call.method === "PUT").length, 1);
  } finally {
    releaseRepos(json(200, { items: [], total: 0 }));
  }
});


for (const metadata of ["missing access host", "catalog failure"] as const) {
  test(`community metadata with ${metadata} offers settings guidance without a guessed grant`, async () => {
    const stub = installFetch((call) => {
      if (call.path === "/api/v1/auth/me") return json(200, { id: 1, username: "admin", role: "admin" });
      if (call.path === "/api/v1/images/community/repos") return metadata === "catalog failure"
        ? json(503, { error: "catalog unavailable" })
        : json(200, { items: [{ repo: "registry.corp/team/runner", engine: "vllm", framework: "cuda" }], total: 1 });
      if (call.path === "/api/v1/server/outbound-policy" && call.method === "GET") return json(200, { restrict_private_hosts: true, allow_private_hosts: [], editable: true, revision: "r1" });
      if (call.path.endsWith("/outbound-policy/check")) return json(200, JSON.parse(call.body!).hosts.map((host: string) => ({ host, status: "needs_allow" })));
      if (call.path === "/api/v1/images/community/browse?repo=registry.corp%2Fteam%2Frunner") return json(403, { error: "outbound URL is not permitted" });
      return undefined;
    });
    restoreFetch = stub.restore;
    render(<I18nextProvider i18n={i18n}><ImageCommunityBrowser onPick={() => {}} /></I18nextProvider>);
    const user = userEvent.setup({ document: dom.window.document });
    if (metadata === "catalog failure") await screen.findByRole("alert");
    else await screen.findByRole("button", { name: "registry.corp/team/runner" });
    fireEvent.change(screen.getByRole("textbox", { name: zhImages.import.community.otherRepo }), { target: { value: "registry.corp/team/runner" } });
    await screen.findByText(zhImages.import.community.accessUnknown);
    assert.equal(screen.getByRole("link", { name: i18n.t("outboundAccess.settings", { ns: "common" }) }).getAttribute("href"), "/settings#outbound-policy");
    assert.equal(screen.queryByRole("checkbox"), null);
    await user.click(screen.getByRole("button", { name: zhImages.import.community.browse }));
    await screen.findByText("outbound URL is not permitted");
    assert.equal(stub.calls.some((call) => call.path.endsWith("/outbound-policy/check") || call.method === "PUT"), false);
  });
}
