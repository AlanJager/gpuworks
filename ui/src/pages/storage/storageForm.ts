import { storageTypeInfo, type Storage, type StorageProbeRequest, type StorageRequest, type StorageTarget, type StorageType } from "../../storageTypes";

/**
 * Where a storage applies: every node, nodes carrying a label, or chosen nodes
 * one by one. "mixed" is a storage the server holds with more than one of those
 * (the API allows it): the form then shows all three and sends them back as
 * they were, instead of narrowing the storage to one kind.
 */
export type NodeScope = "all" | "labels" | "nodes" | "mixed";

export interface LabelTarget {
  key: string;
  value: string;
}

/** One chosen node and what is overridden on it. */
export interface NodeOverride {
  node_uuid: string;
  path: string;
  cache_path: string;
  existing: boolean;
  /** Configuration only: this row does not select the node into scope. */
  override_only?: boolean;
  /** Undefined inherits; zero is explicitly unlimited. */
  cache_max_bytes?: number;
}

export interface StorageFormValues {
  name: string;
  type: StorageType;
  path: string;
  export: string;
  mountOptions: string;
  cachePath: string;
  /** GiB per node; null or 0 means no limit. */
  cacheMaxGiB: number | null;
  scope: NodeScope;
  /** The storage applies to every node. Only chosen in the "mixed" scope; the other scopes imply it. */
  allNodes: boolean;
  labels: LabelTarget[];
  nodes: NodeOverride[];
}

/** Error keys (under storage:form.error.*) by field. */
export type StorageFormErrors = Partial<Record<"name" | "path" | "export" | "cachePath" | "cacheMax" | "targets" | "overrides", string>>;

const GIB = 1024 ** 3;
const namePattern = /^[a-z0-9][a-z0-9-]{0,62}$/;
const exportPattern = /^[A-Za-z0-9._[\]:-]+:\/\S*$/;

export function emptyForm(type: StorageType = "local"): StorageFormValues {
  return { name: "", type, path: "", export: "", mountOptions: "", cachePath: "", cacheMaxGiB: null, scope: "all", allNodes: true, labels: [{ key: "", value: "" }], nodes: [] };
}

function scopeOf(allNodes: boolean, labels: number, nodes: number): NodeScope {
  if (labels > 0 && (nodes > 0 || allNodes)) return "mixed";
  if (allNodes) return "all";
  return labels > 0 ? "labels" : "nodes";
}

export function formFromStorage(storage: Storage): StorageFormValues {
  const labels = storage.targets.filter((t) => t.label_key).map((t) => ({ key: t.label_key ?? "", value: t.label_value ?? "" }));
  const nodes = storage.targets.filter((t) => t.node_uuid).map((t) => ({
    node_uuid: t.node_uuid ?? "", path: t.path ?? "", cache_path: t.cache_path ?? "", existing: !!t.existing,
    ...(t.override_only ? { override_only: true } : {}),
    ...(t.cache_max_bytes != null ? { cache_max_bytes: t.cache_max_bytes } : {}),
  }));
  const scope = scopeOf(storage.all_nodes, labels.length, nodes.filter((n) => !n.override_only).length);
  return {
    name: storage.name,
    type: storage.type as StorageType,
    path: storage.path,
    export: storage.export ?? "",
    mountOptions: storage.mount_options ?? "",
    cachePath: storage.cache_path ?? "",
    cacheMaxGiB: storage.cache_max_bytes ? storage.cache_max_bytes / GIB : null,
    scope,
    allNodes: storage.all_nodes,
    labels: labels.length > 0 ? labels : [{ key: "", value: "" }],
    nodes,
  };
}

export function validCacheBytes(value: number | undefined): boolean {
  return value === undefined || (Number.isSafeInteger(value) && value >= 0);
}

function isAbsolute(path: string): boolean {
  return path.startsWith("/");
}

export function validateForm(values: StorageFormValues, mode: "create" | "edit"): StorageFormErrors {
  const errors: StorageFormErrors = {};
  if (!namePattern.test(values.name.trim())) errors.name = "name";
  const path = values.path.trim();
  if (values.type === "local" && !path) errors.path = "pathRequired";
  else if (values.type === "shared_dir" && !path) errors.path = "pathRequired";
  else if (path && !isAbsolute(path)) errors.path = "pathAbsolute";
  if (storageTypeInfo(values.type).nfs && mode === "create" && !exportPattern.test(values.export.trim())) errors.export = "export";
  const cachePath = values.cachePath.trim();
  if (cachePath && !isAbsolute(cachePath)) errors.cachePath = "pathAbsolute";
  if (storageTypeInfo(values.type).cache) {
    if (values.cacheMaxGiB != null && !validCacheBytes(values.cacheMaxGiB * GIB)) errors.cacheMax = "cacheLimit";
    if (values.nodes.some((n) => !validCacheBytes(n.cache_max_bytes))) errors.overrides = "cacheLimit";
  }
  const selected = values.nodes.filter((n) => !n.override_only);
  if (values.scope === "labels" && !values.labels.some((l) => l.key.trim())) errors.targets = "labelsRequired";
  if (values.scope === "nodes" && selected.length === 0) errors.targets = "nodesRequired";
  if (values.scope === "mixed" && !values.allNodes && selected.length === 0 && !values.labels.some((l) => l.key.trim())) errors.targets = "nodesRequired";
  if (values.nodes.some((n) => (n.path.trim() && !isAbsolute(n.path.trim())) || (n.cache_path.trim() && !isAbsolute(n.cache_path.trim())))) {
    errors.overrides = "pathAbsolute";
  }
  return errors;
}

/** Whether the request says the storage applies to every node. */
export function appliesToAllNodes(values: StorageFormValues): boolean {
  return values.scope === "all" || (values.scope === "mixed" && values.allNodes);
}

export function buildTargets(values: StorageFormValues): StorageTarget[] {
  const labelTargets = (): StorageTarget[] =>
    values.labels.filter((l) => l.key.trim()).map((l) => ({ label_key: l.key.trim(), label_value: l.value.trim() }));
  const nodeTarget = (n: NodeOverride): StorageTarget => {
    const target: StorageTarget = { node_uuid: n.node_uuid };
    if (n.override_only) target.override_only = true;
    if (n.path.trim()) target.path = n.path.trim();
    if (storageTypeInfo(values.type).cache && n.cache_path.trim()) target.cache_path = n.cache_path.trim();
    if (n.existing && storageTypeInfo(values.type).nfs) target.existing = true;
    if (n.cache_max_bytes != null) target.cache_max_bytes = n.cache_max_bytes;
    return target;
  };
  // Node rows are kept as they are in every scope that has them: with every
  // node in scope they carry overrides (a node whose directory differs from the
  // storage's), and dropping any would move that node's model files.
  if (values.scope === "mixed" || values.scope === "labels") return [...labelTargets(), ...values.nodes.map(nodeTarget)];
  return values.nodes.map(nodeTarget);
}

function cacheBytes(values: StorageFormValues): number {
  return values.cacheMaxGiB && values.cacheMaxGiB > 0 ? Math.round(values.cacheMaxGiB * GIB) : 0;
}

/** The body of POST /storages. Only the fields that belong to the chosen type are sent. */
export function buildCreateRequest(values: StorageFormValues, probeNode?: string): StorageRequest {
  const req: StorageRequest = {
    name: values.name.trim(),
    type: values.type,
    all_nodes: appliesToAllNodes(values),
    targets: buildTargets(values),
  };
  if (values.path.trim()) req.path = values.path.trim();
  if (storageTypeInfo(values.type).nfs) {
    req.export = values.export.trim();
    if (values.mountOptions.trim()) req.mount_options = values.mountOptions.trim();
  }
  if (storageTypeInfo(values.type).cache) {
    if (values.cachePath.trim()) req.cache_path = values.cachePath.trim();
    req.cache_max_bytes = cacheBytes(values);
  }
  if (probeNode) req.probe_node = probeNode;
  return req;
}

/** The body of PATCH /storages/{id}: the editable settings, with the type left to the card's switch. */
export function buildPatchRequest(values: StorageFormValues, baseline?: StorageFormValues): StorageRequest {
  const req: StorageRequest = {
    name: values.name.trim(),
    path: values.path.trim(),
    all_nodes: appliesToAllNodes(values),
    targets: buildTargets(values),
  };
  if (storageTypeInfo(values.type).nfs) req.mount_options = values.mountOptions.trim();
  if (storageTypeInfo(values.type).cache) {
    req.cache_path = values.cachePath.trim();
    req.cache_max_bytes = cacheBytes(values);
  }
  if (baseline) {
    const previous = buildPatchRequest(baseline);
    for (const key of Object.keys(req) as Array<keyof StorageRequest>) {
      if (JSON.stringify(req[key]) === JSON.stringify(previous[key])) delete req[key];
    }
  }
  return req;
}

/** The node a trial mount runs on: the first chosen node when nodes are listed one by one. */
export function probeNodeOf(values: StorageFormValues): string | undefined {
  return values.scope === "nodes" || values.scope === "mixed" ? values.nodes.find((n) => !n.override_only)?.node_uuid : undefined;
}

export function buildProbeRequest(values: StorageFormValues): StorageProbeRequest {
  const node = probeNodeOf(values);
  const override = values.scope === "nodes" || values.scope === "mixed" ? values.nodes.find((n) => !n.override_only) : undefined;
  // A shared storage without a path is tried where the server would mount it:
  // the server knows the default mount point, the form only names the storage.
  const path = override?.path.trim() || values.path.trim();
  const req: StorageProbeRequest = { type: values.type, path };
  if (node) req.node_uuid = node;
  if (!path && storageTypeInfo(values.type).shared && values.name.trim()) req.name = values.name.trim();
  if (storageTypeInfo(values.type).nfs) {
    req.export = values.export.trim();
    if (values.mountOptions.trim()) req.mount_options = values.mountOptions.trim();
    if (override?.existing) req.existing = true;
  }
  return req;
}

/** What a trial mount depends on; a successful probe is only good for the same signature. */
export function probeSignature(values: StorageFormValues): string {
  return JSON.stringify(buildProbeRequest(values));
}
