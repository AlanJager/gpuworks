import assert from "node:assert/strict";
import { test } from "node:test";
import { STORAGE_TYPES, canWriteStorage, isModelFileReady, storageTypeInfo } from "../src/storageTypes.ts";

test("each storage type says whether it is shared, NFS, cached and what it switches to", () => {
  assert.deepEqual(STORAGE_TYPES, ["local", "nfs", "nfs_cache", "shared_dir"]);
  assert.deepEqual(storageTypeInfo("local"), { shared: false, nfs: false, cache: false });
  assert.deepEqual(storageTypeInfo("nfs"), { shared: true, nfs: true, cache: false, switchTo: "nfs_cache" });
  assert.deepEqual(storageTypeInfo("nfs_cache"), { shared: true, nfs: true, cache: true, switchTo: "nfs" });
  assert.deepEqual(storageTypeInfo("shared_dir"), { shared: true, nfs: false, cache: false });
});

test("an unknown or missing storage type has none of the facts", () => {
  for (const type of [undefined, "", "some_future_type"]) {
    assert.deepEqual(storageTypeInfo(type), { shared: false, nfs: false, cache: false });
  }
});

test("a storage takes writes only when it is enabled and some node can write to it", () => {
  assert.equal(canWriteStorage({ enabled: true, writable: true }), true);
  assert.equal(canWriteStorage({ enabled: false, writable: true }), false);
  assert.equal(canWriteStorage({ enabled: true, writable: false }), false);
});

test("a model file is ready once present or registered, and not while it is moving or broken", () => {
  assert.equal(isModelFileReady("present"), true);
  assert.equal(isModelFileReady("registered"), true);
  for (const state of ["pending", "downloading", "syncing", "failed", "missing", "download_required", ""]) {
    assert.equal(isModelFileReady(state), false, state);
  }
});
