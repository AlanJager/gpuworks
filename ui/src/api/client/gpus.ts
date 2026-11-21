import { getJSON, listQueryString, type ListQuery } from "../http";
import type { GPUListResponse, GPUPage } from "../gpus";
import type { DGPUListResponse } from "../dgpus";

export const gpusApi = {
  gpus: () => getJSON<GPUListResponse>("/api/v1/gpus"),
  gpusPage: (query: ListQuery) => getJSON<GPUPage>(`/api/v1/gpus?${listQueryString(query)}`),
  dgpus: () => getJSON<DGPUListResponse>("/api/v1/dgpus"),
};
