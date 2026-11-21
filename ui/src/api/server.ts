export interface Health {
  status: string;
  version: string;
  /** Build edition for display only ("community" for the community build). */
  edition: string;
  nodes: number;
  sessions: number;
}
