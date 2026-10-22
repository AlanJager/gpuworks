// The storage-file upload over XMLHttpRequest and the storage-files helpers: the request it sends, the progress it reports, and how each kind of failure comes back.
import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import "./helpers/dom.ts";
import { FakeXHR, installFakeXHR, lastXHR } from "./helpers/fakeXhr.ts";

const { uploadStorageFile, isUploadAbort, StorageUploadNetworkError } = await import("../src/storageUpload.ts");
const { storageFileDownloadURL, storageFileUploadURL, storageFilesQuery } = await import("../src/api.ts");
const { APIError, ProxyErrorPage } = await import("../src/apiError.ts");
const types = await import("../src/storageTypes.ts");
const browse = await import("../src/pages/storage/storageBrowse.ts");

let restore: () => void;
beforeEach(() => { restore = installFakeXHR(); });
afterEach(() => restore());

const file = (name: string, size: number) => new File([new Uint8Array(size)], name);
const settle = () => new Promise((resolve) => setTimeout(resolve, 0));

test("an upload is one PUT of the raw file with the target, node, extract and overwrite in an encoded query", async () => {
  const body = file("a b.tar.gz", 10);
  const done = uploadStorageFile({ storageId: 3, node: "n/1", path: "files/data sets/a b", file: body, extract: "tar.gz", overwrite: true });
  const xhr = lastXHR();
  assert.equal(xhr.method, "PUT");
  assert.equal(xhr.url, "/api/v1/storages/3/files/upload?node_uuid=n%2F1&path=files%2Fdata%20sets%2Fa%20b&extract=tar.gz&overwrite=true");
  assert.equal(xhr.requestHeaders["Content-Type"], "application/octet-stream");
  assert.equal(xhr.withCredentials, true);
  assert.ok(xhr.body === body, "the File itself is the body, not a copy or form data");
  xhr.respond(200, JSON.stringify({ path: "files/data sets/a b", bytes: 10, files: 4 }));
  assert.deepEqual(await done, { path: "files/data sets/a b", bytes: 10, files: 4 });
});

test("a shared storage's upload leaves the node out; no extract and no overwrite leave those out too", async () => {
  const done = uploadStorageFile({ storageId: 2, path: "files/x.bin", file: file("x.bin", 1) });
  assert.equal(lastXHR().url, "/api/v1/storages/2/files/upload?path=files%2Fx.bin");
  lastXHR().respond(200, JSON.stringify({ path: "files/x.bin", bytes: 1, files: 1 }));
  await done;
});

test("progress is reported at most once per interval, and the last byte always", async () => {
  const seen: [number, number][] = [];
  const done = uploadStorageFile({ storageId: 1, node: "n1", path: "files/x", file: file("x", 100), progressIntervalMs: 60_000, onProgress: (p) => seen.push([p.loaded, p.total]) });
  const xhr = lastXHR();
  xhr.progress(10, 100);
  xhr.progress(20, 100);
  xhr.progress(50, 100);
  xhr.progress(100, 100);
  assert.deepEqual(seen, [[10, 100], [100, 100]], "the first report, then nothing until the end");
  xhr.respond(200, JSON.stringify({ path: "files/x", bytes: 100, files: 1 }));
  await done;
});

test("without a computable length the file's size is the total", async () => {
  const seen: number[] = [];
  const done = uploadStorageFile({ storageId: 1, node: "n1", path: "files/x", file: file("x", 64), onProgress: (p) => seen.push(p.total) });
  lastXHR().progress(5, 0, false);
  assert.deepEqual(seen, [64]);
  lastXHR().respond(200, JSON.stringify({ path: "files/x", bytes: 64, files: 1 }));
  await done;
});

test("a JSON error answer becomes an APIError with its code, status and the server's text as fallback", async () => {
  const done = uploadStorageFile({ storageId: 1, node: "n1", path: "files/x", file: file("x", 1) });
  lastXHR().respond(409, JSON.stringify({ code: "STORAGE_FILE_EXISTS", error: "files/x already exists" }));
  const error = await done.then(() => null, (e: unknown) => e);
  assert.ok(error instanceof APIError);
  assert.equal(error.code, "STORAGE_FILE_EXISTS");
  assert.equal(error.status, 409);
  assert.equal(error.fallback, "files/x already exists");
});

test("a reverse proxy's HTML page in place of the answer is reported as such", async () => {
  const done = uploadStorageFile({ storageId: 1, node: "n1", path: "files/x", file: file("x", 1) });
  lastXHR().respond(413, "<html><body>413 Request Entity Too Large</body></html>", { "content-type": "text/html" }, "Payload Too Large");
  const error = await done.then(() => null, (e: unknown) => e);
  assert.ok(error instanceof ProxyErrorPage);
  assert.equal(error.status, 413);
});

test("a non-JSON error answer keeps its text; an unreadable success is an error, not a crash", async () => {
  const failed = uploadStorageFile({ storageId: 1, node: "n1", path: "files/x", file: file("x", 1) });
  lastXHR().respond(502, "bad gateway", { "Content-Type": "text/plain" });
  const error = await failed.then(() => null, (e: unknown) => e);
  assert.ok(error instanceof Error && error.message === "bad gateway");

  const garbled = uploadStorageFile({ storageId: 1, node: "n1", path: "files/x", file: file("x", 1) });
  lastXHR().respond(200, "not json");
  const parseError = await garbled.then(() => null, (e: unknown) => e);
  assert.ok(parseError instanceof Error && /unreadable answer/.test(parseError.message));
});

test("a broken connection rejects with the network error; an empty error answer falls back to the status", async () => {
  const broken = uploadStorageFile({ storageId: 1, node: "n1", path: "files/x", file: file("x", 1) });
  lastXHR().fail();
  assert.ok(await broken.then(() => null, (e: unknown) => e) instanceof StorageUploadNetworkError);

  const empty = uploadStorageFile({ storageId: 1, node: "n1", path: "files/x", file: file("x", 1) });
  lastXHR().respond(500, "", {});
  const error = await empty.then(() => null, (e: unknown) => e);
  assert.ok(error instanceof Error && /files\/upload.*: 500/.test(error.message));
});

test("aborting cancels the request in flight; an already aborted signal sends nothing", async () => {
  const abort = new AbortController();
  const running = uploadStorageFile({ storageId: 1, node: "n1", path: "files/x", file: file("x", 1), signal: abort.signal });
  const xhr = lastXHR();
  abort.abort();
  const error = await running.then(() => null, (e: unknown) => e);
  assert.ok(xhr.aborted);
  assert.ok(isUploadAbort(error));

  const before = FakeXHR.instances.length;
  const early = uploadStorageFile({ storageId: 1, node: "n1", path: "files/x", file: file("x", 1), signal: abort.signal });
  assert.ok(isUploadAbort(await early.then(() => null, (e: unknown) => e)));
  assert.equal(FakeXHR.instances.length, before, "no request for a cancelled upload");
  assert.equal(isUploadAbort(new Error("x")), false);
});

test("aborting after the answer changes nothing", async () => {
  const abort = new AbortController();
  const done = uploadStorageFile({ storageId: 1, node: "n1", path: "files/x", file: file("x", 1), signal: abort.signal });
  lastXHR().respond(200, JSON.stringify({ path: "files/x", bytes: 1, files: 1 }));
  abort.abort();
  await settle();
  assert.equal((await done).path, "files/x");
  assert.equal(lastXHR().aborted, false);
});

test("download and upload links encode every value and leave out what is absent", () => {
  assert.equal(storageFileDownloadURL(1, "n 1", "files/a&b/c.txt"), "/api/v1/storages/1/files/download?node_uuid=n%201&path=files%2Fa%26b%2Fc.txt");
  assert.equal(storageFileDownloadURL(2, undefined, "files/dir", "tar.gz"), "/api/v1/storages/2/files/download?path=files%2Fdir&format=tar.gz");
  assert.equal(storageFileUploadURL(2, undefined, "files/a", "", false), "/api/v1/storages/2/files/upload?path=files%2Fa");
  assert.equal(storageFilesQuery({ path: "", cursor: undefined, limit: 50 }), "limit=50");
});

test("the writable regions, archive names and entry names follow the server's rules", () => {
  assert.equal(types.storageRelWritable("files"), false, "a region root is not itself changeable");
  assert.equal(types.storageRelWritable("files/a"), true);
  assert.equal(types.storageRelWritable(".gpuworks/apps/evalscope"), true);
  assert.equal(types.storageRelWritable(".gpuworks/trash/x"), false);
  assert.equal(types.storageRelWritable("filesx/a"), false);
  assert.equal(types.storageRelWritable("Qwen/Qwen3-8B"), false);
  assert.equal(types.isStorageRegionRoot(".gpuworks/apps"), true);
  assert.equal(types.isStorageRegionRoot("files/a"), false);

  assert.equal(types.archiveFormatOf("a.tar.gz"), "tar.gz");
  assert.equal(types.archiveFormatOf("a.TGZ"), "tar.gz");
  assert.equal(types.archiveFormatOf("a.tar"), "tar");
  assert.equal(types.archiveFormatOf("a.zip"), "zip");
  assert.equal(types.archiveFormatOf("a.gz"), undefined, "a bare .gz is not an archive of files");
  assert.equal(types.archiveStem("gsm8k.tar.gz"), "gsm8k");
  assert.equal(types.archiveStem(".zip"), ".zip", "nothing left keeps the name");

  assert.equal(types.joinStorageRel("", "files"), "files");
  assert.equal(types.joinStorageRel("files", "a"), "files/a");
  assert.equal(types.storageRelParent("files/a/b"), "files/a");
  assert.equal(types.storageRelParent("files"), "");
  for (const bad of ["", ".", "..", "a/b", "a\\b", "a\0b"]) assert.equal(types.validEntryName(bad), false, bad);
  assert.equal(types.validEntryName("data set.v2"), true);
});

test("why a place is read-only: the disabled storage first, then the region, the region root, else the server's say", () => {
  const on = { enabled: true };
  assert.equal(browse.dirReadonlyReason(on, "files", true), null);
  assert.equal(browse.dirReadonlyReason({ enabled: false }, "files", false), "disabled");
  assert.equal(browse.dirReadonlyReason(on, "", false), "region");
  assert.equal(browse.dirReadonlyReason(on, ".gpuworks", false), "region");
  assert.equal(browse.dirReadonlyReason(on, "files/a", false), "other");
  assert.equal(browse.dirReadonlyReason(on, ".gpuworks/apps", false), "other");

  assert.equal(browse.itemReadonlyReason(on, { path: "files/a", writable: true }), null);
  assert.equal(browse.itemReadonlyReason({ enabled: false }, { path: "files/a", writable: false }), "disabled");
  assert.equal(browse.itemReadonlyReason(on, { path: "files", writable: false }), "regionRoot");
  assert.equal(browse.itemReadonlyReason(on, { path: "Qwen", writable: false }), "region");
  assert.equal(browse.itemReadonlyReason(on, { path: ".gpuworks/apps/x/run1", writable: false }), "other");

  assert.deepEqual(browse.pathSteps(""), []);
  assert.deepEqual(browse.pathSteps(".gpuworks/apps/x"), [
    { name: ".gpuworks", path: ".gpuworks" }, { name: "apps", path: ".gpuworks/apps" }, { name: "x", path: ".gpuworks/apps/x" },
  ]);
});
