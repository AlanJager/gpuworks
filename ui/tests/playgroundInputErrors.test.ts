import assert from "node:assert/strict";
import test from "node:test";
import { playgroundErrorText } from "../src/pages/playground/api.ts";
// SGLang wraps Pydantic errors in error.message. Pydantic's input_value
// contains the original base64 audio, repeated for its union alternatives.
test("SGLang validation errors cannot echo megabytes of request audio", () => {
    const audio = "UklGR" + "A".repeat(6492000);
    const error = `25 validation errors for 'Value_error'\nmessages.3.function-after[validate_thinking_parts_role(), ChatCompletionMessage_param]\n  Value error, role must be one of system, assistant, tool, function, developer, latest_reminder [type=value_error, input_value={'role': 'user', 'content': [{'type': 'input_audio', 'input_audio': {'data': '${audio}', 'format':'wav'}}]}, input_type=dict]`;
    const text = playgroundErrorText(400, JSON.stringify({ error: { message: error, type: "BadRequestError", code: 400 } }));
    assert.ok(text.length <= 2048, `error display length ${text.length}`);
    assert.doesNotMatch(text, /AAAA{20}|input_value|UklGR/);
});
test("structured validation input is not displayed even when short", () => {
    const text = playgroundErrorText(422, JSON.stringify({ detail: [{ type: "value_error", loc: ["body", "messages", 0], msg: "Audio input is unsupported", input: { input_audio: { data: "UklGRPRIVATE", format: "wav" } } }] }));
    assert.match(text, /Audio input is unsupported/);
    assert.doesNotMatch(text, /UklGRPRIVATE|input_audio/);
});
test("bounded chunked error reading cancels without consuming the entire payload", async () => {
    const { readErrorBody, MAX_ERROR_BYTES } = await import("../src/pages/playground/errors.ts");
    let chunks = 0, cancelled = false;
    const response = new Response(new ReadableStream<Uint8Array>({ pull(controller) { chunks++; controller.enqueue(new Uint8Array(4096).fill(65)); }, cancel() { cancelled = true; } }));
    assert.equal(await readErrorBody(response), undefined);
    assert.equal(cancelled, true);
    assert.ok(chunks <= MAX_ERROR_BYTES / 4096 + 2, `read ${chunks} chunks`);
});
test("error reader preserves cancellation and useful compact errors", async () => {
    const { readErrorBody } = await import("../src/pages/playground/errors.ts");
    const abort = new DOMException("Aborted", "AbortError");
    const failed = new Response(new ReadableStream({ start(controller) { controller.error(abort); } }));
    await assert.rejects(readErrorBody(failed), (error) => error === abort);
    assert.equal(playgroundErrorText(429, await readErrorBody(new Response('{"error":{"message":"Rate limit reached","code":"rate_limit"}}'))), "Rate limit reached");
    assert.equal(playgroundErrorText(502, "upstream unavailable"), "upstream unavailable");
    assert.equal(playgroundErrorText(502, "<html>gateway</html>"), "Request failed (502)");
});
test("declared oversized error is cancelled before reading its body", async () => {
    const { readErrorBody } = await import("../src/pages/playground/errors.ts");
    let reads = 0;
    let cancelled = false;
    const body = new ReadableStream<Uint8Array>({
        pull() { reads++; }, cancel() { cancelled = true; },
    }, { highWaterMark: 0 });
    assert.equal(await readErrorBody(new Response(body, { headers: { "Content-Length": "6492508" } })), undefined);
    assert.equal(reads, 0);
    assert.equal(cancelled, true);
});
