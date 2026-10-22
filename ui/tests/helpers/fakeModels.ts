import type { List, ListQuery, Model, ModelPage } from "../../src/api";

const presence = { ready_nodes: 0, cached_nodes: 0, ready_storages: 0, in_flight: 0, in_flight_percent: 0, failed: 0 };

// A fixture model needs only the fields a test cares about.
export type FixtureModel = Partial<Model> & { id: number; presence?: Partial<typeof presence> };

/**
 * fakeModels answers the model endpoints as the server does: one page of
 * list rows (GET /models?page, with q a case-insensitive substring of the
 * name, source id or revision, source alternatives, and source_id matching
 * whole ids ignoring case), one model by id (an unknown id fails as a 404
 * does), and listing every model fails the test. `models` is the live
 * registry: a test may push to it, as a registration does.
 */
export function fakeModels(models: FixtureModel[]) {
  const pages: ListQuery[] = [];
  const reads: number[] = [];
  const fake = {
    models,
    pages,
    reads,
    /** How often every model was listed: a caller may swallow the failure. */
    listedAll: 0,
    models_all: async (): Promise<List<Model>> => {
      fake.listedAll++;
      throw new Error("the page fetched every model");
    },
    modelsPage: async (query: ListQuery): Promise<ModelPage> => {
      pages.push(query);
      const q = query.q?.trim().toLowerCase() ?? "";
      const sources = query.extra?.source ?? [];
      const ids = (query.extra?.source_id ?? []).map((id) => id.toLowerCase());
      const matched = models.filter((m) =>
        (!q || [m.name ?? "", m.source_id ?? "", m.revision ?? ""].some((name) => name.toLowerCase().includes(q))) &&
        (!sources.length || sources.includes(m.source ?? "")) &&
        (!ids.length || ids.includes((m.source_id ?? "").toLowerCase())),
      );
      const start = (query.page - 1) * query.page_size;
      return {
        items: matched.slice(start, start + query.page_size).map(({ nodes: _n, storages: _s, readme: _r, presence: own, ...row }) => ({ ...row, presence: { ...presence, ...own } })) as never,
        page: query.page,
        page_size: query.page_size,
        total: matched.length,
        summary: { registered: models.length, ready_files: 0, sources: 0, weight_gib: 0 },
      };
    },
    model: async (id: number): Promise<Model> => {
      reads.push(id);
      const found = models.find((m) => m.id === id);
      if (!found) throw new Error("model not found");
      return found as Model;
    },
  };
  return fake;
}

/** installModels puts fakeModels on the api object for the test's duration. */
export function installModels(api: Record<string, unknown>, models: FixtureModel[]) {
  const fake = fakeModels(models);
  api.models = fake.models_all;
  api.modelsPage = fake.modelsPage;
  api.model = fake.model;
  return fake;
}
