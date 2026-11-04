// A stateful stand-in for the pull-settings API (source rules, sync groups,
// registry credentials) and for GET /nodes/facets, faithful to what the
// server does: it refuses empty and malformed input with the same product
// error (PULL_SETTING_INVALID {field, detail}), numbers rules and proxy
// entries by position, derives a rule's kind from its match, refuses
// control_plane on an image rule and a login inside a proxy address, keeps the
// stored proxy password unless the request says otherwise (omitted keeps, ""
// removes), picks a node's proxy as the first entry it meets,
// accepts a reorder only when it names every rule exactly once, never returns
// a secret, keeps the stored secret when an update sends none, and answers a
// login test with the two failures the server distinguishes.
import { APIError } from "../../src/apiError.ts";
import type {
  NodeFacets,
  NodePullProxy,
  PullProxy,
  PullProxyRequest,
  PullSourceRule,
  PullSourceRuleRequest,
  Credential,
  CredentialRequest,
  SourceProbeOutcome,
  PullSourceRuleTestRequest as SourceRuleTestRequest,
  SourceRuleTestResult,
  SyncGroup,
  SyncGroupRequest,
} from "../../src/api.ts";

export interface FakeNode { name: string; labels: Record<string, string> }

export const FLEET: FakeNode[] = [
  ...[1, 2, 3].map((i) => ({ name: `gpu-a-0${i}`, labels: { site: "idc-a", "gpuworks.ai/provider": "onprem" } })),
  ...[1, 2].map((i) => ({ name: `gpu-b-0${i}`, labels: { site: "idc-b", "gpuworks.ai/provider": "onprem" } })),
  { name: "runpod-7", labels: { site: "cloud", "gpuworks.ai/provider": "runpod" } },
];

type Requirements = Record<string, string[]>;

export function invalid(field: string, detail: string, status = 400, reason?: string, params: Record<string, unknown> = {}): APIError {
  return new APIError("PULL_SETTING_INVALID", { field, detail, ...(reason ? { reason } : {}), ...params }, `${field}: ${detail}`, status);
}

export function matches(node: FakeNode, requirements: Requirements): boolean {
  return Object.entries(requirements).every(([key, values]) => key in node.labels && values.includes(node.labels[key]));
}

export function facetsFor(fleet: FakeNode[], labelPairs: string[] = []): NodeFacets {
  const requirements: Requirements = {};
  for (const pair of labelPairs) {
    const split = pair.indexOf("=");
    (requirements[pair.slice(0, split)] ??= []).push(pair.slice(split + 1));
  }
  const matched = fleet.filter((node) => matches(node, requirements));
  const collect = (system: boolean) => {
    const byKey = new Map<string, Map<string, number>>();
    for (const node of matched) {
      for (const [key, value] of Object.entries(node.labels)) {
        if (key.startsWith("gpuworks.ai/") !== system) continue;
        const values = byKey.get(key) ?? new Map<string, number>();
        values.set(value, (values.get(value) ?? 0) + 1);
        byKey.set(key, values);
      }
    }
    return [...byKey.entries()].map(([key, values]) => ({ key, values: [...values.entries()].map(([value, nodes]) => ({ value, nodes })) }));
  };
  return {
    total: matched.length,
    labels: collect(false),
    system_labels: collect(true).map(({ key, values }) => ({ key, values: values.map((item) => ({ ...item, display: item.value })) })),
    gpus: [],
  };
}

export interface PullFake {
  rules: PullSourceRule[];
  groups: SyncGroup[];
  proxies: (PullProxy & { password: string })[];
  credentials: (Credential & { secret: string })[];
  calls: { method: string; args: unknown[] }[];
  /**
   * What a node answers when asked about an address (default: usable in 120 ms).
   * Return nothing to make the whole node fail with `probeNodeError`.
   */
  probe: (node: { uuid: string; name: string }, base: string) => SourceProbeOutcome | undefined;
  probeNodeError: string;
  /** Makes the next call of the named api method fail with the error. */
  failNext(method: string, error: Error): void;
  restore(): void;
}

export function installPullFake(
  api: Record<string, unknown>,
  initial: {
    rules?: (Omit<PullSourceRule, "position" | "matched_nodes" | "kind" | "control_plane"> & Partial<Pick<PullSourceRule, "kind" | "control_plane">>)[];
    proxies?: (Omit<PullProxy, "position" | "matched_nodes" | "has_password"> & { password?: string })[]; groups?: Omit<SyncGroup, "matched_nodes">[]; credentials?: (Credential & { secret?: string })[]; fleet?: FakeNode[] } = {},
): PullFake {
  const fleet = initial.fleet ?? FLEET;
  const count = (requirements: Requirements) => fleet.filter((node) => matches(node, requirements)).length;
  let nextId = 100;
  const fake: PullFake = {
    rules: [],
    groups: [],
    proxies: [],
    credentials: [],
    calls: [],
    probe: (_node, base) => ({ base, status: "ok", code: 200, latency_ms: 120 }),
    probeNodeError: "unknown rpc method source.probe",
    failNext: (method, error) => { failures.set(method, error); },
    restore: () => { for (const [name, fn] of saved) api[name] = fn; },
  };
  const failures = new Map<string, Error>();
  const saved = new Map<string, unknown>();
  const renumber = () => fake.rules.forEach((rule, i) => { rule.position = i + 1; rule.matched_nodes = count(rule.node_requirements); });
  const kindOf = (match: string): PullSourceRule["kind"] => (/^https?:\/\//.test(match) ? "url" : "image");
  fake.rules = (initial.rules ?? []).map((rule, i) => ({ control_plane: false, kind: kindOf(rule.match), ...rule, position: i + 1, matched_nodes: count(rule.node_requirements) }));
  fake.groups = (initial.groups ?? []).map((group) => ({ ...group, matched_nodes: count(group.node_requirements) }));
  fake.credentials = (initial.credentials ?? []).map((credential) => ({ ...credential, secret: credential.secret ?? (credential.has_secret ? "stored-secret" : "") }));

  const define = (name: string, fn: (...args: any[]) => unknown) => {
    saved.set(name, api[name]);
    api[name] = async (...args: unknown[]) => {
      fake.calls.push({ method: name, args });
      const failure = failures.get(name);
      if (failure) { failures.delete(name); throw failure; }
      return fn(...args);
    };
  };
  const checkRule = (body: PullSourceRuleRequest) => {
    if (typeof body.allow_upstream !== "boolean") throw invalid("allow_upstream", "allow_upstream is required");
    if (!body.match.trim()) throw invalid("match", "match must not be empty");
    if (body.control_plane && kindOf(body.match.trim()) === "image") throw invalid("control_plane", "control_plane applies to model site rules only");
    for (const endpoint of body.endpoints) if (/\s/.test(endpoint)) throw invalid("endpoints", `endpoint ${JSON.stringify(endpoint)} must not contain spaces`);
    if (body.endpoints.length === 0 && !body.allow_upstream) throw invalid("endpoints", "a rule with no endpoint must allow upstream");
  };
  const ruleFrom = (id: number, body: PullSourceRuleRequest): PullSourceRule => ({
    id, position: 0, kind: kindOf(body.match), control_plane: Boolean(body.control_plane), match: body.match, endpoints: body.endpoints, allow_upstream: body.allow_upstream,
    note: body.note, node_requirements: body.node_requirements, matched_nodes: count(body.node_requirements),
  });

  define("pullSourceRules", () => ({ items: fake.rules.map((rule) => ({ ...rule })), total: fake.rules.length }));
  define("createPullSourceRule", (body: PullSourceRuleRequest) => {
    checkRule(body);
    const rule = ruleFrom(++nextId, body);
    fake.rules.push(rule); renumber();
    return { ...rule };
  });
  define("updatePullSourceRule", (id: number, body: PullSourceRuleRequest) => {
    checkRule(body);
    const index = fake.rules.findIndex((rule) => rule.id === id);
    if (index < 0) throw new Error("not found");
    fake.rules[index] = ruleFrom(id, body); renumber();
    return { ...fake.rules[index] };
  });
  define("deletePullSourceRule", (id: number) => {
    fake.rules = fake.rules.filter((rule) => rule.id !== id); renumber();
  });
  define("orderPullSourceRules", (ids: number[]) => {
    const known = fake.rules.map((rule) => rule.id).sort();
    if (JSON.stringify([...ids].sort()) !== JSON.stringify(known)) throw invalid("order", "ids must name every rule exactly once");
    fake.rules = ids.map((id) => fake.rules.find((rule) => rule.id === id)!); renumber();
    return { items: fake.rules.map((rule) => ({ ...rule })), total: fake.rules.length };
  });

  const renumberProxies = () => fake.proxies.forEach((proxy, i) => { proxy.position = i; proxy.matched_nodes = count(proxy.node_requirements); });
  const publicProxy = ({ password, ...rest }: PullProxy & { password: string }): PullProxy => ({ ...rest, has_password: password !== "" });
  const checkProxy = (body: PullProxyRequest) => {
    if (!body.http_proxy.trim() && !body.https_proxy.trim()) throw invalid("http_proxy", "at least one of http_proxy and https_proxy is required");
    if (/@/.test(body.http_proxy)) throw invalid("http_proxy", "the address must not contain a login; use username and password");
    if (/@/.test(body.https_proxy)) throw invalid("https_proxy", "the address must not contain a login; use username and password");
    if (body.password && !body.username.trim()) throw invalid("username", "a password needs a user name");
  };
  const proxyFrom = (id: number, body: PullProxyRequest, password: string): PullProxy & { password: string } => ({
    id, position: 0, http_proxy: body.http_proxy, https_proxy: body.https_proxy, no_proxy: body.no_proxy, username: body.username,
    has_password: password !== "", password, control_plane: body.control_plane, note: body.note,
    node_requirements: body.node_requirements, matched_nodes: count(body.node_requirements),
  });
  fake.proxies = (initial.proxies ?? []).map((proxy, i) => ({ ...proxy, password: proxy.password ?? "", has_password: Boolean(proxy.password), position: i, matched_nodes: count(proxy.node_requirements) }));
  define("pullProxies", () => ({ items: fake.proxies.map(publicProxy), total: fake.proxies.length }));
  define("createPullProxy", (body: PullProxyRequest) => {
    checkProxy(body);
    const proxy = proxyFrom(++nextId, body, body.password ?? "");
    fake.proxies.push(proxy); renumberProxies();
    return publicProxy(proxy);
  });
  define("updatePullProxy", (id: number, body: PullProxyRequest) => {
    const index = fake.proxies.findIndex((proxy) => proxy.id === id);
    if (index < 0) throw new Error("not found");
    checkProxy(body);
    fake.proxies[index] = proxyFrom(id, body, body.password === undefined ? fake.proxies[index].password : body.password); renumberProxies();
    return publicProxy(fake.proxies[index]);
  });
  define("deletePullProxy", (id: number) => { fake.proxies = fake.proxies.filter((proxy) => proxy.id !== id); renumberProxies(); });
  define("orderPullProxies", (ids: number[]) => {
    const known = fake.proxies.map((proxy) => proxy.id).sort();
    if (JSON.stringify([...ids].sort()) !== JSON.stringify(known)) throw invalid("order", "ids must name every proxy exactly once");
    fake.proxies = ids.map((id) => fake.proxies.find((proxy) => proxy.id === id)!); renumberProxies();
    return { items: fake.proxies.map(publicProxy), total: fake.proxies.length };
  });
  define("nodePullProxy", (uuid: string): NodePullProxy => {
    const node = fleet.find((item) => item.name === uuid);
    if (!node) throw new Error("node not found");
    const entry = fake.proxies.find((proxy) => matches(node, proxy.node_requirements));
    return { proxy: entry ? publicProxy(entry) : null, runtime: { driver: "docker" } };
  });

  const checkGroup = (body: SyncGroupRequest, exceptID?: number) => {
    if (/[\u0000-\u001f\u007f-\u009f]/.test(body.name)) throw invalid("name", "name must contain valid text without control characters", 400, "invalid_text");
    const name = body.name.trim();
    if (!name) throw invalid("name", "a sync group needs a name", 400, "name_required");
    if (new TextEncoder().encode(name).length > 64) throw invalid("name", "name is longer than 64 bytes", 400, "max_bytes", { max: 64 });
    if (body.rate_limit_kib_s < 0) throw invalid("rate_limit", "rate limit must not be negative", 400, "nonnegative");
    if (fake.groups.some((group) => group.id !== exceptID && group.name === name)) throw invalid("name", "a sync group with this name already exists", 400, "name_duplicate");
  };
  define("syncGroups", () => ({ items: fake.groups.map((group) => ({ ...group, matched_nodes: count(group.node_requirements) })), total: fake.groups.length }));
  define("createSyncGroup", (body: SyncGroupRequest) => {
    checkGroup(body);
    const group: SyncGroup = { id: ++nextId, name: body.name.trim(), node_requirements: body.node_requirements, rate_limit_kib_s: body.rate_limit_kib_s, matched_nodes: count(body.node_requirements) };
    fake.groups.push(group);
    return { ...group };
  });
  define("updateSyncGroup", (id: number, body: SyncGroupRequest) => {
    const index = fake.groups.findIndex((group) => group.id === id);
    if (index < 0) throw new Error("not found");
    checkGroup(body, id);
    fake.groups[index] = { ...fake.groups[index], name: body.name.trim(), node_requirements: body.node_requirements, rate_limit_kib_s: body.rate_limit_kib_s };
    return { ...fake.groups[index] };
  });
  define("deleteSyncGroup", (id: number) => { fake.groups = fake.groups.filter((group) => group.id !== id); });

  const publicCredential = ({ secret, ...rest }: Credential & { secret: string }): Credential => ({ ...rest, has_secret: secret !== "" });
  define("credentials", () => ({ items: fake.credentials.map(publicCredential), total: fake.credentials.length }));
  define("createCredential", (body: CredentialRequest) => {
    if (!body.host.trim()) throw invalid("host", "host must not be empty");
    if (!body.secret) throw invalid("secret", "secret must not be empty");
    if (fake.credentials.some((item) => item.host === body.host)) throw invalid("host", "a credential for this host already exists");
    const credential = { id: ++nextId, host: body.host, username: body.username, has_secret: true, secret: body.secret };
    fake.credentials.push(credential);
    return publicCredential(credential);
  });
  define("updateCredential", (id: number, body: CredentialRequest) => {
    const index = fake.credentials.findIndex((item) => item.id === id);
    if (index < 0) throw new Error("not found");
    if (!body.host.trim()) throw invalid("host", "host must not be empty");
    fake.credentials[index] = { ...fake.credentials[index], host: body.host, username: body.username, secret: body.secret || fake.credentials[index].secret };
    return publicCredential(fake.credentials[index]);
  });
  define("deleteCredential", (id: number) => { fake.credentials = fake.credentials.filter((item) => item.id !== id); });
  define("testCredential", (id: number) => {
    const credential = fake.credentials.find((item) => item.id === id);
    if (!credential) throw new Error("not found");
    if (credential.host.startsWith("bad.")) throw new APIError("CREDENTIAL_LOGIN_REJECTED", { host: credential.host }, "login rejected", 422);
    if (credential.host.startsWith("down.")) throw new APIError("CREDENTIAL_LOGIN_FAILED", { host: credential.host, detail: "dial tcp: i/o timeout" }, "login failed", 502);
    return { ok: true };
  });
  // POST /pull/source-rules/test as the server answers it: the draft is judged
  // like a saved rule (site rules only), 1-5 nodes are required, the repository
  // is organization/name (default Qwen/Qwen3-0.6B), an offline node carries only
  // an error, and the addresses come back in the order a download tries them
  // (the endpoints, then the site itself unless upstream is off).
  define("testPullSourceRule", async (body: SourceRuleTestRequest): Promise<SourceRuleTestResult> => {
    const match = body.match.trim();
    if (!/^https?:\/\//.test(match)) throw invalid("match", "only a model site rule can be tested");
    for (const endpoint of body.endpoints) if (/\s/.test(endpoint)) throw invalid("endpoints", `endpoint ${JSON.stringify(endpoint)} must not contain spaces`);
    const bases = [...body.endpoints.map((endpoint) => endpoint.replace(/\/+$/, "")), ...(body.allow_upstream ? [match.replace(/\/+$/, "")] : [])];
    if (bases.length === 0) throw invalid("endpoints", "a rule with no endpoint must allow upstream");
    const repo = (body.repo ?? "").trim().replace(/^\/+|\/+$/g, "") || "Qwen/Qwen3-0.6B";
    if (repo.split("/").length !== 2 || /[ ?#]/.test(repo)) throw new APIError("FIELD_INVALID", { field: "repo", detail: "repo is an organization/name repository id" }, "repo is an organization/name repository id", 400);
    if (!body.node_uuid?.trim()) throw new APIError("FIELD_INVALID", { field: "node_uuid", detail: "node_uuid is required" }, "node_uuid is required", 400);
    // As the server: an unknown node is a 404, an offline one 409 NODE_OFFLINE,
    // a node that cannot run the probe 502 SOURCE_PROBE_FAILED.
    const node = await (api.node as (uuid: string) => Promise<{ uuid: string; name: string; state: string }>)(body.node_uuid);
    if (node.state !== "online") throw new APIError("NODE_OFFLINE", undefined, "node is offline", 409);
    const answers = bases.map((base) => fake.probe({ uuid: node.uuid, name: node.name }, base));
    if (answers.some((answer) => answer === undefined)) {
      throw new APIError("SOURCE_PROBE_FAILED", { detail: fake.probeNodeError }, `the node could not run the test (an agent older than this server cannot): ${fake.probeNodeError}`, 502);
    }
    return { source: match.includes("modelscope.cn") ? "modelscope" : "huggingface", repo, node_uuid: node.uuid, node_name: node.name, outcomes: answers as SourceProbeOutcome[] };
  });
  define("nodeFacets", (filter: { label?: string[] } = {}) => facetsFor(fleet, filter.label));
  return fake;
}
