// effectiveShmDefaultBytes is the /dev/shm size an empty field takes before
// the server has resolved the draft: the larger of the engine's default and
// the size the selected template spec states (shm_bytes), the same floor the
// server applies (deploy.launchShmBytes). The CPU memory tier is not known
// here; once the draft is evaluated the server's resolved size replaces this.
export function effectiveShmDefaultBytes(
  engineBytes: number | undefined,
  templateBytes: number | undefined,
): number | undefined {
  const larger = Math.max(engineBytes ?? 0, templateBytes ?? 0);
  return larger > 0 ? larger : undefined;
}
