// ui_locale_changes: whether a change to the console's locale JSON can alter
// what an existing page renders. The browser check picks the pages to walk
// from the import graph, and locale files are not in it, so a locale change
// walks every page. That is only wasted when the change adds texts no page can
// render yet; this module proves that case and nothing wider. Node built-ins
// only.
//
// A change is inert when, across every language of the namespace:
//   - it only adds keys (no value changed, no key or file removed);
//   - each added key is added in every language, so no language starts
//     showing text it used to take from the fallback language;
//   - each added key sits under a prefix whose lookups a two-way check guards:
//     i18n-check fails when a product API error code has no apiError text and
//     when an apiError text has no code, so a new apiError.X belongs to a code
//     no existing console code path receives. Other prefixes are not
//     provable: 346 lookups take their key from a variable;
//   - no added key is a plural or context variant (FOO_one, FOO_zero) of a key
//     that already exists, or the base of existing variants (FOO beside
//     FOO_one), which would change how an existing lookup resolves;
//   - no source file outside the change names an added key literally, which
//     would make that file's pages render the new text.
// Anything else is "not inert", and the caller walks every page as before.

export const guardedPrefixes = [{ namespace: "common", prefix: "apiError." }];

function flatten(value, prefix = "", out = new Map()) {
  for (const [key, child] of Object.entries(value ?? {})) {
    const full = prefix ? `${prefix}.${key}` : key;
    if (child && typeof child === "object" && !Array.isArray(child)) flatten(child, full, out);
    else out.set(full, child);
  }
  return out;
}

// variantBases are the keys an added key would be a suffix variant of: for
// apiError.FOO_BAR_one, apiError.FOO_BAR and apiError.FOO.
function variantBases(key) {
  const dot = key.lastIndexOf(".");
  const parent = key.slice(0, dot + 1);
  const leaf = key.slice(dot + 1);
  const bases = [];
  for (let i = leaf.lastIndexOf("_"); i > 0; i = leaf.lastIndexOf("_", i - 1)) bases.push(parent + leaf.slice(0, i));
  return bases;
}

const quoted = (text, key) => [`"${key}"`, `'${key}'`, `\`${key}\``].some((needle) => text.includes(needle));

/**
 * @param {{ languages: string[],
 *           changes: { language: string, namespace: string, before: object|null, after: object|null }[],
 *           sources: { path: string, text: string }[] }} input
 *   languages: every locale directory; changes: each changed locale file
 *   (null = absent on that side); sources: the console sources the change does
 *   not touch.
 * @returns {{ inert: boolean, reason: string, added: string[] }}
 */
export function localeChangeImpact({ languages, changes, sources }) {
  const no = (reason) => ({ inert: false, reason, added: [] });
  const byNamespace = new Map();
  for (const change of changes) {
    if (change.after === null) return no(`${change.language}/${change.namespace}.json is removed`);
    const before = flatten(change.before);
    const after = flatten(change.after);
    for (const [key, value] of before) {
      if (!after.has(key)) return no(`${change.language}/${change.namespace}:${key} is removed`);
      if (after.get(key) !== value) return no(`${change.language}/${change.namespace}:${key} changes`);
    }
    const added = new Set([...after.keys()].filter((key) => !before.has(key)));
    const entry = byNamespace.get(change.namespace) ?? new Map();
    entry.set(change.language, { before, added });
    byNamespace.set(change.namespace, entry);
  }
  const addedKeys = [];
  for (const [namespace, perLanguage] of byNamespace) {
    const union = new Set([...perLanguage.values()].flatMap(({ added }) => [...added]));
    for (const language of languages) {
      const entry = perLanguage.get(language);
      for (const key of union) {
        if (!entry || !entry.added.has(key)) return no(`${namespace}:${key} is not added in ${language}`);
      }
    }
    for (const key of union) {
      const guarded = guardedPrefixes.some((g) => g.namespace === namespace && key.startsWith(g.prefix));
      if (!guarded) return no(`${namespace}:${key} is not under a prefix a two-way check guards`);
      for (const { before } of perLanguage.values()) {
        const base = variantBases(key).find((candidate) => before.has(candidate));
        if (base) return no(`${namespace}:${key} is a variant of the existing ${base}`);
        const variant = [...before.keys()].find((existing) => existing.startsWith(`${key}_`));
        if (variant) return no(`${namespace}:${key} is the base of the existing ${variant}`);
      }
      const named = sources.find(({ text }) => quoted(text, key) || quoted(text, `${namespace}:${key}`));
      if (named) return no(`${named.path} already names ${namespace}:${key}`);
      addedKeys.push(`${namespace}:${key}`);
    }
  }
  return { inert: true, reason: "only adds texts no existing page can render", added: addedKeys.sort() };
}
