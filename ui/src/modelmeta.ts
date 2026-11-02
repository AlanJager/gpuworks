const README_LIMIT = 512 * 1024;
const CONFIG_LIMIT = 4 * 1024 * 1024;
const REQUEST_TIMEOUT = 8_000;

export interface BrowserModelMetadata {
  /** The commit the repository's default branch pointed at, when the site reported one. */
  sha?: string;
  readme: string;
  config_raw: string;
  weight_bytes: number;
  logo_data: string;
}

// The addresses a browser may read a source's metadata at are the management
// node's candidates (the site rules chosen for it), never a list of our own.
export type LoadMetadataBases = () => Promise<string[]>;

// api is loaded on use: this module is also run where the console shell (i18n, DOM) is absent.
const loadServerBases: LoadMetadataBases = async () => (await (await import("./api")).api.metadataSources("huggingface")).bases;

// Pages served over https cannot read plain http addresses (mixed content).
export function usableBases(bases: string[], pageProtocol = typeof location === "undefined" ? "" : location.protocol): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const raw of bases) {
    const base = raw.trim().replace(/\/+$/, "");
    if (!/^https?:\/\//i.test(base) || seen.has(base)) continue;
    if (pageProtocol === "https:" && /^http:/i.test(base)) continue;
    seen.add(base);
    out.push(base);
  }
  return out;
}

function repoPath(repo: string): string {
  return repo
    .trim()
    .split("/")
    .filter(Boolean)
    .map((part) => encodeURIComponent(part))
    .join("/");
}

function sourceURL(base: string, repo: string, revision: string, file: string): string {
  return `${base}/${repoPath(repo)}/raw/${encodeURIComponent(revision || "main")}/${file}`;
}

function fetchFirst(urls: string[], limit: number, fetcher: typeof fetch): Promise<string> {
  if (urls.length === 0) return Promise.reject(new Error("no Hugging Face metadata source is allowed"));
  const controllers = urls.map(() => new AbortController());
  return new Promise<string>((resolve, reject) => {
    let pending = controllers.length;
    let lastError: unknown = new Error("no Hugging Face metadata source is reachable");
    controllers.forEach((controller, index) => {
      const timer = setTimeout(() => controller.abort(), REQUEST_TIMEOUT);
      fetchText(urls[index], limit, fetcher, controller.signal)
        .then((value) => {
          clearTimeout(timer);
          controllers.forEach((other) => other.abort());
          resolve(value);
        })
        .catch((error: unknown) => {
          clearTimeout(timer);
          lastError = error;
          pending -= 1;
          if (pending === 0) reject(lastError);
        });
    });
  });
}

async function fetchText(url: string, limit: number, fetcher: typeof fetch, signal: AbortSignal): Promise<string> {
  const response = await fetcher(url, { signal, credentials: "omit" });
  if (!response.ok) throw new Error(`Hugging Face metadata: ${response.status}`);
  const body = await response.text();
  return body.length > limit ? body.slice(0, limit) : body;
}

// Try every candidate the server gave in parallel. The first reachable
// route wins, so a local browser proxy does not pay the mirror fallback delay.
async function fetchFromFirstReachable(sourceBases: string[], repo: string, revision: string, file: string, limit: number, fetcher: typeof fetch): Promise<string> {
  return fetchFirst(sourceBases.map((base) => sourceURL(base, repo, revision, file)), limit, fetcher);
}

function indexWeightBytes(raw: string): number {
  try {
    const value = (JSON.parse(raw) as { metadata?: { total_size?: unknown } }).metadata?.total_size;
    return typeof value === "number" && Number.isSafeInteger(value) && value > 0 ? value : 0;
  } catch {
    return 0;
  }
}

interface RepositoryMetadata {
  author?: unknown;
  sha?: unknown;
  siblings?: Array<{ rfilename?: unknown; size?: unknown }>;
}

function repositoryWeightBytes(metadata: RepositoryMetadata): number {
  let total = 0;
  for (const file of metadata.siblings ?? []) {
    if (typeof file.rfilename !== "string" || !/\.(?:safetensors|bin)$/i.test(file.rfilename)) continue;
    if (typeof file.size !== "number" || !Number.isSafeInteger(file.size) || file.size <= 0) continue;
    total += file.size;
    if (!Number.isSafeInteger(total)) return 0;
  }
  return total;
}

const LOGO_LIMIT = 2 * 1024 * 1024;

// The picture an avatar answer points at is read only from a site the server
// allowed, or one of its subdomains (the hub keeps uploads on cdn-avatars.*).
function allowedLogoURL(raw: unknown, sourceBases: string[]): string {
  if (typeof raw !== "string" || !raw.trim()) return "";
  let target: URL;
  try {
    target = new URL(raw.trim(), sourceBases[0]);
  } catch {
    return "";
  }
  // An account without an upload points at a generated gradient (.svg), as
  // /avatars/<name>.svg does for any name: neither is a logo.
  if (target.pathname.toLowerCase().endsWith(".svg")) return "";
  const allowed = sourceBases.some((base) => {
    const host = new URL(base).hostname;
    return target.protocol === new URL(base).protocol && (target.hostname === host || target.hostname.endsWith(`.${host}`));
  });
  return allowed ? target.href : "";
}

async function fetchDataURL(url: string, fetcher: typeof fetch): Promise<string> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), REQUEST_TIMEOUT);
  try {
    const response = await fetcher(url, { signal: controller.signal, credentials: "omit" });
    const type = (response.headers.get("content-type") || "").split(";")[0].trim().toLowerCase();
    if (!response.ok || !/^image\/(png|jpeg|webp|gif)$/.test(type)) return "";
    const bytes = new Uint8Array(await response.arrayBuffer());
    if (bytes.length === 0 || bytes.length > LOGO_LIMIT) return "";
    let binary = "";
    for (let i = 0; i < bytes.length; i += 0x8000) binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
    return `data:${type};base64,${btoa(binary)}`;
  } finally {
    clearTimeout(timer);
  }
}

// The author's uploaded picture, asked of the hub as an organization and then
// as a user.
async function fetchLogo(sourceBases: string[], author: unknown, fetcher: typeof fetch): Promise<string> {
  if (typeof author !== "string" || !author.trim()) return "";
  const name = encodeURIComponent(author.trim());
  for (const kind of ["organizations", "users"]) {
    const raw = await fetchFirst(sourceBases.map((base) => `${base}/api/${kind}/${name}/avatar`), CONFIG_LIMIT, fetcher).catch(() => "");
    if (!raw) continue;
    let avatarURL: unknown;
    try {
      avatarURL = (JSON.parse(raw) as { avatarUrl?: unknown }).avatarUrl;
    } catch {
      continue;
    }
    const url = allowedLogoURL(avatarURL, sourceBases);
    return url ? fetchDataURL(url, fetcher) : "";
  }
  return "";
}

export async function fetchHuggingFaceMetadata(
  repo: string,
  revision = "main",
  fetcher: typeof fetch = fetch,
  loadBases: LoadMetadataBases = loadServerBases,
): Promise<BrowserModelMetadata> {
  if (!repoPath(repo)) throw new Error("Hugging Face repository ID is required");
  // No candidates (the rules forbid the site, or the server cannot be asked):
  // the browser does not fetch, the server reads on its own.
  const sourceBases = usableBases(await loadBases());
  if (sourceBases.length === 0) throw new Error("no Hugging Face metadata source is available to the browser");
  const [readme, configRaw, indexRaw, repositoryRaw] = await Promise.all([
    fetchFromFirstReachable(sourceBases, repo, revision, "README.md", README_LIMIT, fetcher).catch(() => ""),
    fetchFromFirstReachable(sourceBases, repo, revision, "config.json", CONFIG_LIMIT, fetcher).catch(() => ""),
    fetchFromFirstReachable(sourceBases, repo, revision, "model.safetensors.index.json", CONFIG_LIMIT, fetcher).catch(() => ""),
    fetchFirst(sourceBases.map((base) => `${base}/api/models/${repoPath(repo)}?blobs=true`), CONFIG_LIMIT, fetcher).catch(() => ""),
  ]);
  let repository: RepositoryMetadata = {};
  try {
    repository = JSON.parse(repositoryRaw) as RepositoryMetadata;
  } catch {
    // Other metadata remains usable when the repository API returns malformed JSON.
  }
  const logoData = await fetchLogo(sourceBases, repository.author, fetcher).catch(() => "");
  // config.json is what sizing needs. Without it (a gated repository shows
  // its README to anyone and refuses its files to a browser without the
  // login) the browser has read nothing worth keeping: the server reads
  // instead, with the credentials and through the rules.
  if (!configRaw) throw new Error("The browser cannot access Hugging Face model metadata");
  const sha = typeof repository.sha === "string" && /^[0-9a-f]{40}$/i.test(repository.sha) ? repository.sha.toLowerCase() : "";
  return {
    ...(sha ? { sha } : {}),
    readme,
    config_raw: configRaw,
    weight_bytes: indexWeightBytes(indexRaw) || repositoryWeightBytes(repository),
    logo_data: logoData,
  };
}
