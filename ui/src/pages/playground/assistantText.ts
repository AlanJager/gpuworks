/** Derive display/history text without changing the original persisted response. */
export function assistantText(content: string, reasoning = "", streaming = false) {
  // Four spaces or a tab introduce Markdown code, not a reasoning prefix.
  const prefix = /^(?:[ \t]*\r?\n)* {0,3}/.exec(content)![0];
  const trimmed = content.slice(prefix.length);
  const opening = "<think>";
  const closing = "</think>";
  if (streaming && trimmed && opening.startsWith(trimmed)) return { content: "", reasoning };
  const explicit = trimmed.startsWith(opening);
  const body = explicit ? trimmed.slice(opening.length) : content;
  const end = closingTag(body);
  if (!explicit && end < 0) return { content, reasoning };
  let thought = end < 0 ? body : body.slice(0, end);
  // A streaming chunk may end in the middle of the closing marker.
  if (streaming && end < 0) {
    for (let size = closing.length - 1; size > 0; size--) {
      if (thought.endsWith(closing.slice(0, size))) {
        thought = thought.slice(0, -size);
        break;
      }
    }
  }
  return {
    content: end < 0 ? "" : body.slice(end + closing.length),
    reasoning: reasoning && thought ? `${reasoning}\n\n${thought}` : reasoning || thought,
  };
}

// A model can inherit <think> from its chat template and emit only </think>.
// Ignore markers shown as Markdown code or quotations.
function closingTag(text: string): number {
  let fence = "";
  let fenceIndent = 0;
  let inline = 0;
  for (let i = 0; i < text.length; i++) {
    if (i === 0 || text[i - 1] === "\n") {
      const end = text.indexOf("\n", i);
      const line = text.slice(i, end < 0 ? text.length : end);
      // A fence in a list ends with its containing item, even without an
      // explicit closing fence. Root-level fences continue across blank lines.
      if (fenceIndent && line.trim() && !line.startsWith(" ".repeat(fenceIndent))) {
        fence = "";
        fenceIndent = 0;
      }
      // Indented code and quoted examples retain their literal delimiters.
      if (!fence && /^(?: {4}| {0,3}\t| {0,3}>)/.test(line)) {
        if (end < 0) return -1;
        i = end;
        continue;
      }
      let offset = fence ? fenceIndent : 0;
      if (!fence) {
        // Fences may start on an ordered/unordered item, including nesting.
        let item: RegExpExecArray | null;
        while ((item = /^ {0,3}(?:[-+*]|\d{1,9}[.)])[ \t]+/.exec(line.slice(offset)))) offset += item[0].length;
      }
      const match = /^ {0,3}(`{3,}|~{3,})([^\n]*)/.exec(line.slice(offset));
      if (match) {
        const marker = match[1];
        if (!fence) {
          fence = marker;
          fenceIndent = offset;
          inline = 0;
        } else if (marker[0] === fence[0] && marker.length >= fence.length && !match[2].trim()) {
          fence = "";
          fenceIndent = 0;
        }
        i += line.length - 1;
        continue;
      }
    }
    if (fence) continue;
    if (!inline && text[i] === "\\") { i++; continue; }
    if (text[i] === "`") {
      let size = 1;
      while (text[i + size] === "`") size++;
      if (inline === size) inline = 0;
      else if (!inline && text.indexOf("`".repeat(size), i + size) >= 0) inline = size;
      i += size - 1;
      continue;
    }
    if (!inline && text.startsWith("</think>", i)) return i;
  }
  return -1;
}
