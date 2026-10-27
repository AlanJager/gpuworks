/** Derive display/history text without changing the original persisted response. */
export function assistantText(content: string, reasoning = "", streaming = false) {
  const trimmed = content.trimStart();
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
// Ignore markers shown as Markdown code, including variable-length fences.
function closingTag(text: string): number {
  let fence = "";
  let inline = 0;
  for (let i = 0; i < text.length; i++) {
    if (i === 0 || text[i - 1] === "\n") {
      const match = /^ {0,3}(`{3,}|~{3,})([^\n]*)/.exec(text.slice(i));
      if (match) {
        const marker = match[1];
        if (!fence) {
          fence = marker;
          inline = 0;
        } else if (marker[0] === fence[0] && marker.length >= fence.length && !match[2].trim()) {
          fence = "";
        }
        i += match[0].length - 1;
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
