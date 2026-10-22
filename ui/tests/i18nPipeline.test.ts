import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import test from "node:test";

import { flattenStrings, parseJSONObject, sampleSelected } from "../../scripts/i18n-pipeline.mjs";

const execFileAsync = promisify(execFile);
const pipelineScript = path.resolve("../scripts/i18n-pipeline.mjs");

function writeJSON(file: string, value: unknown) {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, `${JSON.stringify(value, null, 2)}\n`);
}

test("pipeline JSON parsing, flattening, and review sampling are deterministic", () => {
  assert.deepEqual(parseJSONObject("```json\n{\"ok\":true}\n```"), { ok: true });
  assert.deepEqual([...flattenStrings({ page: { title: "Title" } })], [["page.title", "Title"]]);
  assert.equal(sampleSelected("same-key", 3, 0.4), sampleSelected("same-key", 3, 0.4));
  assert.equal(sampleSelected("same-key", 3, 1), true);
  assert.equal(sampleSelected("same-key", 3, 0), false);
});

test("translation and review pipeline persists translations, hashes, and verdicts", async (t) => {
  const fixtureRoot = fs.mkdtempSync(path.join(os.tmpdir(), "gpuworks-i18n-pipeline-"));
  t.after(() => fs.rmSync(fixtureRoot, { recursive: true, force: true }));
  const localeRoot = path.join(fixtureRoot, "ui/src/i18n/locales");
  writeJSON(path.join(localeRoot, "en/common.json"), { greeting: "Hello {{name}}" });
  writeJSON(path.join(localeRoot, "zh/common.json"), {});
  writeJSON(path.join(fixtureRoot, "ui/i18n/glossary.json"), { terms: [], do_not_translate: [] });
  writeJSON(path.join(fixtureRoot, "ui/i18n/review-state.json"), { version: 1, review_round: 0, entries: {} });
  writeJSON(path.join(fixtureRoot, "ui/i18n/i18n.config.json"), {
    source_language: "en",
    target_languages: ["zh"],
    translation_batch_size: 8,
    review_batch_size: 8,
    review_concurrency: 2,
    review_sample_rate: 1,
    request_timeout_seconds: 5,
    request_retries: 1,
    resources: [{ id: "ui", directory: "ui/src/i18n/locales", layout: "language-directories" }],
  });

  let translatorAttempts = 0;
  let reviewRequests = 0;
  let reviewContextSeen = false;
  let malformedReviewReturned = false;
  const server = http.createServer(async (request, response) => {
    const chunks: Buffer[] = [];
    for await (const chunk of request) chunks.push(Buffer.from(chunk));
    const body = JSON.parse(Buffer.concat(chunks).toString("utf8"));
    const input = JSON.parse(body.messages[1].content);
    const translationRequest = body.messages[0].content.includes("product UI translator");
    if (translationRequest && ++translatorAttempts === 1) {
      response.writeHead(500, { "Content-Type": "text/plain" });
      response.end("temporary fixture failure");
      return;
    }
    if (!translationRequest) {
      reviewRequests++;
      reviewContextSeen = Array.isArray(input.inputs.k0.context) && input.inputs.k0.context.length > 0;
      if (!malformedReviewReturned) {
        malformedReviewReturned = true;
        response.writeHead(200, { "Content-Type": "application/json" });
        response.end(JSON.stringify({ choices: [{ message: { content: "{}" } }] }));
        return;
      }
    }
    const result = Object.fromEntries(Object.keys(input.inputs).map((id) => [
      id,
      translationRequest
        ? "你好 {{name}}"
        : { verdict: "pass", score: 100, terminology_violations: [], reason: "Accurate", suggestion: "" },
    ]));
    response.writeHead(200, { "Content-Type": "application/json" });
    response.end(JSON.stringify({ choices: [{ message: { content: JSON.stringify(result) } }] }));
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => server.close());
  const address = server.address();
  assert.ok(address && typeof address === "object");
  const baseURL = `http://127.0.0.1:${address.port}/v1`;
  fs.mkdirSync(path.join(fixtureRoot, "secrets"), { recursive: true });
  fs.writeFileSync(path.join(fixtureRoot, "secrets/i18n-llm.env"), [
    `I18N_TRANSLATOR_BASE_URL=${baseURL}`,
    "I18N_TRANSLATOR_API_KEY=test-key",
    "I18N_TRANSLATOR_MODEL=test-translator",
    `I18N_REVIEWER_BASE_URL=${baseURL}`,
    "I18N_REVIEWER_API_KEY=test-key",
    "I18N_REVIEWER_MODEL=test-reviewer",
    "",
  ].join("\n"));

  const env = { ...process.env, I18N_PIPELINE_ROOT: fixtureRoot };
  const translated = await execFileAsync(process.execPath, [pipelineScript, "translate"], { env });
  assert.match(translated.stdout, /wrote 1 keys/);
  assert.equal(translatorAttempts, 2);
  assert.deepEqual(JSON.parse(fs.readFileSync(path.join(localeRoot, "zh/common.json"), "utf8")), { greeting: "你好 {{name}}" });

  const reviewed = await execFileAsync(process.execPath, [pipelineScript, "review"], { env });
  assert.match(reviewed.stdout, /1 passed, 0 rejected/);
  const state = JSON.parse(fs.readFileSync(path.join(fixtureRoot, "ui/i18n/review-state.json"), "utf8"));
  const entry = state.entries["ui/zh/common::greeting"];
  assert.equal(entry.verdict, "pass");
  assert.equal(entry.reviewer_model, "test-reviewer");
  assert.equal(reviewContextSeen, true);
  assert.match(entry.source_hash, /^[a-f0-9]{64}$/);
  assert.match(entry.translation_hash, /^[a-f0-9]{64}$/);
  assert.equal(reviewRequests, 2);
  assert.deepEqual({
    batchesStarted: state.last_run.batches_started,
    batchesSucceeded: state.last_run.batches_succeeded,
    requestAttempts: state.last_run.request_attempts_started,
    entriesReviewed: state.last_run.entries_reviewed,
  }, { batchesStarted: 1, batchesSucceeded: 1, requestAttempts: 2, entriesReviewed: 1 });
  assert.match(state.last_run.completed_at, /^\d{4}-\d{2}-\d{2}T/);
  assert.match(fs.readFileSync(path.join(fixtureRoot, "ui/i18n/review-report.md"), "utf8"), /No unresolved reviewer rejections/);

  entry.verdict = "fail";
  entry.reason = "Fixture rejection";
  writeJSON(path.join(fixtureRoot, "ui/i18n/review-state.json"), state);
  writeJSON(path.join(localeRoot, "en/common.json"), { greeting: "Hello {{name}}", ordinary: "Ordinary" });
  writeJSON(path.join(localeRoot, "zh/common.json"), { greeting: "您好 {{name}}", ordinary: "普通" });
  const configPath = path.join(fixtureRoot, "ui/i18n/i18n.config.json");
  const config = JSON.parse(fs.readFileSync(configPath, "utf8"));
  config.review_sample_rate = 1;
  writeJSON(configPath, config);

  const forcedRecheck = await execFileAsync(process.execPath, [pipelineScript, "review"], { env });
  assert.match(forcedRecheck.stdout, /1 passed, 0 rejected/);
  const forcedState = JSON.parse(fs.readFileSync(path.join(fixtureRoot, "ui/i18n/review-state.json"), "utf8"));
  assert.equal(forcedState.entries["ui/zh/common::greeting"].verdict, "pass");
  assert.equal(forcedState.entries["ui/zh/common::ordinary"].verdict, "pending");
  assert.equal(forcedState.last_run.forced_rechecks, 1);
  assert.equal(forcedState.last_run.sampled_items, 0);
  assert.equal(forcedState.last_run.selected_items, 1);
  assert.equal(reviewRequests, 3);

  config.review_sample_rate = 0;
  writeJSON(configPath, config);
  writeJSON(path.join(localeRoot, "zh/common.json"), { greeting: "你好，{{name}}", ordinary: "普通" });
  const sampledReview = await execFileAsync(process.execPath, [pipelineScript, "review"], { env });
  assert.match(sampledReview.stdout, /0 passed, 0 rejected/);
  const sampledState = JSON.parse(fs.readFileSync(path.join(fixtureRoot, "ui/i18n/review-state.json"), "utf8"));
  assert.equal(sampledState.entries["ui/zh/common::greeting"].verdict, "pending");
  assert.equal(sampledState.last_run.forced_rechecks, 0);
  assert.equal(reviewRequests, 3);

  // Removing a locale key also removes its cached review record.
  writeJSON(path.join(localeRoot, "en/common.json"), { greeting: "Hello {{name}}" });
  writeJSON(path.join(localeRoot, "zh/common.json"), { greeting: "你好，{{name}}" });
  await execFileAsync(process.execPath, [pipelineScript, "review"], { env });
  const prunedState = JSON.parse(fs.readFileSync(path.join(fixtureRoot, "ui/i18n/review-state.json"), "utf8"));
  assert.equal(Object.hasOwn(prunedState.entries, "ui/zh/common::ordinary"), false);
});
