import assert from "node:assert/strict";
import test from "node:test";

import { missingProtectedTerms } from "../../scripts/i18n-terms.mjs";

test("do-not-translate terms must remain verbatim in translated text", () => {
  const terms = ["API", "SSH", "vLLM"];
  assert.deepEqual(
    missingProtectedTerms("Configure the API and SSH connection for vLLM", "为 vLLM 配置接口和 SSH 连接", terms),
    ["API"],
  );
  assert.deepEqual(
    missingProtectedTerms("Configure the API", "配置 API", terms),
    [],
  );
});
