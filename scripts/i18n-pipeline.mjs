import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import process from "node:process";
import { fileURLToPath, pathToFileURL } from "node:url";

const defaultRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const root = process.env.I18N_PIPELINE_ROOT ? path.resolve(process.env.I18N_PIPELINE_ROOT) : defaultRoot;
const configPath = path.join(root, "ui/i18n/i18n.config.json");
const glossaryPath = path.join(root, "ui/i18n/glossary.json");
const statePath = path.join(root, "ui/i18n/review-state.json");
const reportPath = path.join(root, "ui/i18n/review-report.md");
const secretsPath = path.join(root, "secrets/i18n-llm.env");

function readJSON(file) {
  return JSON.parse(fs.readFileSync(file, "utf8"));
}

function writeJSON(file, value) {
  fs.writeFileSync(file, `${JSON.stringify(value, null, 2)}\n`);
}

export function flattenStrings(value, prefix = "", output = new Map()) {
  for (const [key, child] of Object.entries(value ?? {})) {
    const full = prefix ? `${prefix}.${key}` : key;
    if (typeof child === "string") output.set(full, child);
    else if (child && typeof child === "object" && !Array.isArray(child)) flattenStrings(child, full, output);
    else throw new Error(`${full} must be a string or nested object`);
  }
  return output;
}

function setNested(target, dotted, value) {
  const parts = dotted.split(".");
  let cursor = target;
  for (const part of parts.slice(0, -1)) {
    if (!cursor[part] || typeof cursor[part] !== "object" || Array.isArray(cursor[part])) cursor[part] = {};
    cursor = cursor[part];
  }
  cursor[parts.at(-1)] = value;
}

function hash(value) {
  return crypto.createHash("sha256").update(value).digest("hex");
}

export function sampleSelected(identity, round, rate) {
  if (rate >= 1) return true;
  if (rate <= 0) return false;
  const value = Number.parseInt(hash(`${round}\0${identity}`).slice(0, 8), 16) / 0xffffffff;
  return value < rate;
}

function parseEnvFile(file) {
  if (!fs.existsSync(file)) return {};
  const values = {};
  for (const rawLine of fs.readFileSync(file, "utf8").split(/\r?\n/)) {
    const line = rawLine.trim();
    if (!line || line.startsWith("#")) continue;
    const match = line.match(/^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$/);
    if (!match) throw new Error(`${path.relative(root, file)} contains an invalid environment line`);
    let value = match[2].trim();
    if ((value.startsWith('"') && value.endsWith('"')) || (value.startsWith("'") && value.endsWith("'"))) {
      value = value.slice(1, -1);
    }
    values[match[1]] = value;
  }
  return values;
}

function llmConfig(role, fileEnv) {
  const prefix = role === "translator" ? "I18N_TRANSLATOR" : "I18N_REVIEWER";
  const get = (suffix) => process.env[`${prefix}_${suffix}`] ?? fileEnv[`${prefix}_${suffix}`] ?? "";
  const config = { baseURL: get("BASE_URL").trim(), apiKey: get("API_KEY").trim(), model: get("MODEL").trim() };
  return config.baseURL && config.apiKey && config.model ? config : undefined;
}

function chatURL(baseURL) {
  const clean = baseURL.replace(/\/+$/, "");
  if (clean.endsWith("/chat/completions")) return clean;
  return `${clean}/chat/completions`;
}

function contentText(content) {
  if (typeof content === "string") return content;
  if (Array.isArray(content)) return content.map((part) => typeof part === "string" ? part : part?.text ?? "").join("");
  return "";
}

export function parseJSONObject(raw) {
  const trimmed = raw.trim();
  const unfenced = trimmed.replace(/^```(?:json)?\s*/i, "").replace(/\s*```$/, "");
  try {
    const value = JSON.parse(unfenced);
    if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("response is not an object");
    return value;
  } catch (firstError) {
    const start = unfenced.indexOf("{");
    const end = unfenced.lastIndexOf("}");
    if (start >= 0 && end > start) {
      const value = JSON.parse(unfenced.slice(start, end + 1));
      if (value && typeof value === "object" && !Array.isArray(value)) return value;
    }
    throw new Error(`LLM did not return a JSON object: ${firstError.message}`);
  }
}

async function callLLM(llm, system, user, timeoutSeconds) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutSeconds * 1000);
  try {
    const response = await fetch(chatURL(llm.baseURL), {
      method: "POST",
      headers: { Authorization: `Bearer ${llm.apiKey}`, "Content-Type": "application/json" },
      body: JSON.stringify({
        model: llm.model,
        temperature: 0,
        messages: [{ role: "system", content: system }, { role: "user", content: user }],
      }),
      signal: controller.signal,
    });
    if (!response.ok) {
      const detail = (await response.text()).slice(0, 1000);
      const error = new Error(`LLM request failed with HTTP ${response.status}${detail ? `: ${detail}` : ""}`);
      error.retryable = response.status === 429 || response.status >= 500;
      throw error;
    }
    const payload = await response.json();
    const content = contentText(payload?.choices?.[0]?.message?.content);
    if (!content) throw new Error("LLM response did not contain choices[0].message.content");
    return parseJSONObject(content);
  } finally {
    clearTimeout(timer);
  }
}

async function callLLMWithRetry(llm, system, user, timeoutSeconds, retries, label, onAttempt, validateResult) {
  let lastError;
  for (let attempt = 0; attempt <= retries; attempt++) {
    try {
      onAttempt?.(attempt + 1);
      const result = await callLLM(llm, system, user, timeoutSeconds);
      return validateResult ? validateResult(result) : result;
    } catch (error) {
      lastError = error;
      if (error?.retryable === false || attempt >= retries) throw error;
      const delay = Math.min(8000, 1000 * (2 ** attempt));
      console.warn(`i18n pipeline: retrying ${label} after ${error instanceof Error ? error.message : String(error)}`);
      await new Promise((resolve) => setTimeout(resolve, delay));
    }
  }
  throw lastError;
}

function validateTranslationBatch(result, batch) {
  for (const [index, item] of batch.entries()) {
    const translated = result[`k${index}`];
    if (typeof translated !== "string" || !translated.trim()) {
      throw new Error(`translator omitted ${itemIdentity(item.pair, item.key)}`);
    }
    if (!samePlaceholders(item.source, translated)) {
      throw new Error(`translator changed placeholders for ${itemIdentity(item.pair, item.key)}`);
    }
  }
  return result;
}

export function validateReviewBatch(result, size) {
  for (let index = 0; index < size; index++) {
    const value = result[`k${index}`];
    if (!value || typeof value !== "object" || Array.isArray(value) ||
        (value.verdict !== "pass" && value.verdict !== "fail") ||
        !Number.isFinite(Number(value.score)) ||
        !Array.isArray(value.terminology_violations) ||
        typeof value.reason !== "string" || typeof value.suggestion !== "string") {
      throw new Error(`reviewer returned an invalid result for k${index}`);
    }
  }
  return result;
}

function resourcePairs(config) {
  const pairs = [];
  for (const resource of config.resources) {
    const directory = path.join(root, resource.directory);
    if (resource.layout === "language-directories") {
      const sourceDirectory = path.join(directory, config.source_language);
      for (const name of fs.readdirSync(sourceDirectory).filter((value) => value.endsWith(".json")).sort()) {
        for (const language of config.target_languages) {
          pairs.push({
            resource: resource.id,
            namespace: name.slice(0, -5),
            language,
            sourceFile: path.join(sourceDirectory, name),
            targetFile: path.join(directory, language, name),
          });
        }
      }
    } else if (resource.layout === "language-files") {
      for (const language of config.target_languages) {
        pairs.push({
          resource: resource.id,
          namespace: resource.id,
          language,
          sourceFile: path.join(directory, `${config.source_language}.json`),
          targetFile: path.join(directory, `${language}.json`),
        });
      }
    } else {
      throw new Error(`${resource.id}: unsupported resource layout ${resource.layout}`);
    }
  }
  return pairs;
}

function glossaryContext(glossary, language) {
  const terms = (glossary.terms ?? []).map((term) => ({
    source: term.term_en,
    target: term.translations?.[language],
    forbidden: term.forbidden?.[language] ?? [],
  }));
  return JSON.stringify({ terms, do_not_translate: glossary.do_not_translate ?? [] });
}

function placeholderNames(value) {
  return [...value.matchAll(/{{\s*([^},\s]+)[^}]*}}/g)].map((match) => match[1]).sort();
}

function samePlaceholders(source, target) {
  return placeholderNames(source).join("\0") === placeholderNames(target).join("\0");
}

function chunks(values, size) {
  const output = [];
  for (let index = 0; index < values.length; index += size) output.push(values.slice(index, index + size));
  return output;
}

function neighboringContext(entries, target, radius = 4) {
  const index = entries.findIndex(([key]) => key === target);
  if (index < 0) return [];
  const start = Math.max(0, index - radius);
  const end = Math.min(entries.length, index + radius + 1);
  return entries.slice(start, end).map(([key, source]) => ({ key, source }));
}

function itemIdentity(pair, key) {
  return `${pair.resource}/${pair.language}/${pair.namespace}::${key}`;
}

async function translate(config, glossary, llm) {
  if (!llm) {
    console.log(`i18n translate skipped: configure ${path.relative(root, secretsPath)}`);
    return;
  }
  const missing = [];
  const targets = new Map();
  for (const pair of resourcePairs(config)) {
    const sourceObject = readJSON(pair.sourceFile);
    const targetObject = fs.existsSync(pair.targetFile) ? readJSON(pair.targetFile) : {};
    const source = flattenStrings(sourceObject);
    const sourceEntries = [...source];
    const target = flattenStrings(targetObject);
    for (const [key, value] of source) {
      if (!target.has(key) || !target.get(key).trim()) {
        targets.set(pair.targetFile, targetObject);
        missing.push({ pair, key, source: value, targetObject, context: neighboringContext(sourceEntries, key, 6) });
      }
    }
  }
  if (missing.length === 0) {
    console.log("i18n translate: no missing keys");
    return;
  }
  let completed = 0;
  for (const batch of chunks(missing, config.translation_batch_size)) {
    const language = batch[0].pair.language;
    const inputs = Object.fromEntries(batch.map((item, index) => [`k${index}`, {
      key: item.key, source: item.source, namespace: item.pair.namespace, context: item.context,
    }]));
    const result = await callLLMWithRetry(
      llm,
      "You are a product UI translator. Return only one JSON object mapping each input ID to its translated string. Preserve every {{placeholder}} exactly. Follow the glossary, forbidden terminology, capitalization, and do-not-translate list. Do not add commentary.",
      JSON.stringify({ source_language: config.source_language, target_language: language, glossary: glossaryContext(glossary, language), inputs }),
      config.request_timeout_seconds,
      Number(config.request_retries ?? 0),
      `translation batch ending at ${completed + batch.length}/${missing.length}`,
      undefined,
      (value) => validateTranslationBatch(value, batch),
    );
    for (const [index, item] of batch.entries()) {
      const translated = result[`k${index}`];
      if (typeof translated !== "string" || !translated.trim()) throw new Error(`translator omitted ${itemIdentity(item.pair, item.key)}`);
      if (!samePlaceholders(item.source, translated)) throw new Error(`translator changed placeholders for ${itemIdentity(item.pair, item.key)}`);
      setNested(item.targetObject, item.key, translated);
    }
    completed += batch.length;
    console.log(`i18n translate: ${completed}/${missing.length} keys`);
  }
  for (const [file, value] of targets) writeJSON(file, value);
  console.log(`i18n translate completed: wrote ${missing.length} keys; run make i18n-check`);
}

function currentReviewItems(config) {
  const items = [];
  for (const pair of resourcePairs(config)) {
    const source = flattenStrings(readJSON(pair.sourceFile));
    const target = flattenStrings(readJSON(pair.targetFile));
    const sourceEntries = [...source];
    for (const [key, sourceText] of sourceEntries) {
      const translation = target.get(key);
      if (typeof translation !== "string") throw new Error(`${itemIdentity(pair, key)} has no target translation`);
      const context = neighboringContext(sourceEntries, key).map((neighbor) => ({
        ...neighbor,
        translation: target.get(neighbor.key) ?? "",
      }));
      items.push({
        id: itemIdentity(pair, key), pair, key, source: sourceText, translation,
        source_hash: hash(sourceText), translation_hash: hash(translation), context,
      });
    }
  }
  return items;
}

function normalizeReview(value) {
  if (!value || typeof value !== "object") return { verdict: "fail", score: 0, reason: "Reviewer returned an invalid result" };
  const verdict = value.verdict === "pass" ? "pass" : "fail";
  return {
    verdict,
    score: Number.isFinite(Number(value.score)) ? Number(value.score) : undefined,
    terminology_violations: Array.isArray(value.terminology_violations) ? value.terminology_violations.map(String) : [],
    suggestion: typeof value.suggestion === "string" ? value.suggestion : "",
    reason: typeof value.reason === "string" ? value.reason : "",
  };
}

function markdownCell(value) {
  return String(value ?? "").replace(/\r?\n/g, "<br>").replace(/\|/g, "\\|");
}

function writeReviewReport(state, itemByID, summary) {
  const failures = [];
  for (const [id, entry] of Object.entries(state.entries)) {
    const item = itemByID.get(id);
    if (item && entry.verdict === "fail" && entry.source_hash === item.source_hash && entry.translation_hash === item.translation_hash) {
      failures.push({ id, entry, item });
    }
  }
  const lines = [
    "# i18n Translation Review",
    "",
    `- Review round: ${state.review_round}`,
    `- Reviewed this run: ${summary.reviewed}`,
    `- Passed this run: ${summary.passed}`,
    `- Rejected this run: ${summary.failed}`,
    `- Pending after sampling: ${summary.pending}`,
    `- Request batches failed this run: ${summary.requestFailures ?? 0}`,
    `- Current unresolved rejections: ${failures.length}`,
    "",
  ];
  if (failures.length) {
    lines.push("| Key | Source | Translation | Score | Terminology violations | Reviewer reason | Suggested translation |", "|---|---|---|---:|---|---|---|");
    for (const { id, entry, item } of failures) {
      lines.push(`| ${markdownCell(id)} | ${markdownCell(item.source)} | ${markdownCell(item.translation)} | ${markdownCell(entry.score)} | ${markdownCell(entry.terminology_violations?.join(", "))} | ${markdownCell(entry.reason)} | ${markdownCell(entry.suggestion)} |`);
    }
    lines.push("", "Correct the locale values manually, then rerun `make i18n-review`. The reviewer never changes translations automatically.", "");
  } else {
    lines.push("No unresolved reviewer rejections.", "");
  }
  fs.writeFileSync(reportPath, `${lines.join("\n")}\n`);
  return failures.length;
}

async function review(config, glossary, llm) {
	const items = currentReviewItems(config);
	const itemByID = new Map(items.map((item) => [item.id, item]));
	const previous = fs.existsSync(statePath) ? readJSON(statePath) : { version: 1, review_round: 0, entries: {} };
	// Review state is a cache, not a historical ledger. Drop entries for keys
	// that no longer exist in the locale before any subsequent write.
	const liveEntries = Object.fromEntries(Object.entries(previous.entries ?? {}).filter(([id]) => itemByID.has(id)));
	const pruned = Object.keys(liveEntries).length !== Object.keys(previous.entries ?? {}).length;
	if (!llm) {
		if (pruned) writeJSON(statePath, { ...previous, entries: liveEntries });
		console.log(`i18n review skipped: configure ${path.relative(root, secretsPath)}`);
		return;
	}
  const state = { version: 1, review_round: Number(previous.review_round ?? 0) + 1, entries: {} };
  const candidates = [];
  const forcedReviewIDs = new Set();
  for (const item of items) {
    const old = previous.entries?.[item.id];
    const retryableReviewError = old?.verdict === "fail" && old?.score === 0 && old?.reason === "Reviewer returned an invalid result";
    const changed = !old || old.source_hash !== item.source_hash || old.translation_hash !== item.translation_hash;
    const entry = changed || retryableReviewError ? {
      source_hash: item.source_hash, translation_hash: item.translation_hash, verdict: "pending",
    } : old;
    state.entries[item.id] = entry;
    if (changed || retryableReviewError || entry.verdict === "pending") {
      candidates.push(item);
      if ((changed && old?.verdict === "fail") || retryableReviewError) forcedReviewIDs.add(item.id);
    }
  }
  const rate = Number(config.review_sample_rate);
  if (!Number.isFinite(rate) || rate < 0 || rate > 1) throw new Error("review_sample_rate must be between 0 and 1");
  const selected = forcedReviewIDs.size > 0
    ? candidates.filter((item) => forcedReviewIDs.has(item.id))
    : candidates.filter((item) => sampleSelected(item.id, state.review_round, rate));
  const concurrency = Math.max(1, Math.floor(Number(config.review_concurrency ?? 1)));
  const reviewBatches = chunks(selected, config.review_batch_size);
  const run = {
    started_at: new Date().toISOString(),
    total_items: items.length,
    candidate_items: candidates.length,
    selected_items: selected.length,
    forced_rechecks: forcedReviewIDs.size,
    sampled_items: selected.length - forcedReviewIDs.size,
    batch_size: config.review_batch_size,
    concurrency,
    batches_total: reviewBatches.length,
    batches_started: 0,
    batches_succeeded: 0,
    batches_failed: 0,
    request_attempts_started: 0,
    entries_submitted: 0,
    entries_reviewed: 0,
  };
  state.last_run = run;
  writeJSON(statePath, state);
  if (candidates.length === 0) {
    run.completed_at = new Date().toISOString();
    writeJSON(statePath, state);
    const unresolved = writeReviewReport(state, itemByID, { reviewed: 0, passed: 0, failed: 0, pending: 0 });
    console.log(`i18n review: all ${items.length} translations are unchanged; ${unresolved} unresolved rejection(s)`);
    if (unresolved) process.exitCode = 1;
    return;
  }
  let reviewed = 0;
  let passed = 0;
  let failed = 0;
  let nextBatch = 0;
  let requestFailures = 0;
  async function reviewWorker() {
    while (true) {
      const batchNumber = nextBatch++;
      if (batchNumber >= reviewBatches.length) return;
      const batch = reviewBatches[batchNumber];
      const language = batch[0].pair.language;
      const inputs = Object.fromEntries(batch.map((item, index) => [`k${index}`, {
        key: item.key, namespace: item.pair.namespace, source: item.source, translation: item.translation, context: item.context,
      }]));
      run.batches_started++;
      run.entries_submitted += batch.length;
      run.last_activity_at = new Date().toISOString();
      writeJSON(statePath, state);
      let result;
      try {
        result = await callLLMWithRetry(
          llm,
          "You are a senior bilingual localization reviewer. Review meaning, product terminology, placeholders, fluency, brevity, consistency with the provided neighboring namespace context, and UI suitability. Return only one JSON object keyed by input ID. Each value must contain verdict ('pass' or 'fail'), score (0-100), terminology_violations (array), reason, and suggestion. A fail suggestion is advisory only; never rewrite source data.",
          JSON.stringify({ source_language: config.source_language, target_language: language, glossary: glossaryContext(glossary, language), inputs }),
          config.request_timeout_seconds,
          Number(config.request_retries ?? 0),
          `review batch ${batchNumber + 1}/${reviewBatches.length}`,
          () => {
            run.request_attempts_started++;
            run.last_activity_at = new Date().toISOString();
            writeJSON(statePath, state);
          },
          (value) => validateReviewBatch(value, batch.length),
        );
      } catch (error) {
        requestFailures++;
        run.batches_failed++;
        run.last_activity_at = new Date().toISOString();
        writeJSON(statePath, state);
        console.error(`i18n review: batch ${batchNumber + 1}/${reviewBatches.length} remains pending: ${error instanceof Error ? error.message : String(error)}`);
        continue;
      }
      const reviewedAt = new Date().toISOString();
      for (const [index, item] of batch.entries()) {
        const normalized = normalizeReview(result[`k${index}`]);
        state.entries[item.id] = {
          source_hash: item.source_hash,
          translation_hash: item.translation_hash,
          ...normalized,
          reviewed_at: reviewedAt,
          reviewer_model: llm.model,
        };
        reviewed++;
        if (normalized.verdict === "pass") passed++; else failed++;
      }
      run.batches_succeeded++;
      run.entries_reviewed = reviewed;
      run.last_activity_at = new Date().toISOString();
      writeJSON(statePath, state);
      console.log(`i18n review: ${reviewed}/${selected.length} selected translations`);
    }
  }
  await Promise.all(Array.from({ length: Math.min(concurrency, reviewBatches.length) }, () => reviewWorker()));
  run.completed_at = new Date().toISOString();
  writeJSON(statePath, state);
  const stillPending = Object.values(state.entries).filter((entry) => entry.verdict === "pending").length;
  const unresolved = writeReviewReport(state, itemByID, { reviewed, passed, failed, pending: stillPending, requestFailures });
  console.log(`i18n review completed: ${passed} passed, ${failed} rejected, ${stillPending} pending, ${requestFailures} request batch failure(s), ${unresolved} unresolved`);
  if (unresolved || requestFailures) process.exitCode = 1;
}

async function main() {
  const command = process.argv[2];
  if (command !== "translate" && command !== "review") {
    throw new Error("usage: node scripts/i18n-pipeline.mjs <translate|review>");
  }
  const config = readJSON(configPath);
  const glossary = readJSON(glossaryPath);
  const fileEnv = parseEnvFile(secretsPath);
  if (command === "translate") await translate(config, glossary, llmConfig("translator", fileEnv));
  else await review(config, glossary, llmConfig("reviewer", fileEnv));
}

const invokedPath = process.argv[1] ? pathToFileURL(path.resolve(process.argv[1])).href : "";
if (import.meta.url === invokedPath) {
  main().catch((error) => {
    console.error(`i18n pipeline failed: ${error instanceof Error ? error.message : String(error)}`);
    process.exitCode = 1;
  });
}
