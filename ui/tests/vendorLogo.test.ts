import assert from "node:assert/strict";
import test from "node:test";

import { vendorForFramework } from "../src/components/VendorLogo.tsx";
import { VENDOR_ASSETS } from "../src/components/vendorAssets.ts";

test("all accelerator vendors resolve through the shared logo asset map", () => {
  assert.equal(vendorForFramework("dtk"), "hygon");
  assert.equal(vendorForFramework("HYGON"), "hygon");
  assert.equal(VENDOR_ASSETS.hygon, "/vendor-logos/hygon.webp");
  assert.equal(vendorForFramework("hggc"), "ppu");
  assert.ok(VENDOR_ASSETS.thead);
});

test("Apple silicon resolves from its metal framework to the Apple mark", () => {
  assert.equal(vendorForFramework("metal"), "apple");
  assert.equal(vendorForFramework("Apple"), "apple");
  assert.equal(VENDOR_ASSETS.apple, "/vendor-logos/apple.svg");
});
