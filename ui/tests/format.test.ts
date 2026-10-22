import assert from "node:assert/strict";
import { test } from "node:test";
import { agoText, formatBytes } from "../src/format.ts";
import { nodeRowName } from "../src/nodeDisplay.ts";

const KiB = 1024;
const MiB = KiB ** 2;
const GiB = KiB ** 3;

test("formatBytes shows one decimal above bytes, and 0 B for an unknown size", () => {
  assert.equal(formatBytes(0), "0 B");
  assert.equal(formatBytes(undefined), "0 B");
  assert.equal(formatBytes(512), "512 B");
  assert.equal(formatBytes(1536), "1.5 KiB");
  assert.equal(formatBytes(12 * GiB), "12.0 GiB");
  assert.equal(formatBytes(3 * 1024 * GiB), "3.0 TiB");
});

test("compact formatBytes drops the decimal from 10 up, as progress lines do", () => {
  assert.equal(formatBytes(undefined, { compact: true }), "0 B");
  assert.equal(formatBytes(512, { compact: true }), "512 B");
  assert.equal(formatBytes(9.5 * GiB, { compact: true }), "9.5 GiB");
  assert.equal(formatBytes(12 * GiB, { compact: true }), "12 GiB");
});

test("formatBytes can pin the smallest and largest unit, as an audio file size does", () => {
  const audio = { minUnit: "KiB", maxUnit: "MiB" } as const;
  assert.equal(formatBytes(512, audio), "0.5 KiB");
  assert.equal(formatBytes(0, audio), "0.0 KiB");
  assert.equal(formatBytes(3 * MiB, audio), "3.0 MiB");
  assert.equal(formatBytes(2 * GiB, audio), "2048.0 MiB");
});

test("agoText is coarse: just now, minutes, hours, days", () => {
  const t = ((key: string, options?: { value?: number }) => `${key}:${options?.value ?? ""}`) as never;
  assert.equal(agoText(t, 30), "ago.now:");
  assert.equal(agoText(t, 150), "ago.minutes:2");
  assert.equal(agoText(t, 7200), "ago.hours:2");
  assert.equal(agoText(t, 3 * 86400 + 5), "ago.days:3");
});

test("nodeRowName is the node's name, else its whole UUID", () => {
  assert.equal(nodeRowName({ node_name: "gpu3", node_uuid: "abcdef0123456789" }), "gpu3");
  assert.equal(nodeRowName({ node_uuid: "abcdef0123456789" }), "abcdef0123456789");
  assert.equal(nodeRowName(undefined), "");
});
