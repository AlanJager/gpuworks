// A label filter entry is the "key=value" text shown on the label tags.
export function labelPair(key: string, value: string): string {
  return `${key}=${value}`;
}
