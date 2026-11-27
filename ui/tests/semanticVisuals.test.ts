import assert from "node:assert/strict";
import fs from "node:fs";
import { test } from "node:test";
import { terminalOptions, logColors, logEventStyle, logReplicaColors, logSelection, logSurfaceStyle,
  fixedDarkSelectionStyle, fixedDarkSelectionProperty, provideFixedDarkSelection, onBrandSelection } from "../src/theme/terminalTheme";
import { zeronTokens } from "../src/theme/zeron.generated";

function luminance(hex: string) {
  const rgb = hex.slice(1).match(/../g)!.map((v) => Number.parseInt(v, 16) / 255)
    .map((v) => v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4);
  return rgb[0] * 0.2126 + rgb[1] * 0.7152 + rgb[2] * 0.0722;
}
test("terminal purple and foreground remain readable on its fixed dark background", () => {
  const theme = terminalOptions.theme;
  for (const key of ["foreground", "magenta", "brightMagenta"] as const) {
    const contrast = (luminance(theme[key]) + 0.05) / (luminance(theme.background) + 0.05);
    assert.ok(contrast >= 4.5, `${key}: ${contrast}`);
  }
  assert.equal(terminalOptions.minimumContrastRatio, 4.5, "also correct low contrast ANSI/256-colour output dynamically");
  assert.equal(terminalOptions.fontSize, 14);
  assert.equal(Object.keys(theme).length, 20, "explicitly configure all 16 ANSI colours plus background/foreground/cursor/selection");
});

test("platform log events are information blue and all log levels are readable on dark surfaces", () => {
  assert.equal(logEventStyle.color, logColors.info);
  for (const key of ["foreground", "muted", "info", "warning", "error"] as const) {
    for (const background of [logColors.background, logColors.eventBackground]) {
      assert.ok((luminance(logColors[key]) + 0.05) / (luminance(background) + 0.05) >= 4.5, key);
    }
  }
});

test("every replica prefix remains readable on both dark log backgrounds", () => {
  for (const color of logReplicaColors) for (const background of [logColors.background, logColors.eventBackground]) {
    assert.ok((luminance(color) + 0.05) / (luminance(background) + 0.05) >= 4.5, color);
  }
});

// GWKS-540: the global ::selection reads --text-selection from the element the
// text is in. Log surfaces are dark in both themes, so they carry the dark
// theme's value; the light theme's tint is 1.19:1 against this background.
const appCss = fs.readFileSync(new URL("../src/app.css", import.meta.url), "utf8");
const tint = (theme: string) => {
  const blocks = [...appCss.matchAll(new RegExp(`(?:^|\\n)${theme}\\s*\\{([^}]*)\\}`, "g"))]
    .map((m) => m[1].match(/--text-selection:\s*([^;]+);/)?.[1]).filter(Boolean);
  assert.equal(blocks.length, 1, `${theme} defines --text-selection once`);
  return blocks[0]!;
};

test("log surfaces select text with the dark theme's tint in both themes", () => {
  const style = logSurfaceStyle as Record<string, unknown>;
  assert.equal(style["--text-selection"], logSelection);
  assert.equal(logSelection, tint("\\.dark").replace("var(--brand)", zeronTokens.dark.brand));

  const mixed = (theme: "light" | "dark") => {
    const percent = tint(theme === "dark" ? "\\.dark" : ":root")
      .match(/^color-mix\(in srgb, var\(--brand\) (\d+)%, transparent\)$/)![1];
    const share = Number(percent) / 100;
    const channels = (hex: string) => hex.slice(1).match(/../g)!.map((v) => Number.parseInt(v, 16));
    const brand = channels(zeronTokens[theme].brand), under = channels(logColors.background);
    return `#${brand.map((v, i) => Math.round(v * share + under[i] * (1 - share)).toString(16).padStart(2, "0")).join("")}`;
  };
  const against = (a: string, b: string) => (Math.max(luminance(a), luminance(b)) + 0.05) / (Math.min(luminance(a), luminance(b)) + 0.05);
  const onLog = against(mixed("dark"), logColors.background);
  assert.ok(onLog > against(mixed("light"), logColors.background), "the dark tint stands out more on the log surface than the light one");
  assert.ok(onLog >= 1.7, `selection vs log background: ${onLog}`);
  assert.ok(against(logColors.foreground, mixed("dark")) >= 4.5, "log text stays readable while selected");
});

// Stylesheet surfaces with a literal dark background get the same tint through
// theme/layers.css. Every literal dark background in src/**/*.css must be on
// that list (or inside an element on it), or be excluded here with a reason.
const notSelectableText: Record<string, string> = {
  ".playground-column-number": "a 24px column badge holding one digit",
  ".instance-progress-unknown span": "an indeterminate progress bar",
  ".deploy-gpu-grid button.is-picked": "a picked GPU toggle button",
  ".playground-stream-cursor": "the streaming cursor block",
  ".scheduling-swatch.is-awake": "a legend swatch", ".scheduling-bar > span.is-awake": "a capacity bar segment",
  ".scheduling-swatch.is-sleeping": "a legend swatch", ".scheduling-bar > span.is-sleeping": "a capacity bar segment",
};
// A saturated mid-tone fill, the brand blue among them: neither theme's
// brand-tinted selection shows on it.
const saturatedFill = (hex: string) => {
  const [r, g, b] = hex.slice(1).match(/../g)!.map((v) => Number.parseInt(v, 16) / 255);
  const max = Math.max(r, g, b), min = Math.min(r, g, b), light = (max + min) / 2;
  const saturation = max === min ? 0 : (max - min) / (1 - Math.abs(2 * light - 1));
  return saturation >= 0.5 && luminance(hex) > 0.05 && luminance(hex) <= 0.5;
};
const darkFill = (hex: string) => luminance(hex) <= 0.05;
// Commas inside :is(...)/:not(...) do not separate selectors.
function splitSelectors(list: string) {
  const out: string[] = [];
  let depth = 0, start = 0;
  for (let i = 0; i < list.length; i++) {
    if (list[i] === "(") depth++;
    else if (list[i] === ")") depth--;
    else if (list[i] === "," && depth === 0) { out.push(list.slice(start, i)); start = i + 1; }
  }
  out.push(list.slice(start));
  return out.map((v) => v.trim()).filter(Boolean);
}
const cssRules = (css: string) => [...css.replace(/\/\*[\s\S]*?\*\//g, "").matchAll(/([^{}]+)\{([^{}]*)\}/g)]
  .map((m) => ({ selectors: splitSelectors(m[1]), body: m[2] }));
const layersCss = fs.readFileSync(new URL("../src/theme/layers.css", import.meta.url), "utf8");
const selectionRule = cssRules(layersCss).filter((rule) => /--text-selection:/.test(rule.body));

test("stylesheet surfaces dark in both themes take the dark selection tint from the root", () => {
  assert.equal(selectionRule.length, 1, "layers.css sets --text-selection in one rule");
  assert.equal(selectionRule[0].body.match(/--text-selection:\s*([^;]+);/)![1],
    `var(${fixedDarkSelectionProperty}, ${tint("\\.dark")})`, "the fallback is the .dark token itself");

  const set: Record<string, string> = {};
  provideFixedDarkSelection({ style: { setProperty: (key: string, value: string) => { set[key] = value; } } } as unknown as HTMLElement);
  assert.deepEqual(set, { [fixedDarkSelectionProperty]: logSelection });
  assert.equal((fixedDarkSelectionStyle as Record<string, unknown>)["--text-selection"], logSelection);
  const main = fs.readFileSync(new URL("../src/main.tsx", import.meta.url), "utf8");
  assert.match(main, /provideFixedDarkSelection\(document\.documentElement\)/, "the console sets the root property");
});

test("every literal dark or saturated background in the stylesheets selects visibly or holds no text", () => {
  const listed = selectionRule[0].selectors;
  // The surface itself, the surface in a narrower context, or something inside it.
  const covered = (selector: string) => listed.some((surface) =>
    selector === surface || selector.endsWith(` ${surface}`) || selector.startsWith(`${surface} `));
  const root = new URL("../src/", import.meta.url);
  const files = (fs.readdirSync(root, { recursive: true }) as string[])
    .filter((file) => file.endsWith(".css") && file !== "app.css");
  const missing: string[] = [];
  const excused = new Set<string>();
  let seen = 0;
  for (const file of files) for (const rule of cssRules(fs.readFileSync(new URL(file, root), "utf8"))) {
    const colour = rule.body.match(/background(?:-color)?:\s*(#[0-9a-fA-F]{6})\b/)?.[1];
    if (!colour || !(darkFill(colour) || saturatedFill(colour))) continue;
    for (const raw of rule.selectors) {
      const selector = raw.replace(/(?::(?:hover|focus|focus-visible|focus-within|active)|:not\([^)]*\))+$/g, "");
      if (/::(?:before|after)$/.test(selector)) continue; // generated content is not selectable
      seen++;
      // Stylesheets have no on-brand selection rule yet: a saturated fill with
      // selectable text needs one, as layers.css does for dark surfaces.
      const ok = darkFill(colour) ? covered(selector) : false;
      if (selector in notSelectableText) excused.add(selector);
      else if (!ok) missing.push(`${file}: ${raw} (${colour})`);
    }
  }
  assert.ok(seen >= listed.length, `the scan finds the surfaces it guards (${seen})`);
  assert.deepEqual(missing, []);
  assert.deepEqual(Object.keys(notSelectableText).filter((selector) => !excused.has(selector)), [], "every exclusion still matches a rule");
});

// GWKS-540: inline styles get the same scan. A literal dark fill spreads
// fixedDarkSelectionStyle, a saturated one onBrandSelectionStyle, in the same
// style object.
test("every inline literal dark or saturated background carries a selection tint that shows on it", () => {
  const root = new URL("../src/", import.meta.url);
  const files = (fs.readdirSync(root, { recursive: true }) as string[]).filter((file) => file.endsWith(".tsx"));
  const found: string[] = [], missing: string[] = [];
  for (const file of files) {
    const source = fs.readFileSync(new URL(file, root), "utf8");
    for (const match of source.matchAll(/background(?:Color)?:\s*["'](#[0-9a-fA-F]{6})["']/g)) {
      const colour = match[1];
      const kind = darkFill(colour) ? "fixedDarkSelectionStyle" : saturatedFill(colour) ? "onBrandSelectionStyle" : "";
      if (!kind) continue;
      const object = source.slice(source.lastIndexOf("{", match.index), source.indexOf("}", match.index));
      found.push(`${file} ${colour}`);
      if (!object.includes(`...${kind}`)) missing.push(`${file}: ${colour} needs ...${kind}`);
    }
  }
  assert.ok(found.length >= 2, `the scan finds the doctor report and the assistant bubble: ${found.join(", ")}`);
  assert.deepEqual(missing, []);
});

test("a question on the brand-blue bubble stays visible when selected, and its white text readable", () => {
  const panel = fs.readFileSync(new URL("../src/assistant/AssistantPanel.tsx", import.meta.url), "utf8");
  const [, fill, ink] = panel.match(/\.\.\.onBrandSelectionStyle,[^}]*background: "(#[0-9a-fA-F]{6})", color: "#(f{3}|f{6})"/)!;
  const white = "#ffffff";
  assert.equal(ink.length === 3 ? "#ffffff" : `#${ink}`, white);
  const percent = onBrandSelection.match(/^color-mix\(in srgb, (#[0-9a-f]{6}) (\d+)%, transparent\)$/)!;
  assert.equal(percent[1], zeronTokens.dark["fg-on-brand"]);
  const share = Number(percent[2]) / 100;
  const channels = (hex: string) => hex.slice(1).match(/../g)!.map((v) => Number.parseInt(v, 16));
  const over = (tint: string, under: string) => `#${channels(tint).map((v, i) => Math.round(v * share + channels(under)[i] * (1 - share)).toString(16).padStart(2, "0")).join("")}`;
  const against = (a: string, b: string) => (Math.max(luminance(a), luminance(b)) + 0.05) / (Math.min(luminance(a), luminance(b)) + 0.05);
  const selected = over(percent[1], fill);
  assert.ok(against(selected, fill) >= 1.5, `selection vs bubble: ${against(selected, fill)}`);
  assert.ok(against(white, selected) >= 4.5, `white text on the selection: ${against(white, selected)}`);
  // What the page's own tints would give, for the record: neither shows.
  for (const theme of ["light", "dark"] as const) {
    const brand = tint(theme === "dark" ? "\\.dark" : ":root").match(/(\d+)%/)![1];
    const page = `#${channels(zeronTokens[theme].brand).map((v, i) => Math.round(v * Number(brand) / 100 + channels(fill)[i] * (1 - Number(brand) / 100)).toString(16).padStart(2, "0")).join("")}`;
    assert.ok(against(page, fill) < 1.3, `${theme} page tint on the bubble: ${against(page, fill)}`);
  }
});
