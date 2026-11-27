/**
 * shortBuild shortens a long build id to its first and last segments. A build
 * id reads `<source commit>-<middle>-<build suffix>`; two builds of one
 * product share the middle and differ at the ends, so those are what stay
 * (1c9a7b6a-ee.4935a06-agentupg2 becomes 1c9a7b6a…agentupg2). Releases and
 * other short ids come back unchanged.
 */
const LIMIT = 20;

export function shortBuild(value: string | undefined): string {
  const id = (value ?? "").trim();
  if (id.length <= LIMIT) return id;
  const parts = id.split("-");
  if (parts.length >= 3) return `${parts[0]}…${parts[parts.length - 1]}`;
  return `${id.slice(0, LIMIT / 2)}…${id.slice(-LIMIT / 2)}`;
}
