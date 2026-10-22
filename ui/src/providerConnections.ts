import type { SSHConnection } from "./api";

// A provider command channel (Alibaba Cloud's Cloud Assistant) reaches the
// instance without SSH: it has no host, port or user to show.
export function isCommandChannel(connection: Pick<SSHConnection, "type">): boolean {
  return connection.type === "cloud_assistant";
}
