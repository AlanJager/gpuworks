import assert from "node:assert/strict";
import { test } from "node:test";
import { pickNodes } from "./helpers/nodePicker.ts";
import { choose, openDrawer, renderPage } from "./helpers/imageAddDrawerHarness.tsx";
import { communitySeries, json, nightlyVersion, zhImages } from "./helpers/imagesHarness.ts";

const { act, fireEvent, screen, waitFor, within } = await import("@testing-library/react");

test("a node file preview fills actual tags without registering or loading the image", async () => {
  const endpoint = "/api/v1/images/nodes/worker-id/preview-archive-path";
  const pending = Promise.withResolvers<void>();
  const { calls, user } = renderPage(async call => {
    if (call.path.startsWith("/api/v1/nodes?")) {
      return json(200, { items: [{ uuid: "worker-id", name: "GPU Worker" }], total: 1, page: 1, page_size: 50 });
    }
    if (call.path !== endpoint) return undefined;
    const { archive_path } = JSON.parse(call.body!);
    if (archive_path === "/slow.tar") await pending.promise;
    if (archive_path === "/old.tar") return json(409, { code: "AGENT_UPGRADE_REQUIRED" });
    if (archive_path === "/offline.tar") return json(409, { code: "NODE_OFFLINE" });
    if (archive_path === "/bad.tar") return json(409, { code: "IMAGE_ARCHIVE_PREVIEW_FAILED", params: { reason: "invalid_archive" } });
    return json(200, { repo_tags: archive_path === "/empty.tar" ? [] : ["example/real:v2", "example/real:latest"], metadata_by_tag: {
      "example/real:v2": { engine: "vllm", engine_version: "1.0", framework: "cuda", framework_version: "13.0", arch: "amd64" },
      "example/real:latest": { engine_version: "2.0" },
    } });
  });
  await screen.findByRole("button", { name: "vLLM · CUDA" });
  const drawer = within(await openDrawer(user, "自定义镜像"));
  const resolve = drawer.getByRole("button", { name: zhImages.import.url.resolve });
  const ref = drawer.getByRole("textbox", { name: zhImages.import.imageRef }) as HTMLInputElement;
  const path = drawer.getByRole("textbox", { name: zhImages.nodeTable.localDialog.path });
  await user.click(resolve);
  await drawer.findByText(zhImages.import.local.nodeRequired);
  assert.ok(!calls.some(call => call.method === "POST"));
  await pickNodes(drawer.getByRole("combobox", { name: zhImages.nodeTable.column.node }), "GPU Worker");
  const changePath = (value: string) => fireEvent.change(path, { target: { value } });
  changePath("  /filename-without-a-tag.tar  ");
  await user.click(resolve);
  await waitFor(() => assert.equal(ref.value, "example/real:v2"));
  assert.deepEqual(JSON.parse(calls.find(call => call.path === endpoint)!.body!), { archive_path: "/filename-without-a-tag.tar" });
  const version = drawer.getByRole("textbox", { name: zhImages.import.engineVersion }) as HTMLInputElement;
  const frameworkVersion = drawer.getByRole("textbox", { name: zhImages.import.frameworkVersion }) as HTMLInputElement;
  assert.equal(version.value, "1.0");
  assert.equal(frameworkVersion.value, "13.0");
  await user.click(drawer.getByRole("button", { name: "example/real:latest" }));
  assert.equal(version.value, "2.0");
  assert.equal(frameworkVersion.value, "");
  assert.equal(ref.value, "example/real:latest");
  changePath("/slow.tar");
  assert.equal(ref.value, "");
  await user.click(resolve);
  await waitFor(() => assert.equal(calls.filter(call => call.path === endpoint).length, 2));
  changePath("/empty.tar");
  await act(async () => { pending.resolve(); });
  await user.click(resolve);
  await drawer.findByText(zhImages.import.local.noTags);
  assert.equal(ref.value, "");
  await user.type(ref, "example/manual:v1");
  for (const [path, message] of [["/old.tar", "目标节点的 agent 版本过旧；请先升级 agent，再继续操作。"], ["/offline.tar", "计算节点已离线。"]]) {
    changePath(path);
    await user.click(resolve);
    await drawer.findByText(message);
  }
  changePath("/bad.tar");
  await user.click(resolve);
  await drawer.findByText("文件不是有效的镜像归档，或归档已损坏。");
  assert.equal(ref.value, "example/manual:v1");
  assert.ok(calls.filter(call => call.method === "POST").every(call => call.path === endpoint));
});

for (const outcome of ["syncing", "failed", "present"] as const) {
  const rejected = outcome === "failed";
  test(`registering a node file opens the exact custom version when the result is ${outcome}`, async () => {
    const ref = "example/local:v1";
    const path = "/mnt/images/my runner.tar";
    const version = { ...nightlyVersion, image_ref: ref, source: "custom", present_nodes: 0 };
    const sibling = { ...version, id: 32, image_ref: "example/local:v2" };
    const node = { node_uuid: "worker-id", node_name: "GPU Worker", node_state: "online", state: "absent", updated_at: "" };
    const series = { ...communitySeries, name: "Local runner", source: "custom", versions: [sibling, { ...version, nodes: [node] }] };
    const sync = `/api/v1/images/${version.id}/nodes/${node.node_uuid}/sync`;
    const { calls, user } = renderPage((call) => {
      if (call.path.startsWith("/api/v1/nodes?") && new URLSearchParams(call.path.split("?")[1]).get("state") === "online") {
        return json(200, { items: [{ uuid: node.node_uuid, name: node.node_name }], total: 1, page: 1, page_size: 50 });
      }
      if (call.path === "/api/v1/images/import") return json(200, { items: [series], total: 1 });
      if (call.path === sync) return rejected
        ? json(409, { code: "AGENT_UPGRADE_REQUIRED", error: "upgrade the target agent" })
        : json(202, outcome === "present" ? { state: "present" } : { state: "syncing", transfer_id: "attempt" });
      if (call.path === `/api/v1/images/${series.id}`) return json(200, {
        ...series, versions: [sibling, { ...version, nodes: [{ ...node, state: outcome, source: "local" }] }],
      });
      return undefined;
    });
    await screen.findByRole("button", { name: "vLLM · CUDA" });
    const drawer = within(await openDrawer(user, "自定义镜像"));
    const method = drawer.getByRole("combobox", { name: zhImages.import.method });
    assert.equal(method.textContent, zhImages.import.local.enable);
    assert.equal(drawer.queryByRole("textbox", { name: zhImages.import.url.label }), null);
    await user.type(drawer.getByRole("textbox", { name: zhImages.import.imageRef }), ref);
    await choose(drawer.getByRole("combobox", { name: zhImages.import.engine }), "vLLM");
    await choose(drawer.getByRole("combobox", { name: zhImages.import.framework }), "CUDA");
    await user.type(drawer.getByRole("textbox", { name: zhImages.import.frameworkVersion }), "13.0");
    const file = drawer.getByRole("textbox", { name: zhImages.nodeTable.localDialog.path });
    await user.type(file, "relative.tar");
    await user.click(drawer.getByRole("button", { name: zhImages.import.addImage }));
    await drawer.findByText(zhImages.import.local.nodeRequired);
    await drawer.findByText(zhImages.import.local.pathRequired);
    assert.ok(!calls.some((call) => call.method === "POST"));
    await pickNodes(drawer.getByRole("combobox", { name: zhImages.nodeTable.column.node }), node.node_name);
    await user.clear(file);
    await user.type(file, `  ${path}  `);
    await user.dblClick(drawer.getByRole("button", { name: zhImages.import.addImage }));
    await waitFor(() => assert.ok(calls.some((call) => call.path === sync)));
    const posts = calls.filter((call) => call.method === "POST");
    assert.deepEqual(posts.map((call) => call.path), ["/api/v1/images/import", sync]);
    assert.deepEqual(JSON.parse(posts[1].body!), { source: "local", archive_path: path });
    assert.ok(!posts[0].body!.includes(path));
    assert.equal(JSON.parse(posts[0].body!).series[0].versions[0].image_ref, ref);
    await screen.findByText(node.node_name);
    if (outcome === "present") await screen.findByText(zhImages.import.local.alreadyPresent);
    if (rejected) await screen.findByText(/已登记 example\/local:v1.*目标节点的 agent 版本过旧/);
  });
}
