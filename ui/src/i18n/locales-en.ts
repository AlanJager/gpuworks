// Every en namespace, as one chunk the console loads when en is in use.
export default import.meta.glob("./locales/en/*.json", { eager: true, import: "default" }) as Record<string, Record<string, unknown>>;
