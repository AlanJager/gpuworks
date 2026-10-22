// Decode one EventSource payload from the instance log stream. New servers
// send plain text; accepting the old JSON-string shape keeps mixed-version
// server/UI rollouts readable as well.
export function decodeLogEvent(data: string): string {
  try {
    const decoded: unknown = JSON.parse(data);
    return typeof decoded === "string" ? decoded : data;
  } catch {
    return data;
  }
}

export interface AnsiSegment {
  text: string;
  style: {
    color?: string;
    backgroundColor?: string;
    fontWeight?: "bold";
    fontStyle?: "italic";
    textDecoration?: string;
    visibility?: "hidden";
  };
}

const ANSI_COLORS = [
  "#000000", "#cd3131", "#0dbc79", "#e5e510", "#2472c8", "#bc3fbc", "#11a8cd", "#e5e5e5",
  "#666666", "#e74856", "#23d18b", "#f5f543", "#3b8eea", "#d670d6", "#29b8db", "#ffffff",
];

function ansi256Color(value: number): string | undefined {
  if (!Number.isInteger(value) || value < 0 || value > 255) return undefined;
  if (value < 16) return ANSI_COLORS[value];
  if (value >= 232) {
    const level = 8 + (value - 232) * 10;
    return `rgb(${level}, ${level}, ${level})`;
  }
  const n = value - 16;
  const blue = n % 6;
  const green = Math.floor(n / 6) % 6;
  const red = Math.floor(n / 36);
  const channel = (v: number) => v === 0 ? 0 : 55 + v * 40;
  return `rgb(${channel(red)}, ${channel(green)}, ${channel(blue)})`;
}

function basicAnsiColor(code: number): string | undefined {
  if (code >= 30 && code <= 37) return ANSI_COLORS[code - 30];
  if (code >= 90 && code <= 97) return ANSI_COLORS[code - 90 + 8];
  if (code >= 40 && code <= 47) return ANSI_COLORS[code - 40];
  if (code >= 100 && code <= 107) return ANSI_COLORS[code - 100 + 8];
  return undefined;
}

type AnsiState = {
  color?: string;
  backgroundColor?: string;
  bold: boolean;
  italic: boolean;
  underline: boolean;
  strike: boolean;
  conceal: boolean;
};

function stateStyle(state: AnsiState): AnsiSegment["style"] {
  const style: AnsiSegment["style"] = {};
  if (state.color) style.color = state.color;
  if (state.backgroundColor) style.backgroundColor = state.backgroundColor;
  if (state.bold) style.fontWeight = "bold";
  if (state.italic) style.fontStyle = "italic";
  const decorations = [state.underline ? "underline" : "", state.strike ? "line-through" : ""].filter(Boolean);
  if (decorations.length) style.textDecoration = decorations.join(" ");
  if (state.conceal) style.visibility = "hidden";
  return style;
}

function applySGR(state: AnsiState, params: string): void {
  const values = params === "" ? [0] : params.split(";").map((value) => Number(value));
  for (let i = 0; i < values.length; i++) {
    const code = values[i];
    if (!Number.isInteger(code)) continue;
    if (code === 0) {
      state.color = undefined;
      state.backgroundColor = undefined;
      state.bold = state.italic = state.underline = state.strike = state.conceal = false;
    } else if (code === 1) state.bold = true;
    else if (code === 2) { /* dim has no reliable cross-browser equivalent */ }
    else if (code === 3) state.italic = true;
    else if (code === 4) state.underline = true;
    else if (code === 8) state.conceal = true;
    else if (code === 9) state.strike = true;
    else if (code === 22) state.bold = false;
    else if (code === 23) state.italic = false;
    else if (code === 24) state.underline = false;
    else if (code === 28) state.conceal = false;
    else if (code === 29) state.strike = false;
    else if (code === 39) state.color = undefined;
    else if (code === 49) state.backgroundColor = undefined;
    else if (code >= 30 && code <= 37 || code >= 90 && code <= 97) state.color = basicAnsiColor(code);
    else if (code >= 40 && code <= 47 || code >= 100 && code <= 107) state.backgroundColor = basicAnsiColor(code);
    else if (code === 38 || code === 48) {
      const isBackground = code === 48;
      const mode = values[++i];
      let color: string | undefined;
      if (mode === 5) color = ansi256Color(values[++i]);
      else if (mode === 2) {
        const r = values[++i], g = values[++i], b = values[++i];
        if ([r, g, b].every((channel) => Number.isInteger(channel) && channel >= 0 && channel <= 255)) {
          color = `rgb(${r}, ${g}, ${b})`;
        }
      }
      if (color) {
        if (isBackground) state.backgroundColor = color;
        else state.color = color;
      }
    }
  }
}

// ansiToSegments converts terminal output into safe text/style segments.
// SGR colors and text attributes are retained; other escape sequences and
// non-printing control characters are discarded instead of being rendered.
export function ansiToSegments(input: string): AnsiSegment[] {
  const out: AnsiSegment[] = [];
  const state: AnsiState = { bold: false, italic: false, underline: false, strike: false, conceal: false };
  let text = "";
  const flush = () => {
    if (text) {
      out.push({ text, style: stateStyle(state) });
      text = "";
    }
  };
  for (let i = 0; i < input.length;) {
    const code = input.charCodeAt(i);
    if (code === 0x1b) {
      flush();
      if (input[i + 1] === "[") {
        let end = i + 2;
        while (end < input.length && !(/[\x40-\x7e]/.test(input[end]))) end++;
        if (end < input.length) {
          if (input[end] === "m") applySGR(state, input.slice(i + 2, end));
          i = end + 1;
          continue;
        }
      } else if (input[i + 1] === "]") {
        let end = i + 2;
        while (end < input.length && input.charCodeAt(end) !== 0x07 && !(input.charCodeAt(end) === 0x1b && input[end + 1] === "\\")) end++;
        i = end < input.length && input.charCodeAt(end) === 0x1b ? end + 2 : Math.min(end + 1, input.length);
        continue;
      }
      i += Math.min(2, input.length - i);
      continue;
    }
    // Strip C0 controls (including carriage returns); tabs remain useful in
    // command output and are rendered naturally by the preformatted block.
    if ((code < 0x20 && code !== 0x09) || code === 0x7f || code === 0x9b) {
      i++;
      continue;
    }
    text += input[i++];
  }
  flush();
  return out;
}
