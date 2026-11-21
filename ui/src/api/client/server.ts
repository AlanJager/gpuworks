import type { PeerTransferSnapshot } from "../../peerTransferTypes";
import { getJSON } from "../http";
import type { Health } from "../server";

export const serverApi = {
  health: () => getJSON<Health>("/api/v1/healthz"),
  peerTransfers: (transferId?: string) => getJSON<PeerTransferSnapshot>(`/api/v1/peer-transfers${transferId ? `?transfer_id=${encodeURIComponent(transferId)}` : ""}`),
};
