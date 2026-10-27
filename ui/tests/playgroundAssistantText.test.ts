import assert from "node:assert/strict";
import { test } from "node:test";
import { assistantText } from "../src/pages/playground/assistantText.ts";

test("inline reasoning handles every opening and closing stream boundary", () => {
  const raw = "<think>A thought</think>The answer";
  for (let end = 1; end <= raw.length; end++) {
    const part = assistantText(raw.slice(0, end), "", true);
    assert.ok(!part.content.includes("think"), `answer leaked delimiter at ${end}`);
    assert.ok(!part.reasoning.includes("think"), `reasoning leaked delimiter at ${end}`);
    if (end < "<think>".length) assert.equal(part.content, "");
  }
  assert.deepEqual(assistantText(raw), { content: "The answer", reasoning: "A thought" });
  assert.deepEqual(assistantText(" \n<think>Thought</think>Answer", "Separate thought"), { content: "Answer", reasoning: "Separate thought\n\nThought" });
  assert.deepEqual(assistantText("<think>unfinished"), { content: "", reasoning: "unfinished" });
  assert.deepEqual(assistantText("<thi"), { content: "<thi", reasoning: "" });
});

test("closing-only reasoning ignores markers in Markdown code and escaped literals", () => {
  for (const content of [
    "Use `<think>hidden</think>` tags.",
    "``<think>`quoted`</think>``",
    "```xml\n<think>example</think>\n```",
    "~~~xml\n<think>example</think>\n~~~",
    "````xml\n```\n</think>\n````",
    "An escaped \\</think> marker",
    "Plain answer with no reasoning",
  ]) assert.deepEqual(assistantText(content), { content, reasoning: "" });
  for (const thought of [
    "A thought with `code`",
    "A thought ending with `code\\`",
    "Consider `</think>` as a literal",
    "```xml\n</think>\n```\nThe actual thought",
    "~~~xml\n</think>\n~~~\nThe actual thought",
  ]) assert.deepEqual(assistantText(`${thought}</think>Answer`), { content: "Answer", reasoning: thought });
});
