import type { Model } from "../../api";
import { fetchHuggingFaceMetadata, type BrowserModelMetadata } from "../../modelmeta";

type FetchMetadata = (repo: string, revision?: string) => Promise<BrowserModelMetadata>;
type PersistMetadata = (id: number, metadata?: BrowserModelMetadata & { browser_metadata: true }) => Promise<Model>;

// Hugging Face is fetched in the operator's browser first, at the addresses
// the management node's rules give, so a local proxy or network route is
// used. When the browser reads nothing (no allowed address, offline, blocked)
// the request goes without the browser marker and the server reads instead.
export async function refreshRemoteModelMetadata(
  model: Model,
  persistMetadata: PersistMetadata,
  fetchMetadata: FetchMetadata = fetchHuggingFaceMetadata,
): Promise<Model> {
  if (model.source.trim().toLowerCase() !== "huggingface") {
    return persistMetadata(model.id);
  }
  let metadata: BrowserModelMetadata;
  try {
    metadata = await fetchMetadata(model.source_id, model.revision || "main");
  } catch {
    return persistMetadata(model.id);
  }
  const { readme, config_raw, weight_bytes, logo_data } = metadata;
  return persistMetadata(model.id, { readme, config_raw, weight_bytes, logo_data, browser_metadata: true });
}
