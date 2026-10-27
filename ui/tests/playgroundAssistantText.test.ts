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

test("literal delimiters in indented code and blockquotes are not removed from answers", () => {
  for (const content of [
    "The closing marker can be written as indented code:\n\n    </think>\n\nKeep the rest of the explanation.",
    "The closing marker can be written as indented code:\n\n\t</think>\n\nKeep the rest of the explanation.",
    "    <think>literal code</think>",
    "\n\t<think>literal code</think>",
    "> ```xml\n> </think>\n> ```\n\nAn explanation.",
    "> ~~~xml\n> </think>\n> ~~~\n\nAn explanation.",
    ">     <think>literal code</think>\n\nAn explanation.",
    "> Quoted </think> marker\n\nAn explanation.",
  ]) {
    assert.deepEqual(assistantText(content), { content, reasoning: "" });
    assert.deepEqual(assistantText(content, "", true), { content, reasoning: "" });
  }
  const thought = "Consider this code:\n\n    </think>\n\nOr a quote:\n> ```xml\n> </think>\n> ```\n\nThe actual reasoning";
  assert.deepEqual(assistantText(`${thought}</think>Answer`), { content: "Answer", reasoning: thought });
  assert.deepEqual(assistantText(`<think>${thought}</think>Answer`), { content: "Answer", reasoning: thought });
});

test("list-contained fences preserve literal markers and end before genuine reasoning delimiters", () => {
  for (const [start, indent] of [["- ", "  "], ["+ ", "  "], ["* ", "  "], ["1. ", "   "], ["12) ", "    "], ["- 1. ", "     "], ["  - ", "    "]]) {
    for (const fence of ["~~~", "```", "~~~~"]) {
      const example = `Example:\n\n${start}${fence}xml\n${indent}</think>\n${indent}${fence}\n\nKeep this answer.`;
      assert.deepEqual(assistantText(example), { content: example, reasoning: "" });
      const thought = `${example}\n\nThe actual thought`;
      assert.deepEqual(assistantText(`${thought}</think>Answer`), { content: "Answer", reasoning: thought });
      assert.deepEqual(assistantText(`<think>${thought}</think>Answer`), { content: "Answer", reasoning: thought });
      const unclosed = `Example:\n\n${start}${fence}xml\n${indent}</think>\n\nOutside the list`;
      assert.deepEqual(assistantText(`${unclosed}</think>Answer`), { content: "Answer", reasoning: unclosed });
    }
  }
});
