import { getJSON, sendJSON } from "../http";
import type { PlatformSSHKey } from "../ssh";

export const sshApi = {
  platformSSHKey: () => getJSON<PlatformSSHKey>("/api/v1/ssh-key"),
  rotatePlatformSSHKey: () => sendJSON<PlatformSSHKey>("POST", "/api/v1/ssh-key/rotate"),
  importPlatformSSHKey: (privateKeyPEM: string) =>
    sendJSON<PlatformSSHKey>("PUT", "/api/v1/ssh-key", { private_key_pem: privateKeyPEM }),
};
