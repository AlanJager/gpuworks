// Every zh namespace, as one chunk the console loads when zh is in use.
export default import.meta.glob("./locales/zh/*.json", { eager: true, import: "default" }) as Record<string, Record<string, unknown>>;
