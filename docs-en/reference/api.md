# Complete REST API Routes

This document lists the HTTP endpoints for managing GPUWorks resources, including nodes, models, deployments, and settings. Management endpoints use the `/api/v1` prefix;
authentication and response conventions appear first, followed by endpoints organized by resource type. Replace `{id}`, `{uuid}`, and similar placeholders in paths with actual resource identifiers.

For your first call, see [Log In and Query the Inference Service List](api-conventions.md#示例登录并查询推理服务列表), which includes address, login, request, and response examples.

Model inference uses a separate set of endpoints: `/v1/*` is the external OpenAI-compatible gateway, and `/api/v1/playground/v1/*` is
the internal Playground proxy. See [OpenAI-Compatible Gateway](openai-gateway.md) for their calling conventions; they are not listed again here.

## Authentication Model

### Distinguish Credential Purposes First

Authentication first establishes who is making a request, then checks whether that identity can perform the operation. Choose credentials for your access purpose; management, node connection, and inference credentials are not interchangeable:

| Credential | Identity and usage | How to provide it |
|---|---|---|
| Management login session | A logged-in user, for management APIs and the console | Browsers use the `gpuworks_session` Cookie; programs take the `token` from the login response and send it in the `Authorization: Bearer <JWT>` request header |
| Platform API token | The token's owner, for programmatic management API access | `Authorization: Bearer gwp_…`; token scope further restricts permissions |
| Node token | A registered node, for agent connections | The hub handshake at `GET /api/v1/agent/connect` (a yamux connection is established after the WebSocket upgrade) |
| Inference API key | An inference caller, for the `/v1/*` gateway | See [OpenAI-Compatible Gateway](openai-gateway.md); not used to log in to management APIs |

The Cookie and JWT for a management login session are two credentials from the same login. Both must be associated with a session that is still valid on the server.
Missing or invalid credentials, or an expired or revoked session, return `401 AUTHENTICATION_REQUIRED`. See [Auth](#auth) below for login and expiration rules.

### The Authentication Column in Route Tables

| Label | Calling requirements |
|---|---|
| `JWT` | Any valid management identity; use a login session or a platform API token within its allowed scope |
| `JWT(admin)`, `admin` | In addition to management authentication, the current user must have the `admin` role; otherwise, returns `403 ADMINISTRATOR_REQUIRED` |
| Management session | Accepts only a Cookie or JWT obtained by logging in; does not accept platform API tokens |
| None | Does not validate management JWTs; the endpoint may still enforce constraints such as origin checks |

`JWT` is a historical abbreviation in these tables. It does not mean that only Bearer JWTs are accepted. Routes that use dedicated credentials, such as node connections, are labeled separately.

### Additional Restrictions on Platform API Tokens

Platform tokens operate with the owner's **current role**, further restricted by their own `read` or `write` scope.
For example, a regular user's `write` token cannot perform administrator-only operations. See [Platform Tokens](#platform-tokens平台-api-令牌) for creation and revocation.

| Condition | Behavior or error |
|---|---|
| Using a `read` token | The read-only scope check allows GET, HEAD, OPTIONS, and the read-only tool entry point `POST /api/v1/mcp`; other write requests return `403 PLATFORM_TOKEN_READ_ONLY`. Whether the endpoint supports the HTTP method still depends on the specific route |
| Using any platform token to access `/api/v1/platform-tokens` or `auth/activity` | Returns `403 PLATFORM_TOKEN_NOT_ALLOWED`; these operations require a login session |
| The token is unknown or expired, or its owner has been deleted | Returns `401 AUTHENTICATION_REQUIRED` |

### Public Installation and Health Check Endpoints

The following endpoints are outside `/api/v1` and publicly accessible:

| Path (all use GET) | Purpose |
|---|---|
| `/install.sh` | Agent installation script |
| `/healthz` | Liveness and load balancer health checks; the management endpoint `/api/v1/healthz` still requires authentication |
| `/dl/gpuworks-agent-<os>-<arch>` | Download the agent binary for the specified operating system and architecture |
| `/dl/gpuworks-agent.run` | Offline agent installation package |
| `/dl/gpuworks-agent.run.sha256` | Offline package checksum in `sha256sum` format |

Downloads are served from the node connection listener address. The offline package is a self-extracting sh script that includes the linux-amd64, linux-arm64, and darwin-arm64
agents and `install.sh`. It does not contain a node token. If the server has no embedded agents, the offline package returns 404.

## Error and Response Conventions

### JSON Fields and List Structure

JSON fields use snake_case, such as `node_uuid`, as described in [API Conventions](api-conventions.md).
When returning a collection of objects, elements appear in the `items` field rather than in a top-level array. `total` is the total number of matches after filtering:
without pagination, it equals the length of `items`; with pagination, it may exceed the number of items on the current page.

| List type | Response structure and meaning |
|---|---|
| Unpaginated | `{"items": [...], "total": N}`, returning all matching objects |
| Page-based pagination | Also includes `page` and `page_size`, the current page number and the requested number of items per page, respectively |
| Cursor-based pagination | Returns `{"items": [...]}`; the cursor marks the last position read, and the endpoint determines whether a total is returned |

### Reading Error Responses

Check the HTTP status first, then read `error` (the error description), `code` (a stable error code), and `params` (related values).
Ordinary errors may contain only `error`; do not assume that `code` or `params` is always present.
See [HTTP API Conventions: Errors](api-conventions.md#错误) for examples and general handling. The sections below list only endpoint-specific restrictions and errors.

### Nodes and Deployments: Pages, Filters, and Sorting

The following rules apply to `GET /api/v1/nodes` and `GET /api/v1/deployments`. Both require management authentication and have no request body.
Provide the following optional parameters in the URL query string:

| Parameter | Type | Default and usage |
|---|---|---|
| `page` | Integer | Page numbers start at 1; defaults to 1 when pagination is enabled and this parameter is omitted |
| `page_size` | Integer | 1–200 items per page; defaults to 50 when pagination is enabled |
| `q` | String | No name filtering when omitted; performs a case-insensitive substring match. See the corresponding resource below for matching fields |
| `state` | String | No state restriction when omitted; separate multiple values with commas to match any of them |
| `sort` | String | Newest first when omitted or empty; `sort=<column>` sorts in ascending order, and `sort=-<column>` sorts in descending order. See the corresponding resource below for available columns |

Including either `page` or `page_size` enables pagination, and the response contains `items`, `page`, `page_size`, and `total`.
When both parameters are omitted, all matching objects are returned for convenient scripting and programmatic access; filtering and sorting still apply.
Processing **filters all objects first, sorts them next, and then selects the current page**. Objects with equal sort values remain ordered newest first.

Invalid page numbers or page sizes return `400 LIST_PAGE_INVALID` rather than silently falling back to defaults.
Unknown filter values or sort columns return `400 LIST_FILTER_INVALID`.

`GET /api/v1/inference-templates` uses the same page validation and response structure but **always paginates**, defaulting to page 1 with 50 items per page.
It also has its own hardware and text filters. `GET /api/v1/models` and `GET /api/v1/gpus` also support pagination; see their route entries for parameters.

### Real-Time Event Streams and Response Compression

`GET /api/v1/events` and `GET /api/v1/instances/{uuid}/logs` use SSE, a continuous stream of server-sent events,
with `Content-Type: text/event-stream`. Do not read them as a single JSON response.
`/api/v1/events` only indicates which resources have changed; it does not carry resource objects. After receiving a notification, clients fetch the corresponding resources again.
Every agent reports its status once a minute, which arrives as a `node` event with `reason` `status`; one stream forwards at most one such event every 5 seconds:
the first in a window goes out at once and the rest collapse into one event, with an empty `key`, at the window's end. Other node changes (going online or offline, scheduling, deletion) are never coalesced.

For regular management resource endpoints that return `Content-Type: application/json`, send `Accept-Encoding: gzip` to enable gzip compression.
Event streams, log streams, file transfers, and image transfers remain unchanged. WebSocket upgrades are unaffected.

## Auth

| Method | Path | Description | Authentication |
|---|---|---|---|
| POST | `/api/v1/auth/login` | Log in with a username and password and create a persistent session; returns a session-bound JWT and sets an opaque Cookie | None |
| POST | `/api/v1/auth/logout` | Revoke the session associated with the submitted credential and clear the Cookie; repeated logout is idempotent | None |
| GET | `/api/v1/auth/me` | Return the current user and live permissions; does not extend the idle timeout | Management session or platform API token |
| POST | `/api/v1/auth/activity` | Report real activity and extend the idle timeout; no request body; returns 204 on success | Management session |

### Login Request and Response

`POST /api/v1/auth/login` requires no existing login credentials. Send a JSON request body with `Content-Type: application/json`.
There are no path or query parameters.

| Request field | Type and requirement | Meaning and constraints |
|---|---|---|
| `username` | String, required | Username; leading and trailing whitespace is trimmed for lookup; up to 256 characters; must not contain control characters |
| `password` | String, required | The user's password, validated as provided without trimming whitespace |

The request body is limited to 1 MiB and must be a single JSON object, not multiple concatenated JSON values.
Success returns HTTP 200 and sets `gpuworks_session` through `Set-Cookie`. Example response structure:

```json
{
  "token": "<JWT>",
  "user": {"id": 1, "username": "admin", "role": "admin"}
}
```

`token` is the JWT for subsequent requests; `user` contains the user ID, username, and role (`admin` or `user`).
For a complete request example, see [Set the Address and Log In](api-conventions.md#1-设置地址并登录).

`auth/me` returns current user information in the form `{"id":1,"username":"admin","role":"admin"}`.
The Cookie is set to `HttpOnly` and `SameSite=Lax` and stores an opaque session credential, not a JWT.
When a request includes both Bearer and Cookie credentials, Bearer takes precedence; an invalid Bearer does not fall back to the Cookie.

An incorrect username or password returns `401 INVALID_CREDENTIALS`; overly frequent login attempts return `429 LOGIN_THROTTLED`.
Wait for the number of seconds specified by the `Retry-After` response header before retrying. For other errors, see [Common HTTP Statuses and Error Codes](api-conventions.md#常见-http-状态与错误码).

### When Sessions Expire

By default, a session expires after 24 hours without real activity and lasts no more than 7 days from login. The Cookie expiration uses the latter absolute deadline.
Both time limits are configurable; see [Management Session Configuration](configuration.md#管理会话). The JWT in the login response has its own 24-hour validity period
and must also be associated with a valid session. An unexpired JWT alone does not mean the login is still valid.

| Situation | Result |
|---|---|
| Calling `auth/me` or passive polling | Does not extend the idle timeout |
| Performing management write requests through a login session, or calling `auth/activity` to report real activity | Extends the idle timeout but not the absolute deadline measured from login |
| Password reset | Revokes the user's management sessions and invalidates associated JWTs; does not revoke platform API tokens |
| Logout | Revokes the session associated with the submitted credential, invalidates associated JWTs, and clears the Cookie; repeated logout still succeeds |
| A JWT not bound to a session before the upgrade, or an old Cookie | Requires logging in again |

Platform API tokens authenticate independently according to their own scope and validity period; `auth/activity` does not extend them.

### Login Endpoint Errors and Origin Checks

An invalid session returns `401 AUTHENTICATION_REQUIRED`; unavailable session storage returns 503.
Login, logout, and management write requests using login sessions check the request origin; cross-origin requests return `403 MANAGEMENT_ORIGIN_REJECTED`.

## Nodes (Node Connection)

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/agent/connect` | WS upgrade entry point for persistent agent connections | Node token (not a management JWT) |
| GET | `/install.sh` | curl\|sh installation script | None |
| GET | `/api/v1/healthz` | Health check (control plane) | JWT |
| GET | `/healthz` | Health check (no login required, for liveness/load balancing) | None |
| GET | `/api/v1/engines` | Engine parameter dictionary (for frontend form rendering) | JWT |
| GET | `/api/v1/nodes` | List nodes; paginate with `page`/`page_size` and filter by `q`/`state`/`label` (see “Error and Response Conventions”) | JWT |
| GET | `/api/v1/nodes/facets` | Filter options and node count distributions; the `q`, `state`, and `label` query parameters are the same as for the node list. Returns distributions of user labels, system labels, and GPU models; see [Node List](#节点列表的筛选与返回字段) | JWT |
| GET | `/api/v1/dashboard/summary` | Dashboard summary: node counts by state and hardware totals, deployment counts by state, and capacity grouped by vendor and label (`label_key`; uses the most common key when omitted or when no node has that key) | JWT |
| POST | `/api/v1/nodes` | Create a node awaiting connection and return the installation command and token; see [Connection Parameters for Node Creation](#创建节点的连接参数) for all fields and conditions | JWT(admin) |
| GET | `/api/v1/agent/rejects` | Recent rejected connection attempts | JWT |
| GET | `/api/v1/agent/upgrade-errors` | Recent agent upgrade failures | JWT |
| POST | `/api/v1/nodes/precheck` | Check the host environment and bidirectional connectivity required for SSH connection; see [SSH Checks and Installation](#ssh-检查与安装) for address rules | JWT(admin) |
| POST | `/api/v1/nodes/install` | Install the agent through SSH; connection parameters are the same as for node creation. Use `no_systemd` to select the Linux background execution mode. See [SSH Checks and Installation](#ssh-检查与安装) for conflicts and replacement conditions | JWT(admin) |
| GET | `/api/v1/nodes/{uuid}` | Node details (including the GPU list; also includes the token for administrators) | JWT |
| GET | `/api/v1/nodes/{uuid}/fs` | Browse the node filesystem (for registering local models) | JWT |
| POST | `/api/v1/nodes/{uuid}/doctor` | Trigger the node self-check RPC | JWT |
| POST | `/api/v1/nodes/{uuid}/upgrade` | Trigger a remote agent upgrade | JWT |
| PUT | `/api/v1/nodes/{uuid}/scheduling` | Set `scheduling` to `cordoned`, `draining`, or `schedulable` and return the node; invalid values return 400 `FIELD_INVALID`. See [Node Scheduling and Accelerator Switching](#节点调度与加速卡切换) for the effects of each state | JWT |
| GET | `/api/v1/nodes/{uuid}/replicas` | Instances still running on the node and how eviction handles them: `{"scheduling", "items": [{"instance", "deployment_id", "deployment", "state", "kind"}]}`. `kind` is `pool` (moves to another node), `pinned` (the deployment is pinned only to this node and waits), or `named` (a multi-node or PD rank: a drain starts a replacement on another node first and switches over once it runs, waiting if there is no node to go to; only forced eviction removes it immediately); stopped and failed instances are excluded | JWT |
| POST | `/api/v1/nodes/{uuid}/evict` | Force eviction: immediately remove every replica on the node without waiting for replacements (the replica's leader drains, followed by its followers). Replicas are recreated wherever placement is possible, without counting toward retries; returns `{"evicted": [leader UUID…]}` (202). Returns 409 `NODE_SCHEDULABLE` if the node is still schedulable | JWT |
| PUT | `/api/v1/nodes/{uuid}/labels` | Replace node labels (key/value; an empty object clears them). Keys must not use the platform-reserved `gpuworks.ai/` namespace or its subdomains | JWT |
| PATCH | `/api/v1/nodes/{uuid}/name` | Rename the node | JWT |
| PATCH | `/api/v1/nodes/{uuid}/accelerator` | Select the accelerator vendor managed on the node: `{"accelerator":"auto"}`; also accepts `nvidia`, `ascend`, `hygon`, or `ppu`. On success, restarts the agent and returns the node; see [Node Scheduling and Accelerator Switching](#节点调度与加速卡切换) for restrictions | JWT |
| GET | `/api/v1/nodes/{uuid}/pull-proxy` | The proxy used by the node: `proxy` (the first proxy entry that selects it, in the same form as an item in `/pull/proxies`; `null` when no entry selects it, in which case the node agent uses its own environment) and `runtime`: the runtime type. Docker nodes also include the proxy actually used by dockerd, whether it matches the entry (`applied`; when the node has no proxy, compares systemd's currently loaded `configured_*` configuration with actual values), live-restore, whether `daemon.json` also configures a proxy, and the agent's privileges (`root` / `sudo` / `none`) | JWT |
| GET | `/api/v1/nodes/{uuid}/network` | Removed: 410 `SETTING_REMOVED` (`route` points to `/api/v1/pull/proxies`). Node proxies and site addresses are configured by node labels in pull settings (`/nodes/{uuid}/pull-proxy` identifies the entry used by the node) | JWT |
| PUT | `/api/v1/nodes/{uuid}/network` | Removed: 410 `SETTING_REMOVED`, as above | JWT(admin) |
| POST | `/api/v1/nodes/{uuid}/runtime/restart` | Restart the node's dockerd to apply the proxy, executed by the agent (requires root or passwordless sudo); failures return `RUNTIME_RESTART_FAILED`. If dockerd does not have live-restore enabled, the node's containers also stop | JWT(admin) |
| GET | `/api/v1/nodes/{uuid}/networks` | Networks resolved for each role on the node (NIC/IP/HCA/hints), the management network (address, NAT, disabled roles), reported NICs, RDMA port inventory (including send/receive rates over the last two minutes), and accelerator network port inventory (`accelerator_ports`) | JWT |
| GET | `/api/v1/nodes/{uuid}/ssh` | View the node's SSH credential configuration (without plaintext keys) | JWT |
| PUT | `/api/v1/nodes/{uuid}/ssh` | Write the node's SSH credentials (for offline recovery) | JWT(admin) |
| DELETE | `/api/v1/nodes/{uuid}/ssh` | Delete the node's SSH credentials | JWT(admin) |
| POST | `/api/v1/nodes/{uuid}/recovery/probe` | Manually trigger one recovery probe for an offline node | JWT(admin) |
| POST | `/api/v1/nodes/{uuid}/token/regenerate` | Regenerate the node token (disconnects the current connection) | JWT(admin) |
| DELETE | `/api/v1/nodes/{uuid}` | By default, uninstall the agent using saved SSH credentials, delete the record after confirming completion, and return `{"status":"deleted","agent_uninstalled":true}`. If uninstallation is not confirmed, return 409 `NODE_UNINSTALL_UNCONFIRMED` and retain the node and SSH credentials. Explicitly specifying `?force=true` skips uninstallation, deletes only the record, and returns `agent_uninstalled:false`; both modes reject nodes with instances (409). A non-boolean force value or repeated parameter returns 400; a nonexistent node returns 404 | JWT |
| GET | `/api/v1/events` | SSE change event stream | JWT |

### Node List Filters and Response Fields

`q` matches node names, hostnames, and UUIDs. `state` indicates only the agent connection state: `online`, `offline`, or `connecting`;
accelerator problems do not change it. Sortable columns are `name`, `state`, `agent_version`, `gpu_count`, `cpu_cores`, `mem_total`, and `created_at`.

With pagination, node objects omit `topology` (topology) and `path_usage` (path usage); see `GET /api/v1/nodes/{uuid}` for details.
Paginated responses also include two fields unaffected by filters: `all`, the total number of nodes, and `labels`, user labels across all nodes, in the form `[{"key", "values"}]`.

Filter by labels using `label=key=value`, which can be repeated. Multiple values for the same key use OR; different keys use AND,
following the same matching rules as deployment placement conditions.
Keys starting with `gpuworks.ai/` match system labels, such as `gpuworks.ai/vendor=nvidia` and `gpuworks.ai/gpu-model=A100`;
other keys match user labels. Unknown system keys or invalid values return `400 LIST_FILTER_INVALID`.

`GET /api/v1/nodes/facets` returns **filter options and corresponding node counts** and accepts the same `q`, `state`, and `label` parameters.
Deployment forms and placement condition editors can use it directly to generate options and counts without first reading all nodes. The response includes:

| Content | Meaning |
|---|---|
| Matching node count | Total number of nodes that satisfy the current filters |
| User label values and system label values | Each value includes the number of nodes carrying it; system labels include server-provided display text and exclude the node's own `gpuworks.ai/node` label |
| GPU model | `vendor`, `name`, and `cc` describe the vendor, name, and compute capability; `model` corresponds to the value of the `gpuworks.ai/gpu-model` system label |
| Count and GPU memory for each GPU model | Node count, plus `cards` (card count) and `memory_bytes` (maximum GPU memory per card) |

### Connection Parameters for Node Creation

`POST /api/v1/nodes` creates a node awaiting connection and returns the installation command and token. The connection direction determines address field requirements:

- **`agent_dial` (the default for `connect_mode`)**: The request body must include `server_url`, the address the agent dials; the UI prefills it with the current browser address.
  If missing, returns 400 `SERVER_URL_REQUIRED`. It must be an absolute http(s) address without credentials, query, or fragment; otherwise, returns 400 `SERVER_URL_INVALID`.
- **`server_dial`**: The agent never connects to the server, so `server_url` can be omitted and is not saved if provided; `server_url` in the response is empty.
  `install_command` returns a command for the [offline package](../how-to/connect-nodes.md#离线包): `sh gpuworks-agent.run --token '…' --connect-mode server_dial --listen '…'`.
- **Listen address**: `server_dial_listen` defaults to `0.0.0.0:18800`; it must be a valid `host:port` / `:port` with a port between 1 and 65535; otherwise, returns 400 `SERVER_DIAL_LISTEN_INVALID` (`address`).

### SSH Checks and Installation

`POST /api/v1/nodes/precheck` checks the host environment. Connectivity checks are returned according to the connection mode and detected results and may include:

- `Server reachability`: Whether the node can access the server.
- `Server to agent`: Whether the server can dial `host_address` (defaults to the SSH host). If the address includes a port, that port is used; otherwise, the port from `server_dial_listen` is used (default `0.0.0.0:18800`). In `server_dial` mode, failure to connect is an error, not a warning.
- `Connectivity`: Connectivity summary. See [No Network Connectivity Between the Node and Server](../how-to/connect-nodes.md#节点与-server-网络不通) for troubleshooting.

The last two checks are not displayed when `agent_dial` can reach the server.

On both Linux and macOS, `POST /api/v1/nodes/install` uploads the agent and `install.sh`, then runs the script.
The requirements and error codes for `server_url` and `server_dial_listen` are the same as for [node creation](#创建节点的连接参数). On Linux, execution modes are:

- Register `gpuworks-agent.service` by default.
- `no_systemd: true` uses nohup, does not modify existing services or agents in other directories, and does not reject installation merely because the host is already connected.
  Only an agent already running in the installation directory causes 409 `NODE_INSTALL_EXISTING_AGENT_SAME_DIR` (`dir`); after confirming replacement, retry with `replace_existing_agent`.
  When a directory above the installation directory is closed to the dedicated runtime user `udocker` and the probe establishes that the node will run a root agent with runveil and that user, it answers 400 `NODE_INSTALL_DIR_UNTRAVERSABLE` (`dir`, `mode`, `install_dir`, `fix`) and uploads nothing; `POST /api/v1/providers/{id}/install` answers the same. The precheck request may carry `runtime_driver` so that its row matches the install.

macOS ignores `no_systemd`.

When `install_dir` is omitted, `/opt/gpuworks` is used, falling back to `/tmp/gpuworks` if the default directory is not writable. If an explicitly specified directory is not writable, installation fails without a fallback.

### Node Scheduling and Accelerator Switching

The three values for `PUT /api/v1/nodes/{uuid}/scheduling` have the following effects:

- `cordoned`: No new replicas start; existing replicas continue serving. The candidate pool lists the node separately as `cordoned`; manually placed multi-node or PD ranks assigned to it wait with the reason `node_cordoned`, while automatically placed ones move to other nodes when the replica is about to start (`UNIT_PLACED`).
- `draining`: Also migrates the replicas on this node one at a time. A replacement starts on another node first; once it is running, the original replica drains. This proceeds one replica at a time per deployment. Multi-node and PD replicas move the same way, replacement first (the replacement node has the same compute network, the same GPU architecture, and free cards; the placement is rewritten only once it runs, recorded in the timeline as `NODE_DRAIN_MOVED`), and stay in place waiting if there is no node to go to. Single-node deployments pinned to this node do not move.
- `schedulable`: Resumes scheduling; waiting replicas retry immediately.

`PATCH /api/v1/nodes/{uuid}/accelerator` selects which vendor's cards to manage on a host with multiple vendors. The selection is saved only after the agent accepts it and restarts in place.
The returned node's `accelerator_preference` indicates the selection; `accelerator_vendors` lists `{vendor, cards, model}` for each vendor on the host.

- Invalid value: 400 `NODE_ACCELERATOR_INVALID`; the host has no cards from that vendor: 400 `NODE_ACCELERATOR_NOT_ON_HOST`.
- The node has instances that are not stopped: 409 `NODE_ACCELERATOR_HAS_INSTANCES` (`params.count`); agent rejection or an offline agent also returns 409.
- The node does not exist: 404.

### Node Labels: Written and Generated

Nodes have two types of labels, represented by two DTO fields:

- `labels`: Written by operations staff (`PUT /api/v1/nodes/{uuid}/labels`), with custom keys and values.
- `system_labels`: Read-only labels computed by the platform from facts about the node itself. They are not stored in the database and are recomputed on every read, so card replacements, network changes,
  and network renames are reflected immediately.

Generated keys are all under the reserved `gpuworks.ai/` namespace. Written labels must not use that namespace or its subdomains
(`rack.gpuworks.ai/...`), so the key identifies whether a value is a platform fact or human-authored. Labels used to attach networks follow the same rule.
When upgrading to the version that introduces this rule, migration 119 deletes existing keys of this type from nodes (case-insensitively), along with networks
attached using those keys. Label conditions in deployments that reference these keys are left unchanged, because removing a condition relaxes the constraints. Such deployments
are not scheduled to any node until someone modifies them.

| Key | Value | Description |
|---|---|---|
| `gpuworks.ai/node` | Node UUID | `display` is the node name |
| `gpuworks.ai/vendor` | Canonical vendor ID | One per host |
| `gpuworks.ai/cpu-arch` | `amd64` / `arm64` | |
| `gpuworks.ai/gpu-model` | Normalized model name | One entry per model; nodes with mixed cards have multiple entries |
| `gpuworks.ai/gpu-arch` | Architecture generation within the vendor (`turing`, `910b`, etc.) | Omitted for unrecognized cards |
| `gpuworks.ai/compute-network` | Network ID; `management` when the management network handles the role | Omitted if the role has no network |
| `gpuworks.ai/storage-sync-network` | Same as above | |
| `gpuworks.ai/serving-network` | Same as above | |

The same key can have multiple entries (the values form a set). Each entry is `{key, value, display, display_code}`: `value` is the literal value
used for matching, `display` is display text, and `display_code` is provided only when the value is platform terminology (vendor or management network), referring to
an existing client translation. Clients prefer `display_code` and fall back to `display` if it is unavailable. Network and node values are IDs rather than names,
so renaming changes only `display` and does not affect label-based matching.

## Models

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/models` | List models; supports `page`/`page_size`, `q`, `source` (`huggingface/modelscope/url/local`), `operation`, and `source_id`. The last three accept comma-separated values; `source_id` uses case-insensitive whole-value matching. `sort` accepts `name/source/source_id/published_at/created_at/weight/presence`. Paginated responses return a `presence{ready_nodes,cached_nodes,ready_storages,in_flight,in_flight_percent,failed}` presence summary and a `summary` for all models; query model details for per-location information. See [List Filtering and Presence Summary](#列表筛选与就位汇总) | JWT |
| POST | `/api/v1/models` | Register a model from a public repository (HF/ModelScope) | JWT |
| POST | `/api/v1/models/preview-url` | Preview an HTTP model directory (`url`); requires an http/https URL with a hostname and no embedded credentials. Invalid URLs return 400 `MODEL_SOURCE_URL_INVALID`; valid but unreadable directories return 502 `HTTP_DIRECTORY_UNREACHABLE` | JWT |
| POST | `/api/v1/models/register-url` | Register an HTTP model directory (`source_url`, optional `source_id` and `name`); URL validation is the same as preview-url. Returns 201 on successful registration | JWT |
| POST | `/api/v1/models/local/preview` | Preview whether a local directory on a node can be registered as a model | JWT |
| POST | `/api/v1/models/local` | Register a local model on a node | JWT |
| GET | `/api/v1/models/{id}/logo` | Get the model logo image | JWT |
| GET | `/api/v1/models/{id}` | Get model details | JWT |
| DELETE | `/api/v1/models/{id}` | Delete a model | JWT(admin) |
| POST | `/api/v1/models/{id}/metadata/refresh` | Fetch model metadata again (config/README/logo) | JWT |
| POST | `/api/v1/models/{id}/download` | Start a public repository download: `storage_id` selects the storage (defaults to the default storage when omitted). For local storage, also provide `node_uuid` or `node_uuids`; for shared storage, one writer node downloads the model once | JWT |
| POST | `/api/v1/models/{id}/copy` | Copy model files between two locations: `source`/`target` each contain `{storage_id, node_uuid?}`. Copy locally when the same node can read the source and write the target; otherwise, transfer directly between nodes. Returns 400 if the source and target are the same location; shared storage is identified by storage ID (ignoring the node). State conflicts such as offline or non-writable storage still return 409 | JWT |
| POST | `/api/v1/models/{id}/nodes/{uuid}/cache/retry` | Retry a failed NFS Cache copy (`preparation.retryable=true`), with request body `{storage_id}`; returns 202 when accepted. Returns 409 `MODEL_CACHE_RETRY_BLOCKED` if the task has not failed, the node is disconnected, capacity is insufficient, termination of the previous task is unconfirmed, or a concurrent change occurs | JWT |
| DELETE | `/api/v1/models/{id}/storages/{storage_id}` | Delete model files from shared storage (and all node caches for NFS with caching); returns 409 `STORAGE_SHARED_BY_OTHER_CONTROL_PLANES` if another gpuworks control plane is using it. Use `?force=1` to force deletion | JWT(admin) |
| POST | `/api/v1/models/{id}/register-path` | Register an existing path on a node as local files for this model | JWT |
| POST | `/api/v1/models/{id}/nodes/{uuid}/sync` | Perform direct peer sync from an explicitly selected source node (`source_node`, optional `rate_limit_kib_s`; when no source node is provided, retry using the source node from the failed record) | JWT |
| POST | `/api/v1/models/{id}/nodes/{uuid}/sync/probe` | Test the direct-connection handshake from the source node to the target node without transferring bytes | JWT |
| POST | `/api/v1/models/{id}/nodes/{uuid}/sync/cancel` | Cancel peer sync | JWT |
| DELETE | `/api/v1/models/{id}/nodes/{uuid}` | Evict model files from a node (local storage or node cache; `?storage_id=` specifies the storage). Rejects nodes occupied by an active deployment. Any download in progress at that path is canceled first, and the operation waits for it to end; you can download again immediately after the response | JWT(admin) |

### List Filtering and Presence Summary

`GET /api/v1/models` without pagination parameters returns all models with presence details for each node and storage.
With `page` or `page_size`, it returns `{items, page, page_size, total, summary}`:

- Each row omits `readme`, `nodes`, and `storages`, and uses `presence{ready_nodes, cached_nodes, ready_storages, in_flight, in_flight_percent, failed}` to summarize presence instead; query `GET /api/v1/models/{id}` for details.
- `summary{registered, ready_files, sources, weight_gib}` covers all models and is unaffected by filters.

Use the query parameters as follows:

| Parameter | Usage |
|---|---|
| `q` | A substring of the name, source ID, or revision |
| `source` | `huggingface`, `modelscope`, `url`, or `local`; separate multiple values with commas |
| `operation` | Capability type; separate multiple values with commas. Matching any value is sufficient |
| `source_id` | Case-insensitive whole-value matching of source IDs; separate multiple values with commas. Use this to find registered models by a template's weight source |
| `sort` | `name`, `source`, `source_id`, `published_at`, `created_at`, `weight`, or `presence`; `published_at` is the creation time on the source site, falling back to the most recent update time when unavailable; `presence` sorts by the number of nodes where the model is ready |

### Register an Existing Node Path

The request for `POST /api/v1/models/{id}/register-path` is
`{"node_uuid":"…","path":"/data/existing-model","storage_id":1}`.
`path` is the host path of an existing directory on the node. Provide an absolute path; it can be outside the selected local storage root.
`storage_id` selects the storage to which the record belongs, rather than restricting the path. If omitted, it uses the storage of the model's existing copy on the node first, or the default local storage otherwise.
This operation only registers existing files; it does not copy or move directories. To import a library from shared storage, use the storage scan/import endpoints.

Returns `200 {"status":"registered"}` on success. The node checks that the directory is readable, contains non-empty files, and has no incomplete downloads;
it does not require fixed weight filenames or `config.json`, preserving custom engine layouts. `registered` means the path selected by the caller has been registered;
it does not prove that the directory contains the correct model or that the engine can load it. Do not treat a request as invalid because the path is outside the storage root.

An empty `path` returns 400; a missing target model returns 404. The node rejects missing or empty directories and directories containing `.part`/`.part.state` files;
the endpoint returns 409 and does not replace the existing registration. Errors such as unavailable RPC to the actual node may also return 409; handle them according to the error details.

## Images / Storages (Image Catalog and Storage Policies)

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/images` | List image series | JWT |
| GET | `/api/v1/images/versions` | List image version rows; each row includes `present_nodes`/`total_nodes` and `reference_count`, without per-node rows (see series details for node rows) | JWT |
| GET | `/api/v1/images/engines` | Get the engine dictionary `[{"name","label"}]` for image forms, in display order: all deployment engines plus the catalog-only `vllm-omni` | JWT |
| GET | `/api/v1/images/community/repos` | Get community image repository cards (the six built-in categories merged with the `image_community_catalog` setting, without tags) | JWT |
| GET | `/api/v1/images/community/browse` | Use `?repo=` to list all upstream tags for the repository (any repository is allowed, with no semver filtering; cached in memory for 1 hour). An empty or malformed repository name returns 400; an upstream report that the repository does not exist returns 404 | JWT |
| PATCH | `/api/v1/images/versions/{id}` | Update an image version row: accepts only `enabled` and `registry_sources` (acquisition URLs can also be added to system versions). Other keys return 400 `IMAGE_VERSION_PATCH_INVALID`; the image catalog does not hold startup configuration (ADR 0031) | JWT(admin) |
| GET | `/api/v1/images/{id}` | Get image series details; each version's node rows include `node_name`, `node_state`, `image_digest_id`, and `updated_at`. Rows acquired through peer sync also include `source_node_name` | JWT |
| PATCH | `/api/v1/images/{id}` | Update an image series: `enabled`; community/custom series also allow changes to `common_args`/`common_env` (system series return 400 `OFFICIAL_IMAGE_SERIES_MANAGED`) | JWT(admin) |
| DELETE | `/api/v1/images/{id}` | Delete a community/custom image version; system images return 400 `OFFICIAL_IMAGE_DELETE_FORBIDDEN`. Returns 409 `IMAGE_IN_USE` if still referenced by deployments, unfinished instances, or models (using the same reference criteria as system image deletion/deprecation) | JWT(admin) |
| DELETE | `/api/v1/image-series/{id}` | Delete a community/custom image series; returns 409 `IMAGE_IN_USE` if any version is still referenced | JWT(admin) |
| POST | `/api/v1/images/import` | Accepts two request body types: a system catalog manifest with `schema_version` (schema 2/3; other values return 400), or `{"series":[...]}` for community/custom additions (cannot set `source=official`) | JWT(admin) |
| POST | `/api/v1/images/sync` | Update the system image catalog from the release channel; ignores the request body and returns version counts and series. For errors, see [Catalog Import Results](#目录导入结果) | JWT(admin) |
| POST | `/api/v1/images/preview-archive-url` | Parse a `docker save` archive URL before downloading: `{"url"}` → `url`, `size_bytes`, `repo_tags`, `previewable` | JWT(admin) |
| POST | `/api/v1/images/{id}/nodes/{uuid}/sync` | Trigger image acquisition on a node: an empty request body pulls according to [Pull Settings](../how-to/configure-pull-settings.md) (uses the same candidates as deployment startup; you cannot specify a repository); `source: url` / `url-relay` imports an HTTP archive; or use direct peer sync from `source_node` (optional `rate_limit_kib_s`, unrestricted by sync groups). A `source` value other than `""`, `default`, `url`, or `url-relay` returns 400 `IMAGE_SOURCE_REMOVED`; returns 409 `IMAGE_SOURCE_FORBIDDEN` if source rules leave the node with no available pull source. With `"force":true`, pulls again even if the image is already `present` on the node | JWT |
| POST | `/api/v1/images/{id}/nodes/{uuid}/sync/probe` | Test the direct-connection handshake from `source_node` to the target node without transferring bytes | JWT |
| POST | `/api/v1/images/{id}/nodes/{uuid}/sync/retry` | Retry image acquisition on a node; peer sync reuses the recorded source node and previous rate limit | JWT |
| POST | `/api/v1/images/{id}/nodes/{uuid}/cancel` | Cancel an image transfer in progress on this node | JWT |
| DELETE | `/api/v1/images/{id}/nodes/{uuid}` | Delete this image from the node | JWT(admin) |
| POST | `/api/v1/images/{id}/nodes/{uuid}/inspect` | Read the image's OCI configuration (`entrypoint`/`cmd`/`env`/`volumes`) and image ID on the node; configuration is returned only in the response and is not written back to the catalog | JWT(admin) |
| GET | `/api/v1/storages` | List storage (type, path or export, node summary and problem nodes, model count, and deployments using it) | JWT |
| GET | `/api/v1/storages/{id}` | Get a single storage; `?all_nodes=1` returns the status of each node | JWT |
| POST | `/api/v1/storages` | Add storage; NFS types are test-mounted on one node first, and storage that cannot be written to is saved as disabled (read-only) | admin |
| PATCH | `/api/v1/storages/{id}` | Update the name, enabled status, default status, node scope, path, mount options, cache directory and limit, or `nfs`↔`nfs_cache` type. Include `expected_updated_at` from the read response to prevent overwriting concurrent changes; a version change returns 409 `STORAGE_CONFIG_CHANGED` | admin |
| DELETE | `/api/v1/storages/{id}` | Remove a storage object (without deleting files) and remove models stored only in it from the model list; returns 409 `STORAGE_IN_USE` if deployments are bound to it or a deployment's model is stored only in it | admin |
| POST | `/api/v1/storages/probe` | Test-mount a storage definition on one node without leaving it mounted | admin |
| GET | `/api/v1/storages/{id}/control-planes` | Get other gpuworks control planes using the shared storage (heartbeats) | JWT |
| PATCH | `/api/v1/storages/{id}/node-overrides` | Atomically update cache overrides for fixed `node_uuids`; omitted `cache_path`/`cache_max_bytes` remain unchanged, null inherits, and a limit of 0 means unlimited. New overrides do not expand the node scope | admin |
| POST | `/api/v1/storages/{id}/cache/reconcile` | Submit background cache reconciliation with `{node_uuids, mode}` (`mode` is `"trim"` or `"clear"`); the 202 response returns the accepted count. See the cache status in storage details for per-node results | admin |
| GET | `/api/v1/storages/{id}/model-files` | Get model files in storage: one row per model for shared storage, and one row per node for local storage and `nfs_cache` node caches (including the last-used time and bound deployments) | JWT |
| POST | `/api/v1/storages/{id}/scan` | Scan model directories in shared storage, with pagination (`cursor`) | admin |
| POST | `/api/v1/storages/{id}/import` | Import scanned directories; writes only to the database | admin |
| DELETE | `/api/v1/storages/{id}/cache` | Evict all evictable node caches for `nfs_cache` storage; use `?node_uuid=` to restrict this to one node | admin |
| GET | `/api/v1/storages/{id}/files` | Browse files in storage ([ADR 0053](../adr/0053-storage-files.md)): `?path=` (relative to the storage root; empty means the root) `&cursor=&limit=` (up to 500 per page, ordered by name bytes; an empty `next_cursor` indicates the last page). Returns `{path, node_uuid, writable, items[{name, path, dir, symlink, size, mtime, writable}], next_cursor}`. Top-level `writable` indicates whether children can be created in the current directory; item-level `writable` indicates whether the item can be renamed/deleted. Symbolic links are listed but not followed. For shared storage, `node_uuid` is optional (automatically selects an online, ready node with agent support and a connected data lane, preferring writable nodes). It is required for local storage; otherwise, returns 400 `STORAGE_FILE_NODE_REQUIRED`. For `nfs_cache`, browses NFS itself, not the node cache | admin |
| GET | `/api/v1/storages/{id}/files/download` | Download a file or directory: `?node_uuid=&path=&format=zip\|tar.gz`. Files are returned unchanged (`Content-Length`, `application/octet-stream`); directories are archived while streaming (zip by default, `application/zip` or `application/gzip`, no `Content-Length`). Symbolic links are skipped and listed in `SKIPPED_SYMLINKS.txt` inside the archive. `Content-Disposition: attachment` includes the filename (directories append `.zip`/`.tar.gz`). Errors before byte transfer starts return JSON; errors afterward close the connection directly. Downloads remain available when storage is disabled | admin |
| PUT | `/api/v1/storages/{id}/files/upload` | Stream an upload in a single request: `?node_uuid=&path=&extract=tar\|tar.gz\|zip&overwrite=true\|false`, with file bytes as the request body (`application/octet-stream`). `path` is the destination file without extraction, or the destination directory with extraction. The server does not write to disk or cache the upload; `Content-Length` is the expected size (unknown when omitted). The agent first stages the upload in `.gpuworks/incoming/`, then atomically renames it to the destination after completion (including extraction) before returning `{path, bytes, files}`. All checks that do not require the agent are completed before reading the request body | admin |
| POST | `/api/v1/storages/{id}/files/mkdir` | Create a directory with `{node_uuid, path}` (including missing parents); existing directories do not cause an error. Returns `{path}` | admin |
| POST | `/api/v1/storages/{id}/files/rename` | Rename or move with `{node_uuid, from, to}`; both paths must be in writable regions, and the destination must not exist. Returns `{path}` | admin |
| DELETE | `/api/v1/storages/{id}/files` | Delete a file or directory with `?node_uuid=&path=`: first moves it to the storage's trash directory, then deletes the bytes in the background; 204 | admin |
| GET | `/api/v1/storages/settings` | Get the control plane display name and reserved free space on cache disks | JWT |
| PATCH | `/api/v1/storages/settings` | Update the preceding settings | admin |
| GET | `/api/v1/storages/usage` | Get model disk usage statistics for each node | JWT |

### Storage Configuration and Cache Status

- `updated_at` serves as the edit version; `override_only: true` on a target indicates a node configuration override only, which does not participate in scope selection. Omitting it or setting it to `false` preserves explicit selection semantics.
- NFS cache nodes can include `cache{used_bytes,reclaimable_bytes,protected_bytes,limit_bytes,state,reason?,message?}`.
  `state` is `pending`, `running`, `failed`, `offline`, `blocked`, `satisfied`, or `unavailable`.
- An absent `cache` means statistics are unknown, not 0; when a new directory has not been verified, do not substitute the old directory's disk capacity.

### Storage File Operation Scope and Errors

Uploads, directory creation, renaming, and deletion are allowed only under `files/` and `.gpuworks/apps/`; these two region directories themselves cannot be deleted or renamed.

| HTTP Status | Error Code | Meaning |
|---|---|---|
| 403 | `STORAGE_FILE_REGION_READONLY` | The path is outside a writable region, or the operation attempts to modify the region directory itself |
| 400 | `STORAGE_FILE_PATH_INVALID` | The path contains `..` or backslashes, or resolves outside the storage root |
| 404 | `STORAGE_FILE_NOT_FOUND` | The path does not exist |
| 409 | `STORAGE_FILE_EXISTS` | The destination already exists |
| 409 | `STORAGE_FILE_AGENT_UNSUPPORTED` | The node agent does not support file operations; `params.node` identifies the node |
| 422 | `STORAGE_FILE_EXTRACT_FAILED` | Extraction failed |
| 503 | `STORAGE_FILE_DATA_LANE_UNAVAILABLE` | The node data lane is disconnected during upload or download; retry is allowed |
| 502 | `STORAGE_FILE_OPERATION_FAILED` | Another node-side operation failed |

May also return 409 `STORAGE_DISABLED` (write operations), `STORAGE_NOT_WRITABLE`, `NODE_OFFLINE`, or `STORAGE_UNAVAILABLE_ON_NODE`.

### Catalog Import Results

`POST /images/sync` and `POST /images/import` with `schema_version` import the system catalog and return
`{catalog_version, created, updated, deleted, deprecated, skipped:[], series:[...]}`.
Counts are per version row; `series` contains the system series after import. System versions absent from the manifest are deleted if unreferenced, or deprecated if referenced.
Community/custom imports return the affected series as `{items:[...], total}`.

Failures leave the catalog unchanged. Handle the following errors:

- All release sources are unreachable: 502 `UPDATE_SOURCES_UNAVAILABLE`; an index exists but no catalog file matches it: 502 `UPDATE_DOWNLOAD_FAILED`.
- The channel has not published a catalog: 404 `UPDATE_NOT_PUBLISHED`.
- Invalid content: 400 `IMAGE_MANIFEST_INVALID`; a name or ref conflicts with a community/custom row: 400 `IMAGE_IMPORT_CONFLICT`.

The version row's `rolling_tag`, `deprecated`, and `deprecated_since` describe rolling tags and deprecation status;
`reference_count` counts deployments, unfinished instances, and models that reference the ref.
For the format and import rules, see [Image Catalog and Inference Image Families](image-families.md).

## Inference Templates (Inference Catalog/Templates)

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/inference-templates` | List inference templates | JWT |
| GET | `/api/v1/inference-templates/index` | Get the template index (a lightweight list for selectors) | JWT |
| GET | `/api/v1/inference-templates/organizations` | List organizations/publishers represented in the templates | JWT |
| GET | `/api/v1/inference-templates/chips` | List accelerator cards that templates declare as supported | JWT |
| GET | `/api/v1/inference-templates/my-chips` | List accelerator cards actually detected in the current cluster | JWT |
| GET | `/api/v1/inference-templates/summary` | Get template summary statistics | JWT |
| GET | `/api/v1/inference-templates/export/{digest}` | Export an executable template package by content digest | JWT(admin) |
| GET | `/api/v1/inference-templates/model/*` | Get template details by model path (wildcard path) | JWT |
| GET | `/api/v1/inference-templates/yaml?model_id=…` | Get flat YAML for the template editor: the server writes it using the same catalog types as import. Add `spec_source=user` (or `upstream`) to retain only specs from that source; `readme_*` fields are written only when all specs are listed or the template itself belongs to the user. Saving it unchanged (`import?overwrite=true`) preserves all fields | JWT |
| GET | `/api/v1/inference-templates/schema` | Get the dictionary for the flat YAML editor: field completion, hover descriptions, and constraints for initial client-side validation (authoritative validation still occurs at import) | JWT |
| POST | `/api/v1/inference-templates/clone` | Clone a template into an editable user spec | JWT |
| POST | `/api/v1/inference-templates/evaluate` | Check flat YAML or a catalog package without writing data; accepts the same request as `import` and supports `?overwrite=true`. Returns 200 `{valid, errors, warnings}`; for check scope and results, see [Pre-Import Checks](#导入前检查) | JWT(admin) |
| POST | `/api/v1/inference-templates/import` | Import a template/catalog package; returns `{models, package_digest}` on success, with an additional `warnings` field when warnings exist (same as `evaluate`; the field is absent when there are no warnings) | JWT(admin) |
| POST | `/api/v1/inference-templates/sync` | “Check for updates”: fetch the official inference catalog package from the release channel and reconcile upstream sources (leaving user specs unchanged); ignores the request body. Returns `{models, package_digest, version}`. Returns 409 `UPDATE_CATALOG_INCOMPATIBLE` if the catalog package schema is newer than this server supports; returns 400 `OFFICIAL_CATALOG_NEEDS_NEWER_SERVER` if package parsing or validation fails (`params.error` contains the original error, and the UI indicates that this version of the inference templates requires the latest server version). For other errors, see [Catalog Import Results](#目录导入结果) | JWT(admin) |
| POST | `/api/v1/inference-templates/official` | Upload an official inference catalog package (request body is a `.tar.gz` catalog package, up to 32 MiB), using the same upstream reconciliation as “Check for updates.” Returns 400 `OFFICIAL_CATALOG_PACKAGE_REQUIRED` if the input is not a catalog package, or 400 `OFFICIAL_CATALOG_NEEDS_NEWER_SERVER` if parsing or validation fails | JWT(admin) |

For template package structure and import rules, see [Executable Inference Catalog Package](inference-catalog-package.md).

### Pre-Import Checks

`POST /api/v1/inference-templates/evaluate` accepts the same flat YAML or `.gpuworks-catalog.tar.gz` catalog package as `import`, also supports `?overwrite=true`, and uses the same parsing and validation.

Returns 200 `{valid, errors, warnings}`. Each item in `errors` and `warnings` is `{code, params, error}`: `code` is stable, `error` is English text, and `params` includes `model_id` and `spec`.
When parsing or validation fails, `valid=false` and `errors` contains the same error codes that import would return; parsing errors without a dedicated code use `CATALOG_INVALID`, with the original text preserved in `params.detail`.

This endpoint does not check whether a user spec with the same name already exists (the actual import may return 409), fetch weight sizes, or write to template or catalog package storage. For warning codes, see [Inference Catalog Syntax Reference](inference-catalog-spec.md#导入前检查与警告).

## Deployments / Instances (Deployments and Instances)

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/deployments` | List deployments; with `page`/`page_size`, returns paginated list columns and supports filtering by `q`/`state` (see “Error and Response Conventions”) | JWT |
| GET | `/api/v1/routing-policies` | Get server-provided multi-replica routing policies and localized descriptions | JWT |
| POST | `/api/v1/deployments/preflight` | Run preflight checks for multi-node inference deployment (rank assignment and network probing) | JWT |
| POST | `/api/v1/deployments` | Create a deployment (`model_source` contains only `{storage_id, provider}`: where to store the model and which provider's weights to run; it does not accept an image acquisition method. `placement.pd` specifies a PD disaggregated deployment; see below). Returns 409 `SERVED_NAME_IN_USE` if the served model name is occupied by a deployment that is not being deleted; `params` includes `served_name`, `deployment`, and `deployment_id`. When omitted, the name defaults to the model name and undergoes the same validation | JWT |
| GET | `/api/v1/deployments/{id}/candidates` | Query the candidate pool for the next replica; `status` is `room/busy/unmet/cordoned/offline`. Supports `offset` and `limit` (1–200, default 50). Returns overall `counts`, `unmet_by_key`, and one page of `items`; `room` does not guarantee that the model fits. Check the deployment's `placement_status` as well. See [Query the Candidate Pool](#候选池查询) | JWT |
| GET | `/api/v1/deployments/{id}/draft` | Read the deployment back as the request that creates it (`deployment`), with `revision` and the non-editable fields `locked`. The deploy page fills its edit and "Clone and modify" forms from it. See [Edit a Deployment](#编辑部署) | JWT |
| PUT | `/api/v1/deployments/{id}` | Edit a deployment: submit the modified draft with `base_revision`; `?dry_run=1` returns only the change plan. Launch configuration changes replace instances according to `apply` (`rolling`/`all`/`none`). See [Edit a Deployment](#编辑部署) | JWT |
| PUT | `/api/v1/deployments/{id}/requirements` | Replace the node requirements and placement strategy of a regular single-node deployment and return its details. Affects only replicas started afterward; running replicas are not migrated. See [Modify Node Requirements](#修改节点条件) | JWT |
| GET | `/api/v1/deployments/{id}` | Get deployment details. Takes no query parameters; returns the deployment object directly, or 404 if not found. Includes `instances`, `node_requirements`, `node_requirement_labels`, `placement_strategy`, `ready_replicas` (check each role for PD), `health`, `tokenize_enabled`, and `max_replicas`; `placement_status` gives scheduling failure reasons, `retry_exhausted` identifies replicas that have exhausted their retries, and `restart_requested_at` identifies a rolling restart. See `units` for multi-node replicas. See [Query Deployment Details](#查询部署详情) | JWT |
| PUT | `/api/v1/deployments/{id}/routing` | Change the routing policy (`least_request`, `prefix_affinity`, or `failover`) | JWT + administrator |
| PUT | `/api/v1/deployments/{id}/dgpu` | Adjust `vram_limit_gib` for a single-node dGPU deployment. Returns `live` instances where the change takes effect immediately and `pending` instances that require a restart. See [Adjust the GPU Memory Limit](#调整显存上限) | JWT |
| PUT | `/api/v1/deployments/{id}/health-check` | Enable or disable health checks for the deployment and set the threshold: `{"enabled": true, "threshold_sec": 60}` (`threshold_sec` is 1–600; omit it or use 0 to retain the current value). Enabling requires the model to provide chat or embedding; otherwise returns 400 `FIELD_INVALID` (`params.field` is `health_check`). An out-of-range threshold returns 400 `FIELD_INVALID`. Does not restart instances; the new settings apply to the next check. Returns deployment details | JWT |
| PUT | `/api/v1/deployments/{id}/tokenize` | Enable or disable gateway forwarding of tokenization requests (`/v1/tokenize`, `/v1/detokenize`) to this deployment: `{"enabled": false}`. `enabled` is required; omitting it returns 400 `FIELD_INVALID` (`params.field` is `enabled`), and a non-boolean value returns 400. Returns 404 if the deployment does not exist. Does not restart instances; takes effect after the gateway routing table is rebuilt in response to the change event. Returns deployment details (including `tokenize_enabled`) | JWT |
| GET | `/api/v1/deployments/{id}/events` | Deployment event timeline (newest first, cursor pagination, collapsed on read) | JWT |
| POST | `/api/v1/deployments/{id}/clone` | Clone the deployment configuration, including node requirements and placement strategy. The clone's served model name is the original name followed by `-` and 4 random lowercase letters or digits. The request body `{"name": ...}` sets both the deployment name and the served model name; returns 409 `SERVED_NAME_IN_USE` if the name is taken | JWT |
| POST | `/api/v1/deployments/{id}/stop` | Stop the deployment: delete all instances and retain the configuration | JWT |
| POST | `/api/v1/deployments/{id}/start` | Start a stopped deployment and reschedule it using the retained configuration (resetting each replica's retry count). Does nothing if already running; returns 409 `DEPLOYMENT_DELETING` if the deployment is being deleted | JWT |
| DELETE | `/api/v1/deployments/{id}` | Delete the deployment | JWT |
| POST | `/api/v1/deployments/{id}/sleep` | Put the deployment to sleep (`?level=1\|2`, default 1; `?mode=wait\|abort`, default wait): write the sleep intent to every active instance's desired state. The gateway immediately stops dispatching requests to them, and the deployment is marked as under a deployment-wide sleep command (instances started afterward also sleep accordingly). `?role=prefill` puts only instances of that PD role to sleep (the response includes `role` and `instances`), without placing the whole deployment under a sleep command. Returns 400 `PD_NOT_PD` for non-PD deployments, 400 `PD_ROLE_UNKNOWN` for a role not in the deployment's mode, or 400 `SLEEP_ROLE_CANNOT_EMPTY` for a role that the mode does not allow to be empty (such as decode). With `level=1`, if the node's host memory cannot hold the weights to be offloaded (available memory minus a headroom of max(5% of total, 4 GiB)), returns 409 `SLEEP_HOST_MEMORY_INSUFFICIENT` (`params.node`, `need_gib`, `available_gib`); use L2. A node with no recent host memory observation is not refused | JWT |
| POST | `/api/v1/deployments/{id}/wake` | Wake the deployment: sleeping instances must fit back on their original GPUs; otherwise returns 409 `WAKE_INSUFFICIENT_GPU_MEMORY` (all or nothing). `?role=` wakes only instances of that PD role. `?partial=true` wakes those that fit, with response `{"status":"waking","woken":[...],"refused":[{"instances","node","gpu_indexes","shortfall_gib","occupants"}]}`; still returns 409 if none fit. The 409 parameters describe the first rejected node, and `refused` lists all rejected replicas. Any wake operation revokes the deployment-wide sleep command. A card held by a process outside the platform returns 409 `WAKE_GPU_HELD_OUTSIDE` (same parameters, `refused[].held_outside` is true). `?wait=true` wakes what fits and makes the rest wait for room; `?yield=true` (optionally `&yield_level=1\|2`, default 1, switched to 2 automatically when the occupant's host cannot hold L1) first puts the occupants to sleep and then waits; see [Waiting Wakes and Yielding Wakes](#唤醒等位与让位唤醒) for the responses | JWT |
| POST | `/api/v1/deployments/{id}/units/suggest` | Suggest nodes for one replica of a multi-node deployment. The request body `{"units":[...]}` lists the replicas already selected. Returns a suggested `unit` or the reason placement is not possible, without saving. See [Suggest Nodes for a Multi-Node Replica](#推荐多机副本节点) | JWT |
| POST | `/api/v1/deployments/{id}/units/retry` | Give a replica a fresh retry allowance: request body `{"role","index"}` (`role` is empty for regular deployments). Remove the replica's failed attempts (clean up containers and retain the timeline); the controller then recreates it without affecting other replicas. Returns 404 `UNIT_UNKNOWN` if the replica does not exist, 409 `UNIT_NOT_FAILED` if there are no failed attempts or instances still exist, or 409 `UNIT_RETRY_NOT_ACTIVE` if the deployment is not running | JWT |
| PUT | `/api/v1/deployments/{id}/replicas` | Change the replica count for a single-node deployment (when growing, `place_on` names the nodes the new replicas start on first), the replica node list for a manually placed multi-node deployment, or the replica count for an automatically placed multi-node deployment (`place_on` names the nodes of the new replicas' ranks). See [Adjust Replicas for Regular Deployments](#普通部署的副本调整) for all fields and conditions | JWT |
| PUT | `/api/v1/deployments/{id}/placement-mode` | Switch a multi-node or PD deployment between placement modes: body `{"mode":"auto"}` or `{"mode":"manual"}`. Instances are not restarted and units keep their current nodes; switching to `auto` also drops the cards PD units were pinned to. 200 returns the deployment detail (`placement_mode` has the new value). A `mode` other than these two returns 400 `FIELD_INVALID` (`params.field` is `mode`), a single-node deployment returns 400 `PLACEMENT_MODE_SHAPE`, and an unknown deployment returns 404; see [Automatically Placed Multi-Node and PD Deployments](#自动放置多机与-pd-部署) | JWT |
| DELETE | `/api/v1/deployments/{id}/wake-waiting` | Cancel every waiting wake of the deployment; returns `{"items":[instance UUIDs],"total":n}` | JWT |
| GET | `/api/v1/deployments/{id}/schedule` | The deployment's schedule, `{"items":[schedules],"total":n}`; see [Schedules](#定时计划) | JWT |
| POST | `/api/v1/deployments/{id}/schedule` | Create a schedule entry; 201 returns it. Validation failures return 400 `SCHEDULE_INVALID`, `SCHEDULE_TIMEZONE_UNKNOWN`, `SCHEDULE_REPLICAS_SHAPE` (PD), `SCHEDULE_REPLICAS_MANUAL_PLACEMENT` (a multi-node deployment placed by hand), `SLEEP_NOT_ENABLED`, or `SLEEP_ENGINE_UNSUPPORTED` | JWT |
| PUT | `/api/v1/deployments/{id}/schedule/{sid}` | Replace one entry; the body is the same as for creating it, 200; an entry that does not belong to the deployment returns 404 | JWT |
| DELETE | `/api/v1/deployments/{id}/schedule/{sid}` | Delete one entry, 204; a missing one returns 404 | JWT |
| PUT | `/api/v1/deployments/{id}/idle-sleep` | Set idle sleep `{"after_sec":900,"level":2}`; `after_sec` is 0 (off) or 60–604800 and `level` is 1 or 2. 200 returns the same body; out of range returns 400 `IDLE_SLEEP_INVALID`, and `after_sec` above 0 on a deployment that cannot sleep returns 400 `SLEEP_NOT_ENABLED` or `SLEEP_ENGINE_UNSUPPORTED` | JWT |
| PUT | `/api/v1/deployments/{id}/service-level` | Set the service level `{"level":"offline"}`; values are `resident` / `online` / `shared` / `offline`. 200 returns the same body; an invalid value returns 400 `SERVICE_LEVEL_INVALID` (`params.level`); anything other than resident requires sleep capability (400 `SLEEP_NOT_ENABLED` / `SLEEP_ENGINE_UNSUPPORTED`) and a single-node deployment (400 `SERVICE_LEVEL_SHAPE`); setting online while sharing cards with another online deployment returns 409 `SERVICE_LEVEL_ONLINE_SHARED` (`params.deployment`); see [Service levels](../explanation/service-levels.md) | JWT |
| PUT | `/api/v1/deployments/{id}/node-replacement` | Set how long a lost node is waited for before its replicas move, `{"after_sec":600}`; 0 is never, otherwise 60–604800. 200 returns the same body; out of range returns 400 `NODE_REPLACEMENT_INVALID` (`params.minimum`, `maximum`); see [Reschedule Replicas on Node Loss](../how-to/node-failover.md) | JWT |
| GET | `/api/v1/scheduling/board` | The scheduling board: nodes paged by `?page=&page_size=&q=` (each GPU's awake usage, sleep residue, free amount, occupants, and outside processes) plus fleet-wide `pending` (deployments refused by the scheduler in the last 30 minutes), `waiting_wakes`, `upcoming` (schedule actions in the next 24 hours), and `recent` (the latest 50 automation events, kept for compatibility; the console reads the scheduling history instead); see [Scheduling Board](#调度看板) | JWT |
| GET | `/api/v1/scheduling/history` | The scheduling history: scheduling events of the whole cluster, newest first, cursor-paged (`limit`, `before`, as for the deployment timeline), filterable by `deployment_id`, `category` (`auto` / `failure` / `manual`, comma separated), and `since` / `until` (RFC 3339); an invalid filter returns 400 `FIELD_INVALID` (`params.field`), an invalid cursor or page size 400 `EVENT_CURSOR_INVALID` / `EVENT_LIMIT_INVALID`; see [Scheduling History](#调度记录) | JWT |
| PUT | `/api/v1/deployments/{id}/pd` | Adjust replicas for each role (`roles.<role>.units`) and `fallback` in a PD deployment. `mode`, each role's `tp`/`pp`, and `kv_load_failure` are immutable (`PD_IMMUTABLE`). Returns 400 `PD_NOT_PD` for non-PD deployments | JWT |
| POST | `/api/v1/deployments/{id}/restart` | Recreate the deployment using the current configuration and reset replica retry counts. The request body accepts an optional `strategy` (default `rolling`; `all` interrupts service). See [Restart Strategies](#重启策略) | JWT |
| POST | `/api/v1/evaluate` | Estimate GPU memory, simulate scheduling, and estimate acquisition (without creating a deployment). The response's `acquisition` includes `present` (storage that already holds the model and is readable by the target node), `targets` (storage to which nodes can save the model when acquiring it), `providers` (the template's weight sources), `image`, and `nodes[]` when nodes are specified in the request (see "Evaluation Requests and Acquisition Estimates") | JWT |
| GET | `/api/v1/instances` | List instances | JWT |
| GET | `/api/v1/instances/{uuid}` | Get instance details. Server-derived fields: `replica` (the replica `{role,index}` served by this instance; omitted if none), `asleep` (not receiving requests because of sleep: commanded to sleep or still asleep), `desired_sleep` (the commanded level), and `can_sleep_alone` / `can_wake_alone` (whether it can sleep or wake individually). Followers of a multi-node replica do not report their own sleep level; `sleep_level` is the level of the leader in their generation. For old rows without a recorded role, `dist_role` is inferred from headless and rank. `instances[]` in deployment details includes the same fields, and PD roles include `may_be_empty` | JWT |
| GET | `/api/v1/instances/{uuid}/events` | Instance event timeline (newest first, cursor pagination, collapsed on read) | JWT |
| GET | `/api/v1/instances/{uuid}/launch-facts` | Three-layer projection of instance launch facts (intent/resolved/runtime observation; unknown is not replaced by a fallback; sensitive text is redacted) | JWT |
| GET | `/api/v1/instances/{uuid}/logs` | Instance container logs (SSE, through the agent logtail tunnel) | JWT |
| POST | `/api/v1/instances/{uuid}/sleep` | Put only the replica containing this instance to sleep (`?level=` and `?mode=` are the same as at deployment level). Any rank of a multi-node replica represents the entire replica; intent is written to all its instances, and the leader's engine puts followers to sleep along with it. The response's `instances` lists all ranks. Other replicas in the deployment continue serving, and the deployment is not considered asleep as a whole (new instances are awake if this replica is recreated). Returns 400 if the deployment cannot sleep (same codes as at deployment level), 409 `INSTANCE_NOT_SERVING` if the instance is not serving a replica, or 409 `SLEEP_ROLE_CANNOT_EMPTY` if this is the last awake replica of a role that the PD mode does not allow to be empty | JWT |
| POST | `/api/v1/instances/{uuid}/wake` | Wake the replica containing this instance (the entire multi-node replica, as above). Checks only whether this replica fits back on its original GPUs (409 `WAKE_INSUFFICIENT_GPU_MEMORY`). Revokes the deployment-wide sleep command; other sleeping replicas remain asleep. Also accepts `?wait=true`, `?yield=true`, and `yield_level`, and can return 409 `WAKE_GPU_HELD_OUTSIDE` | JWT |
| POST | `/api/v1/instances/{uuid}/restart` | Restart the replica containing this instance (a follower represents its entire generation). By default, put the replica's leader into `draining` (the gateway stops dispatching new requests, and it retires after finishing its current requests) while starting replacement instances. `?drain=false` immediately removes the entire generation. Does not consume the replica's retry allowance. Records deployment event `REPLICA_RESTARTED{instance,drain}`. Returns 409 `DEPLOYMENT_NOT_ACTIVE` if the deployment is not active, or 409 `INSTANCE_NOT_SERVING` if the instance is not serving a replica (failed, draining, or being removed) | JWT |

The instance launch facts endpoint provides three layers of information: deployment intent (intent), the resolved launch plan (resolved), and runtime observations (runtime observation).
Missing or unavailable information remains unknown and is not replaced with information from another layer; sensitive text is redacted.

### Deployment List Filters and Response Fields

`q` matches the deployment name, served model name, and model name. `state` corresponds to the state displayed in the UI: `running`, `starting`, `pulling`,
`pending`, `failed`, `stopped`, or `deleting`; the deleting state takes precedence over runtime states.
Sortable columns are `name`, `model`, `state`, `replicas`, and `created_at`.

Success returns HTTP 200. In the outer response, `items` is an array of deployment objects, and `total` is the total number of matches after filtering (an integer).
When pagination is enabled, integer fields `page` and `page_size` are also included. An empty result is `items: []`; requests beyond the last page also return an empty array.

With pagination, each deployment returns only the following list fields. See [Query Deployment Details](#查询部署详情) for commonly used fields and the deployment route entries for the remaining descriptions:

| Field Group | Fields |
|---|---|
| Identity and creation time | `id`, `name`, `created_at` |
| Template and model | `template_id`, `template_deviated`, `engine`, `image`, `model_id`, `model_name`, `served_name` |
| Nodes | `node_uuid`, `node_name`, `node_names` |
| State and replicas | `desired_state`, `runtime_state`, `sleepable`, `replicas`, `ready_replicas`, `health`, `preparation` |
| Operations | `operations` |

In a paginated list, `preparation` contains information about the first model file that is not yet ready in an active deployment; it is omitted if there is none.
Complete preparation information is in `model_progress` in the details.

Without pagination, the remaining deployment configuration fields are returned, but `instances` and `node_requirement_labels` are still excluded.
Paginated lists also omit details such as launch configuration and catalog selection snapshots. To retrieve instances or full details, query `GET /api/v1/deployments/{id}`.

### Query Deployment Details

`GET /api/v1/deployments/{id}` returns a deployment's configuration and current state and requires management authentication.
The request has no query parameters or body. The path `id` is a positive integer deployment ID obtained from `items[].id` in the list.

Success returns HTTP 200 with the deployment object directly as the response body, without an outer `item` or `items`.
If the deployment is not found, returns 404 with response `{"error":"..."}`. Clients should handle the HTTP status and display the reason in `error`; this endpoint has no dedicated stable error code.
Authentication failures are handled as described in [Authentication Model](#鉴权模型) above.

The following illustrates the key fields of a regular deployment, omitting other configuration and instance fields:

```json
{
  "id": 7,
  "name": "qwen3-chat",
  "served_name": "qwen3",
  "desired_state": "active",
  "runtime_state": "running",
  "replicas": 1,
  "ready_replicas": 1
}
```

| Key Field | Type | Meaning |
|---|---|---|
| `id`, `name` | Integer, string | Deployment ID and display name in the management UI |
| `served_name` | String | The `model` name to specify in inference requests |
| `model_id`, `model_name` | Integer, string | ID and name of the associated registered model |
| `desired_state` | String | Desired state: `active` (keep running), `stopped` (stopped), or `deleting` (being deleted) |
| `runtime_state` | String | Current runtime state: `pending`, `pulling`, `starting`, `running`, `failed`, or `stopped` |
| `replicas`, `ready_replicas` | Integer | Target replica count for a regular deployment and the current number of replicas with all ranks running. PD deployments omit `ready_replicas`; check `pd.roles[].ready` instead |
| `health` | Object | Health check status and latest results; see [Health Checks](#健康检查) |
| `instances` (optional) | Array of objects | Instances in the deployment; `uuid` identifies an instance, `state` indicates its state, and `node_uuid` and `gpu_indexes` indicate where it runs |
| `model_progress` (optional) | Array of objects | Preparation status of model files; `state` is the file state, `progress` is progress from 0–1, and `message` provides related information |
| `quick_params`, `extra_args` (optional) | Object, string | Saved launch configuration and extra arguments; see [Deployment Launch Parameter Sources](deployment-launch-sources.md) |
| `pd` (optional) | Object | PD deployment mode and replica counts and states for each role; see [PD Disaggregated Deployment](#pd-分离部署placementpd) |
| `idle_sleep` | Object | Idle sleep `{after_sec, level}`; an `after_sec` of 0 means off |
| `node_replacement` | Object | How long a lost node is waited for before its replicas move, `{after_sec}`; default 600, 0 means never |
| `service_level` | String | Service level: `resident` (default), `online`, `shared`, or `offline`; see [Service levels](../explanation/service-levels.md) |
| `sleep_held` | Boolean | Whether the sleep was issued by an operator or a schedule (held sleep): when `true`, requests do not wake the deployment |
| `wake_waiting_since` (optional) | RFC3339 time | Present when a wake of the deployment is waiting for room; the value is when the wait began |

`active` means the deployment is intended to run, not that it can already serve requests; also check its runtime state, ready replicas, and health checks.
Do not uniformly treat absent optional fields as `0` or empty arrays. Use the following fields to determine placement and retry status:

- `node_requirements` is always present and is `{}` when there are no requirements. Fixed nodes are stored in `gpuworks.ai/node`; deleted nodes remain in the requirements but cannot match.
  Details additionally return `node_requirement_labels[]`: `{key,value,display,display_code?,nodes}`. Nodes and networks display names; `nodes` is the current match count. A value of 0 does not mean the requirement is invalid, only that no nodes currently match. Lists do not include these display fields.
- `placement_strategy` is `spread` or `binpack`. When a regular single-node replica has no instance yet, `placement_status{unscheduled,reason,at}` provides the waiting count and latest reason: `pool_empty` or `pool_no_fit`.
  Reason parameters include counts by category, up to 3 closest candidates, and rejection reasons; at most one event is recorded per waiting timeout window.
- `placement_mode` appears only on multi-node and PD deployments: `auto` means the platform picks the nodes (at creation from free cards, and again when a unit starts and no longer fits), `manual` means the operator wrote them; deployments that existed before the upgrade are all `manual`. Single-node deployments do not carry this field; their placement is in the node requirements (`placement_mode` and `node_uuid` in `GET /deployments/{id}/draft`).
- `max_replicas` is the upper limit when adjusting replica counts; `restart_requested_at` appears only while a rolling restart is in progress.
- For regular deployments, `retry_exhausted{index,failed_attempts,last_failure}` identifies replicas that have exhausted their retries; use `units/retry` to retry them. For multi-node deployments, `units[]` gives each replica's leader and `followers[]`. See [Adjust Replicas for Regular Deployments](#普通部署的副本调整) for adjustment methods.

For a request example, see [Query Details of One Deployment](api-conventions.md#3-查询一个部署的详情).

### Query the Candidate Pool

`GET /api/v1/deployments/{id}/candidates`

Provides the deployment candidate pool's live state. From the perspective of the "next replica," each cluster node is categorized by the first check it fails (`room`: appears to have free GPUs; `busy`: GPUs occupied by awake instances; `unmet`: does not meet node requirements; `cordoned`: scheduling stopped or eviction in progress; `offline`: offline). `counts` covers all nodes and sums to `nodes`; `unmet_by_key` counts nodes by the first unmet requirement key.

`items` contains one page of details (candidates include `rank` in replica attempt order, followed by nodes with unmet requirements and offline nodes). Filter with `?status=` and paginate with `?offset=` and `?limit=` (1–200, default 50). Invalid parameters return 400 `FIELD_INVALID`.

This endpoint does not simulate placement. Whether nodes that "appear to have free GPUs" actually have enough capacity is determined by the controller's simulation; see `placement_status` in deployment details.

### Edit a Deployment

`GET /api/v1/deployments/{id}/draft` returns `{"deployment": {...}, "revision": "...", "locked": [...]}`:

- `deployment` has the same shape as the `POST /api/v1/deployments` request. `quick_params` is the complete launch configuration the deployment stores. A manual placement of a single-node deployment is written as `placement_mode: "manual"` with `node_uuid`, and the vendor as `target_vendor`; neither appears in `node_requirements`.
- `revision` identifies the configuration the draft was read from. Send it back as `base_revision` when editing.
- `locked` lists the request fields this deployment cannot edit: model, template, engine, vendor, name and served model name, weight source, and node count. Multi-node and PD deployments also lock the per-rank nodes, replica count, dGPU quota, and related fields (change them with [Replica Adjustment for Regular Deployments](#普通部署的副本调整) and the PD API); a PD deployment's `placement` is locked too. To change these fields, clone the deployment.

`PUT /api/v1/deployments/{id}` request body:

```json
{"deployment": {...}, "base_revision": "...", "apply": "rolling", "start": false, "wake": false}
```

`deployment` is the modified draft. A `locked` field, when stated, must match the draft (omitted means unchanged); otherwise the response is 400 `DEPLOYMENT_FIELD_NOT_EDITABLE` (`params.field`). Other fields are validated by the create rules (parameter dictionary, node requirements, health checks, the "never fits" check `DEPLOYMENT_NEVER_FITS`, and so on).

The launch configuration starts from the configuration the deployment stores and does not reapply the template: values sent back unchanged from the draft stay as they are, and only changed values are checked against the parameter dictionary's ranges. Template changes made after the deployment are not brought in by an edit, and dictionary defaults the deployment never had are not added.

`?dry_run=1` returns only the plan and writes nothing:

| Field | Description |
|---|---|
| `changes[]` | Per-field changes `{field, layer, from, to}`, named as in the request (launch parameters are `quick_params.<key>`, container parameters `container_overrides.<key>`). Environment variables are listed one by one as `container_overrides.env.<NAME>` with only `masked: true` and no values |
| `changes[].layer` | `live`: takes effect at once (replica count, routing, health check, tokenize, start timeout, node requirements and placement strategy); `restart`: read when an instance starts (launch parameters, extra arguments, image, container parameters, parallelism, sleep capability, dGPU quota), so running instances keep the old value until they are replaced |
| `restart` | Whether any change is in the `restart` layer |
| `apply_options`, `default_apply` | The apply modes available in the current state and the default; see the table below |
| `can_start` | The deployment is stopped and can be started with the edit (`start: true`) |
| `can_wake` | The deployment is under a sleep order that `apply: "all"` can withdraw at the same time (`wake: true`), so the replacement instances start awake |
| `apply_note` | Why the apply modes are fewer than the deployment's state alone allows; `partition_change` is described below |
| `revision` | The configuration the plan was judged against |

| Deployment state | `apply_options` | Default |
|---|---|---|
| Partition shape changes (dGPU turned on or off, another partition backend), `apply_note` is `partition_change` | `all`, `none` | `all` (`none` under a sleep order) |
| Running with at least two replicas, at least one serving | `rolling`, `all`, `none` | `rolling` |
| One replica, or no replica serving (starting, failed) | `all`, `none` | `all` |
| Under a sleep order | `none`, `all` | `none` |
| Stopped | `none` | `none` |

- `rolling` is the rolling restart of [Restart Strategies](#重启策略): replicas are replaced one at a time, failed attempts are retired, and retry counts are reset.
- `all` replaces every instance at once and interrupts service; failed attempts are also retired.
- `none` only writes the configuration, which takes effect the next time an instance starts.
- When the partition shape changes, every old instance must exit before the new ones start (dGPU caches and card claims are recorded per instance, so the old and new generations cannot coexist). The replicas therefore cannot be replaced one at a time, and service is interrupted until the new instances are ready. Resizing a dGPU slice alone does not change the partition shape and can roll.
- When only `live` changes are present, no instance restarts, and `apply` must be omitted or `none`.

An `apply` not in `apply_options`, `start` on a deployment that is not stopped, or `wake` without a sleep order or without `all` returns 400 `DEPLOYMENT_EDIT_APPLY_INVALID` (`params.apply`, `params.options`).

Without `dry_run`, the configuration, node requirements, and apply mode are written in one transaction, and the response is 200 `{"plan": {...}, "deployment": {...}}`, where `plan.revision` is the new revision after the edit. When nothing changes, the response returns at once without writing or recording anything.

Conflicts and limits:

- A missing `base_revision` returns 400 `FIELD_INVALID`. A `base_revision` that does not match the current configuration (the deployment changed after the draft was read), or a configuration, node requirement, or desired state that changed while writing, returns 409 `DEPLOYMENT_CHANGED`. State the controller advances on its own, such as rolling restart progress or the sleep level, is not a conflict.
- When a restart is needed, shared storage is checked as by the restart API; unreachable storage returns 409.
- A deployment being deleted returns 409 `DEPLOYMENT_DELETING`; a missing one returns 404.

The timeline records `DEPLOYMENT_EDITED` with `fields` (the changed fields, comma-separated), `count`, and `apply`.

`POST /api/v1/evaluate` accepts `deployment_id`: the evaluation is treated as an edit of that deployment, so placement does not count the GPU memory its existing instances hold, and its own instances are not suggested for sleep.

### Modify Node Requirements

`PUT /api/v1/deployments/{id}/requirements`

Replace the node requirements and placement strategy of a regular single-node deployment without redeploying: `{"node_requirements": {...}, "placement_strategy": "spread"}` (omit the strategy to retain it). Affects only replicas started afterward; running replicas are not migrated (restarting a replica places it again according to the new requirements).

The vendor is determined by the deployment image: omitting it retains the current vendor; specifying a different vendor returns 400 `NODE_REQUIREMENTS_INVALID`. Requirements are validated as during creation. The same "never fits" check as at creation applies: if no node under the new requirements can fit one replica even when idle, returns 400 `DEPLOYMENT_NEVER_FITS` and leaves the original requirements unchanged.

Multi-node and PD deployments list nodes per rank and return 400 `NODE_REQUIREMENTS_NOT_EDITABLE` (`params.shape` is `multi_node` or `pd`). Returns 409 `DEPLOYMENT_DELETING` if deletion is in progress. Success returns deployment details.

### Adjust the GPU Memory Limit

Example `PUT /api/v1/deployments/{id}/dgpu` request: `{"vram_limit_gib":12}`. Applies only to single-node dGPU deployments.
New instances use the new limit; running instances are adjusted immediately without restarting where possible. Returns 200 `{deployment,live,pending}`:

- `live`: UUIDs of instances where the change takes effect immediately.
- `pending`: `{instance_uuid,reason,used_bytes}` for instances that require a restart for the change to take effect.
  `reason` is `backend` (the backend reads it only at startup), `in_use` (usage exceeds the new limit), `starting`, `asleep`, or `no_usage`.

Call restrictions:

- Full-GPU deployments: 400 `DGPU_QUOTA_NOT_PARTITIONED`; multi-node/PD: 400 `DGPU_QUOTA_NOT_EDITABLE` (`params.shape`).
- Non-positive limit: 400 `DGPU_QUOTA_INVALID`; no node can fit the deployment with the new limit: 400 `DEPLOYMENT_NEVER_FITS`.
- When increasing the limit, the GPU still cannot accommodate it after excluding the instance's own usage: 409 `DGPU_QUOTA_NO_ROOM` (`params.node`, `gpu`, `free_gib`, `need_gib`).
- Deployment is being deleted: 409 `DEPLOYMENT_DELETING`.

The timeline records `DEPLOYMENT_DGPU_QUOTA_CHANGED`, including `from_bytes`, `to_bytes`, `live`, and `pending`.

### Suggest Nodes for a Multi-Node Replica

`POST /api/v1/deployments/{id}/units/suggest`

Suggest nodes for one more replica of a multi-node deployment. You confirm the suggestion in "Adjust Replicas" and save it with the list (this endpoint writes nothing). The request body `{"units": [...]}` lists replicas already in the editor (their leaders cannot serve as leaders again; replicas that do not yet exist in the deployment occupy these nodes according to the GPU count per rank).

Suggested nodes share the same compute network and GPU architecture, each have enough free GPUs for one rank, are schedulable and online, and meet the deployment's node requirements. Each rank is evaluated using the deployment's saved configuration; at most the best 3 groups are considered, with 3 nodes tried per rank.

Returns `{"unit": {"node_uuid", "followers"}, "network": {"id","name"}, "arch", "nodes": [{"uuid","name","free_gpus","model"}]}`. If placement is not possible, `unit` is `null`, and `reason.code` is `unit_none` (node counts excluded by category and the maximum number of nodes that can form a group) or `unit_no_fit` (the first rejected node and its reason). Non-multi-node deployments return 400 `REPLICAS_BODY_SHAPE`.

### Restart Strategies

`POST /api/v1/deployments/{id}/restart` applies to all deployment shapes. It recreates the deployment using the current network and image, resets each replica's retry count, and retires failed attempts.
Omit the request body or submit `{"strategy":"rolling"}` / `{"strategy":"all"}`:

- `rolling` (default): Replace replicas one at a time. The old replica enters `draining` and exits after finishing existing requests, while replacement instances start; the next replica is processed after service recovers.
  A regular multi-node replica reuses the leader's master port, so replacement instances must wait for the old generation to exit before starting on the same node.
  Returns `restart_requested_at` while in progress and records `DEPLOYMENT_ROLLING_RESTARTED{replicas}` in the timeline.
- `all`: Immediately stop all instances, then recreate them together, interrupting service in the meantime. Records `DEPLOYMENT_RESTARTED{instances}`.

An unknown strategy returns 400 `RESTART_STRATEGY_INVALID`; a non-active deployment returns 409 `DEPLOYMENT_NOT_ACTIVE`.

### Waiting Wakes and Yielding Wakes

`?wait=true` and `?yield=true` on `POST /api/v1/deployments/{id}/wake` and `POST /api/v1/instances/{uuid}/wake`:

- `wait`: replicas that fit wake first and the rest wait for room; the server wakes them automatically once capacity frees up, and gives up after 30 minutes (timeline `WAKE_WAIT_TIMEOUT`). The start of the wait is kept on the instance, and the deployment's `wake_waiting_since` is the earliest of them.
- `yield`: first puts the awake deployments holding the needed GPUs to sleep (`yield_level` is 1 or 2, default 1; when the occupant's host memory cannot hold L1 it switches to 2 automatically), then waits.
- When everything fits the response is 200 `{"status":"waking","woken":[…],"waiting":[],"yielded":[]}`; when some must wait it is 202 `{"status":"waiting",…}`, where `waiting` lists the instance UUIDs waiting and `yielded` the names of the deployments put to sleep.
- When an occupant cannot sleep (a deployment that is not sleep-capable, the last awake role of a PD deployment, or a process outside the platform) the response is 409 `WAKE_YIELD_BLOCKED` (`params.occupants` names, `woken`) and nothing changes; a `yield_level` other than 1 or 2 returns 400 `SLEEP_INVALID_LEVEL`.

A wake without these parameters behaves as before. For usage see [Sleep and Wake Operations](../how-to/sleep-wake.md#放不下时等位与让位).

### Schedules

`/api/v1/deployments/{id}/schedule` manages a deployment's schedule. A schedule entry:

```json
{"id":3,"action":"sleep","weekdays":[1,2,3,4,5],"time":"22:00","timezone":"Asia/Shanghai","sleep_level":2,"enabled":true,"next_at":"2026-10-26T14:00:00Z"}
```

| Field | Meaning |
|---|---|
| `action` | `sleep`, `wake`, or `replicas` |
| `weekdays` | Array, 0 for Sunday through 6 for Saturday, at least one, no repeats |
| `time` | `HH:MM`, 24-hour |
| `timezone` | An IANA time zone name; an empty string uses the server's time zone |
| `sleep_level` | Required for `sleep`, 1 or 2; not accepted for other actions |
| `replicas` | Required for `replicas`, 1 to `max_replicas`; single-node deployments and automatically placed multi-node deployments (not PD); not accepted for other actions |
| `enabled` | May be omitted in a request, default true |
| `next_at` | Response field, the next time it runs; omitted when disabled |

Each occurrence runs once; after a server outage only the newest missed occurrence runs, and only if it is within 12 hours; manual operations are not overridden; a new or edited entry takes effect from its next occurrence. See [Use the Scheduling Center](../how-to/scheduling-center.md).

### Scheduling Board

`GET /api/v1/scheduling/board?page=1&page_size=20&q=` is read-only. The response:

| Field | Meaning |
|---|---|
| `nodes` | A paged object (`items`, `page`, `page_size`, `total`) with nodes sorted by name; `q` matches the name or UUID. Each node has `uuid`, `name`, `online`, and `gpus[]`: `index`, `name`, `memory_bytes`, `awake_bytes`, `sleeping_bytes`, `free_bytes`, `held_outside`, and `occupants[]` (`deployment_id`, `deployment`, `instance`, `state`: `awake` or `asleep`, `sleep_level`, `claim_bytes`, `residue_bytes`) |
| `pending` | Active deployments refused by the scheduler in the last 30 minutes: `deployment_id`, `deployment`, `reason{code,params}`, `at` |
| `waiting_wakes` | Wakes waiting for room: `deployment_id`, `deployment`, `instances`, `since` |
| `upcoming` | Schedule actions in the next 24 hours: `deployment_id`, `deployment`, `schedule_id`, `action`, `sleep_level`, `replicas`, `at` |
| `recent` | The latest 50 automation events: `deployment_id`, `deployment`, `code`, `params`, `at` |

For timeline event codes see [Use the Scheduling Center](../how-to/scheduling-center.md#时间线事件) and [Reschedule Replicas on Node Loss](../how-to/node-failover.md#时间线事件).

### 调度记录

`GET /api/v1/scheduling/history` is read-only and returns scheduling events across deployments, newest first:

```bash
curl --fail-with-body -sS \
  "$API_URL/api/v1/scheduling/history?category=failure,manual&since=2026-10-27T00:00:00Z&limit=50" \
  -H "Authorization: Bearer $TOKEN"
```

```json
{"items":[
  {"id":812,"created_at":"2026-10-28T09:14:03Z","deployment_id":7,"deployment":"qwen","kind":"schedule",
   "code":"REPLICA_REBUILT","category":"failure",
   "params":{"replica":1,"attempt":2,"reason":"CUDA out of memory","node":"gpu-3"}},
  {"id":810,"created_at":"2026-10-28T09:10:41Z","deployment_id":7,"deployment":"qwen","kind":"schedule",
   "code":"REPLICAS_CHANGED","category":"manual","params":{"from":1,"to":3,"actor":"alice"},"actor":"alice"}
]}
```

| Parameter | Meaning |
|---|---|
| `limit`, `before` | As for `GET /api/v1/deployments/{id}/events`: `limit` defaults to 100 and is capped at 500; pass the `id` of the last item of the previous page as `before` for the next page |
| `deployment_id` | Only one deployment |
| `category` | `auto`, `failure`, `manual`; give several separated by commas; omitted means all |
| `since`, `until` | RFC 3339 times, inclusive |

| Response field | Meaning |
|---|---|
| `id`, `created_at` | The event id (the paging cursor) and its UTC time |
| `deployment_id`, `deployment` | The deployment and its name; for a deleted deployment the name comes from what it left behind and `deployment_deleted: true` is set; when not even a name is known, `deployment` is omitted |
| `instance` | The instance UUID when the event hangs on an instance, otherwise omitted |
| `kind`, `code` | The event kind (`schedule` / `automation`) and the event code |
| `category` | `auto`: the platform acted on its own settings or schedules; `failure`: something went wrong and the platform is dealing with it or has given up; `manual`: an operator's order |
| `params` | The event code's params, as on the deployment timeline |
| `actor` | The user who gave the order; present only on `manual` events whose request carried an identity, and also in `params.actor` |

The server decides in one place which event codes belong to the history and in which category:

| Category | Event codes |
|---|---|
| `auto` | `AUTO_SLEEP`, `AUTO_WAKE`, `AUTO_SCALE`, `WAKE_WAITING`, `WAKE_WAIT_TIMEOUT`, `SCHEDULE_ACTION_FAILED`, `YIELDED_FOR_WAKE`, `UNIT_PLACED`, `NODE_DRAIN_MOVED` |
| `failure` | `NODE_REPLACED`, `NODE_REPLACE_REVERTED`, `NODE_REPLACE_NO_CANDIDATE`, `REPLICA_REBUILT`, `UNIT_RETRY_EXHAUSTED`, `DEPLOYMENT_UNSCHEDULABLE`, `DEPLOYMENT_ROLLING_STALLED` |
| `manual` | `REPLICAS_CHANGED`, `REPLICA_RESTARTED`, `UNIT_RETRIED`, `DEPLOYMENT_RESTARTED`, `DEPLOYMENT_ROLLING_RESTARTED`, `SLEEP_ORDERED`, `WAKE_ORDERED` |

An order an operator gives through the API leaves an event on the deployment timeline with `params.actor`:

| Event code | When it is recorded | Main params |
|---|---|---|
| `REPLICAS_CHANGED` | `PUT /deployments/{id}/replicas` or `PUT /deployments/{id}/pd` changed the replica count (no event if the count is unchanged). Unrelated to the 409 error code of the same name | `from`, `to`, and `role` for PD |
| `SLEEP_ORDERED` | A deployment, a PD role, or one replica was ordered to sleep | `level`, `mode`, `scope` (`deployment` / `role` / `replica`), `role`, `instances` |
| `WAKE_ORDERED` | A wake started waking at least one instance; a wake in which every instance waits for room records nothing here (it has `WAKE_WAITING`) | `instances` |
| `DEPLOYMENT_RESTARTED`, `DEPLOYMENT_ROLLING_RESTARTED`, `REPLICA_RESTARTED`, `UNIT_RETRIED` | Restarts and retries | Their existing params |

The platform's own actions (the controller, the automation) carry no `actor`.

When a failed replica is rebuilt, the controller records `REPLICA_REBUILT` (deployment timeline, `kind` `schedule`), one per rebuild, so their number is bounded by the retry budget (see [Retry After a Fix](../how-to/troubleshooting.md#修复后重试)):

| Param | Meaning |
|---|---|
| `replica` | The replica number, counting from 1 |
| `role` | The role of a PD deployment |
| `attempt` | Which attempt this is |
| `reason` | The message of the previous failed attempt, cut off after 200 characters |
| `node` | The name of the node of the new attempt's leader |

The read goes through the `(kind, id)` index of `instance_events`: one page per event kind, merged, without scanning the whole timeline.

### Adjust Replicas for Regular Deployments

`PUT /api/v1/deployments/{id}/replicas` adjusts replicas of regular deployments. Multi-node and single-node deployments use different request bodies.

For **manually placed multi-node deployments**, submit all desired replicas: `{"units":[{"node_uuid":"<leader>","followers":[{"node_uuid":"..."}]}]}`.

- Each replica must have exactly `nnodes` ranks, each on a different known node; otherwise returns `REPLICA_UNIT_RANKS` / `REPLICA_UNIT_NODE`.
- GPUs cannot be specified; otherwise returns `REPLICA_UNIT_GPUS`.
- Leaders of different replicas cannot be on the same node; otherwise returns `REPLICA_LEADER_SHARED`. This is because leaders listen on the deployment's master port.
- The total number of GPUs required by all ranks listed on a node, regardless of which replica they belong to, cannot exceed that node's GPU count; otherwise returns `REPLICA_UNIT_GPUS_INSUFFICIENT`.
- Nodes for new replicas or replicas whose nodes have changed must also be online (`REPLICA_UNIT_NODE_OFFLINE`) and meet the deployment's node requirements (`REPLICA_UNIT_REQUIREMENTS`). Already listed replicas whose nodes and order remain unchanged are retained as-is, so offline nodes or changed labels do not block changes to the entire list.
- When the first replica's leader changes, the master address stored on the deployment is cleared; each replica rendezvouses at its own leader's parameter-network address.
- Replicas removed from the list drain and then retire. New replicas form separate groups and rendezvous at their own leaders. The response's `units` lists current replicas and their retry status.

For **automatically placed multi-node deployments** (`placement_mode` is `auto`), submit the replica count: `{"replicas":N,"place_on":["<node uuid>"],"remove":["<uuid>"]}`, without `units` (with `units`, the list is written as given, as above).

- When growing, the server picks nodes for the new replicas by the placement strategy (within a replica, one compute network and one GPU architecture, a free-card slot for one rank on each node, and a leader that does not share a node with another replica's leader) and writes them into the deployment's replica list; the rules are those of [how nodes are chosen at creation](../how-to/multi-node-inference.md#创建时怎样挑节点).
- `place_on` is optional and used only when growing: it names nodes for the new replicas' ranks in order (the first new replica's ranks 0…N-1, then the next replica's). You may give only some, and the server fills in the rest. More entries than the new replicas have ranks returns 400 `REPLICAS_PLACE_ON_TOO_MANY` (`params.count`, `maximum`); named nodes that cannot form a replica that fits return 409 `REPLICAS_PLACE_ON_REFUSED` (`params.node`, `reason` is `no_group`); when no group fits at all, 409 `UNIT_PLACEMENT_NONE`.
- `remove` is used only when shrinking; with it on a grow, the request returns 400 `REPLICAS_REMOVE_TOO_MANY` (`params.maximum` is 0).
- When shrinking, the replicas of the instances listed in `remove` go first (an instance that is not in this deployment returns 400 `REPLICAS_REMOVE_INVALID`, more than the reduction returns 400 `REPLICAS_REMOVE_TOO_MANY`); without it the most recently added go. At least one replica remains, otherwise 400 `REPLICAS_TOO_FEW`.

For **single-node deployments**, submit the replica count: `{"replicas":N,"remove":["<uuid>"]}`.

- `place_on` is optional and used only when adding replicas: a list of node UUIDs the new replicas start on first. It decides only the first placement, and the hint is kept in server memory. Naming more nodes than replicas added returns 400 `REPLICAS_PLACE_ON_TOO_MANY` (`params.count`, `maximum`); a node that cannot take a replica returns 409 `REPLICAS_PLACE_ON_REFUSED` (`params.node`, `reason`: `unknown`, `offline`, `cordoned`, `requirements`, `no_room`, with `detail` or `code` / `params` where relevant).
- `remove` is optional and specifies which instances' replicas to remove when scaling down. These replicas enter `draining` and retire after finishing requests. If omitted, the server selects replicas, choosing unready replicas first, then the most recently created.
- The count must satisfy 1 ≤ N ≤ the deployment's `max_replicas` (currently 32); otherwise returns `REPLICAS_TOO_FEW` / `REPLICAS_TOO_MANY`.
- Returns 400 `REPLICAS_REMOVE_TOO_MANY` if `remove` contains more entries than the number of replicas being removed; returns 400 `REPLICAS_REMOVE_INVALID` if a specified instance is not serving a replica of this deployment.
- If the replica count or specified instances change during the request, for example because of concurrent scaling or an instance that has just failed: 409 `REPLICAS_CHANGED`, with no changes written.

**Error conditions shared by both requests:**

- Request body does not match the deployment shape (PD, a count for a manually placed multi-node deployment, a list for a single-node deployment, or `place_on` on a manually placed multi-node deployment): 400 `REPLICAS_BODY_SHAPE`.
- Deployment is being deleted: 409 `DEPLOYMENT_DELETING`.

### Configuration Fields for Evaluation and Creation

`POST /evaluate` and `POST /api/v1/deployments` share the same deployment configuration request; evaluation results do not reserve resources. Configuration fields are as follows:

| Field | Semantics |
|---|---|
| `quick_params` | Submit values in the types declared by the `/engines` parameter dictionary; explicit `false` differs from omission |
| `quick_params_unset` | Array of parameter keys explicitly excluded from inheritance in this request, preventing template backfill; not written to persistent configuration |
| `placement_mode` | `auto` lets the server select nodes; `manual` uses explicit nodes. Defaults to `manual` if omitted. Single-node: only request terminology, the deployment does not store this field, and manual placement is stored as node requirement `gpuworks.ai/node: [node_uuid]`. Multi-node and PD: with `auto` the server picks nodes for the ranks or units the request leaves out at creation (see [Automatically Placed Multi-Node and PD Deployments](#自动放置多机与-pd-部署)), the deployment is stored as automatically placed, and `placement_mode` in the detail is `auto` |
| `target_vendor` | Evaluation and creation use the same accelerator vendor; stored as node requirement `gpuworks.ai/vendor`. If omitted, creation fills it in using the destination node's vendor |
| `node_requirements` | Node requirements: `{"<key>": ["<value>", …]}`. Multiple values for one key mean "any of these"; different keys must "all match." Keys can be node labels or system labels (see "Node Labels" above). Any key in the reserved domain other than the eight system keys is rejected (a misspelled key does not silently match 0 nodes). `gpuworks.ai/vendor` can have only one value; network key values are network IDs or `management`. Multi-node and PD deployments list nodes per rank and do not accept `gpuworks.ai/node`. Errors: `NODE_REQUIREMENTS_INVALID`; selected nodes do not meet requirements: `NODE_REQUIREMENTS_UNMET` (`params.detail` identifies the key) |
| `service_level` | The service level; when omitted it is `resident`. Anything other than resident requires `sleepable` and a single-node deployment, otherwise 400 `SERVICE_LEVEL_INVALID` / `SLEEP_NOT_ENABLED` / `SERVICE_LEVEL_SHAPE`. Evaluation computes placement from it: while a deployment that shares cards by service level is asleep, levels it does not share with are still counted at full usage |
| `placement_strategy` | `spread` or `binpack`, saved with the deployment; other values return `PLACEMENT_STRATEGY_INVALID`. When omitted it is `spread` for single-node and `binpack` for multi-node and PD (automatic or manual) |
| `placement.gpus`, `placement.tp`, `placement.pp` | Explicit physical GPU indexes; TP and PP are parallelism for the entire deployment (TP × PP ÷ `nnodes` GPUs per node, which must divide evenly). If TP is omitted for multi-node deployment, the default is one pipeline stage per node, with TP set to the smallest GPU count among the selected nodes; effective values are written back to placement. If TP is omitted for single-node deployment, the scheduler selects a GPU count that is an integer multiple of PP (TP = GPU count ÷ PP). Automatic placement also selects nodes according to the supplied TP and PP. Evaluation returns values in `form_values.tp/pp` and `parallelism` (`tp`, `pp`, `nodes`, `gpus_per_node`); `acquisition.nodes` in multi-node evaluation and manual placement provides acquisition estimates for each node |
| `routing_policy` | Multi-replica gateway policy: `least_request` (default), `prefix_affinity`, or `failover`; not an engine launch parameter |
| `tokenize_enabled` | Whether the gateway forwards `/v1/tokenize` and `/v1/detokenize` to this deployment (see [OpenAI-Compatible Gateway](openai-gateway.md#分词tokenize)). Enabled if omitted; `false` disables it. Independent of the engine; if the engine does not provide these endpoints, the engine itself returns an error |
| `health_check`, `health_check_threshold_sec` | Service health checks (see "Health Checks" below): when `health_check` is omitted, enabled if the model provides chat or embedding, otherwise disabled. If explicitly `true` and the model provides neither, creation returns 400 `FIELD_INVALID`. The threshold is 1–600 seconds; omission or 0 means 60. For out-of-range values, evaluation reports a field error and creation returns 400, both with code `FIELD_INVALID` |
| `start_timeout_sec` | Startup health check budget (seconds) for instances of this deployment: the maximum time after an instance starts to wait for the engine health check to pass. Omission or 0 follows the cluster default (setting `deployment.start_timeout_sec`, factory default 1800); otherwise must be 60–86400. For out-of-range values, evaluation reports a field error and creation returns 400, both with code `FIELD_INVALID`. Saved with the deployment and returned under the same field name in deployment details when a value is set. The value is fixed when an instance is created; changes affect only instances started afterward |

### Health Checks

When enabled, the server sends a minimal request through the gateway every minute, selects a replica according to the gateway policy, and waits up to the threshold:
for chat, it sends a message containing `1` with `max_tokens:1`; when only embedding is available, it sends the input `1`.

- Checks only report results; they do not restart or remove replicas from service. No request is sent if no replica is awake and running.
- The gateway excludes checks from request counts, token counts, and TTFT/TPOT metrics. It does not save request records or update the last request time.
  The engine's own metrics may still include the once-per-minute, 1-token check; the gateway cannot subtract it from engine metrics.
- Only the 10 most recent results are retained in memory. New results appear within one minute after the server restarts.

The `health` field in deployment details, deployment lists, and the monitoring service page contains:

| Field | Meaning |
|---|---|
| `status` | `ok` (responded within the threshold), `timeout` (no response within the threshold), `failed` (returned an error, or the gateway could not connect to the replica), `off` (not enabled), `no_replica` (no replica available to check; no request sent), `pending` (enabled but not yet checked) |
| `threshold_sec` | Threshold |
| `last` | Most recent check, `{at_ms, status, latency_ms, http_status?, instance_uuid?, message?}`: `instance_uuid` identifies the replica that responded; `message` is the beginning of the error response or the reason for no response. For timeouts, `latency_ms` is recorded as the threshold. `null` if no check exists |
| `recent` | Up to 10 most recent checks, oldest first |
| `recent_mean_ms`, `recent_ok` | Mean latency and count of checks that responded within the threshold; `null` and `0` if none |

### Automatically Placed Multi-Node and PD Deployments

Create and evaluate requests for multi-node and PD deployments take `placement_mode: "auto"`, and the server picks the nodes for the ranks or units the request leaves out. With it omitted or `manual`, the request supplies the nodes.

Multi-node request (`nnodes` > 1):

```json
{
  "model_id": 3, "nnodes": 2, "placement_mode": "auto",
  "placement": {"tp": 8, "pp": 2},
  "node_uuids": ["<node uuid of rank 0, optional>"]
}
```

- `placement.tp` is required, otherwise 400 `AUTO_PLACEMENT_TP_REQUIRED`; the free cards each node needs are TP × PP ÷ `nnodes`.
- `node_uuids` may be omitted or may name only the first ranks' nodes (more than `nnodes` entries return 400 `DISTRIBUTED_NODE_COUNT`); the rest are filled in from the compute network and GPU architecture those nodes are in. Named nodes that are not in one compute network and one architecture are treated as no group fitting (409 `UNIT_PLACEMENT_NONE`).
- A multi-node deployment still has exactly one replica at creation (`replicas` is 1); scale it afterwards with `PUT /replicas` by replica count (see [Adjust Replicas for Regular Deployments](#普通部署的副本调整)).

PD request (`placement.pd`): a unit may omit `node_uuid`. Units that name a node stay where they are, and the platform fills in only the rest; units placed automatically pin no cards (the scheduler picks `gpus` at launch), and the cards that units already naming a node will use are subtracted first when nodes are picked for the others. When a unit without a node has `followers`, the request returns 400 `AUTO_PLACEMENT_PD_MULTI_NODE` (`params.role`): automatically placed units must fit one node. Adding units through `PUT /deployments/{id}/pd` works the same way, and new units may omit their nodes.

Selection rules (a multi-node request is chosen per replica, a PD request for the whole deployment; only online, schedulable nodes that meet the node requirements and have free cards are considered):

1. The nodes within one replica (the whole deployment for PD) share one compute network and one GPU architecture; when a PD deployment already has units, new units join the network and architecture those are in.
2. Between groups: RDMA networks (host RDMA, accelerator RDMA) before Ethernet, then the group with more nodes that already hold the model file.
3. Within a group: nodes with the model first, then `placement_strategy` (multi-node and PD default to `binpack` when it is omitted): `binpack` takes the node with the fewest free cards that still fit, `spread` the most, and ties go to the lower node ID. For PD under `binpack`, if one node can take every unit they all go on that node; under `spread`, prefill and decode avoid sharing a node where they can.

When no group fits, the request returns 409 `UNIT_PLACEMENT_NONE`, with `params` `nodes` (total nodes), `unmet` (node requirements not met), `cordoned`, `offline`, `busy` (not enough free cards), `no_network` (no usable compute network), and `largest` (how many nodes the largest group has). Evaluation does not answer these failures with an HTTP error; it returns `ok=false` with an entry in `errors[]` whose `field` is `placement_mode`, and the response carries `auto_placed: true`.

A successful evaluation carries `auto_placed: true` and previews the nodes picked now; creation picks again, so the two can differ. Creation does not submit the nodes evaluation selected, so it has none of single-node automatic placement's `AUTOMATIC_PLACEMENT_STALE`; when resources are taken after evaluation, the re-pick when the unit starts handles it. The deployment stores the nodes that were picked (in the multi-node `node_uuids` or in the PD units), and `placement_mode` in the detail is `auto`.

When a unit starts, the controller plans its nodes against the current free cards. If all nodes are online and fit, it starts. If nodes were cordoned, no longer meet the node requirements, or would be refused by scheduling (another deployment holds the cards), only those ranks move to other nodes and `UNIT_PLACED` is recorded (`params`: `unit` (from 1), `role`, `from`, `to` (node names by rank), `reason`). With nowhere to go the unit keeps its nodes and waits, trying again about every minute. Offline nodes belong to node-loss rescheduling and draining nodes to drain, regardless of the placement mode. Manually placed units do not do this; when PD pinned cards are held by other deployments the scheduling reason is `pinned_gpus_unavailable` (`params`: `node`, `gpus`, `holders`).

`PUT /deployments/{id}/placement-mode` switches between automatic and manual; existing deployments are manual.

### Evaluation Requests and Acquisition Estimates

Deployment requests do not select where images or models come from ([ADR 0048](../adr/0048-pull-settings.md)): `model_source` contains only `{storage_id, provider}`. The former is the storage where a node saves the model it acquires; the latter selects the weight provider for the running template (`huggingface` / `modelscope`). `acquisition.nodes[]` provides estimates for the nodes specified in the request (manual placement or multi-node, in rank order). These are estimates only; the node reassesses acquisition when it starts:

| Field | Meaning |
|---|---|
| `image_plan` | `present`: already on the node; `copy`: copy from the `image_copy_from` node in the sync group; `pull`: pull from `image_pull_refs`. The first `image_pull_fixed` entries are alternative addresses from source rules, tried first in order. The remaining entries are the image's own replicas (when `image_pull_ranked` is true, the node orders them by probe latency). If the rules leave the node with no available pull source, `image_pull_error` is included (`{code, params, message}`; code is usually `IMAGE_SOURCE_FORBIDDEN`, and params contains `node` and `route`) |
| `weights_plan` | `present`: already present; `copy`: copy from the `weights_copy_from` node; `storage`: read or cache from shared storage `weights_storage`; `download`: download from `weights_provider` |
| `image_cached`, `image_cache_reason`, `weights_present` | Whether the image is cached (with a reason if unknown) and whether the model is already on the node |

Single-node automatic placement does not provide per-node estimates before the scheduler selects a node; after scheduling, the selected node acquires the assets itself. Automatic placement of multi-node and PD requests is first resolved to the nodes picked now in evaluation, as if those nodes had been written by hand.

### Evaluation Field Errors (`errors[]`) and `ok=false` Semantics

`POST /evaluate` is an interactive endpoint:
validation failures attributable to request fields return HTTP 200 with `ok=false`, and errors appear in the response's
`errors[]` (each entry is `{field, code, params?}`; `code` is a stable ProductError code that maps one-to-one to the error code on the corresponding create
400 path, and `params` contains interpolation parameters for the client's locale). The form retains the draft and displays
errors beside the corresponding controls; `POST /api/v1/deployments` (create) continues to return 400/409. Failures that cannot be attributed to a field
(authentication, model not found, server errors) retain their original status codes. Field path conventions:

- Quick parameters use the dictionary key directly (the form field name), such as `max_model_len` and `gpu_memory_utilization`.
- Request-level fields use their names in the request JSON: `replicas`, `nnodes`, `engine`, `target_vendor`, `extra_args`,
  `quick_params_unset`, `master_port`, `placement`, `placement.tp`, `placement.pp`, `node_requirements`,
  `placement_strategy`, `partition_backend`, `start_timeout_sec`, and `health_check_threshold_sec`.
- `node_uuids` (the multi-node list) maps to the `distributed_nodes` control in the form; when an automatic multi-node request cannot pick nodes or lacks `placement.tp`, the error is attributed to `placement_mode`.
- All additional mount rows map to `container_mount_rows` (the request carries only user-added mounts, without row numbers).
- Docker argument errors map to `container_docker_args`, and shared memory size errors map to `container_shm_gib`.
- **Errors with an empty `field` string do not belong to any control**: failures unrelated to an individual parameter, such as launch assembly or placement simulation failures
  (for example, the node does not have the specified GPU index, or no runtime image is available), use this form. Clients must merge them into page-level
  messages and must not attach them to parameter controls.

The rules cover replica shape (`REPLICAS_NEGATIVE`/`REPLICAS_TOO_MANY`/`DISTRIBUTED_REPLICA_COUNT`),
node requirements (`NODE_REQUIREMENTS_INVALID`/`NODE_REQUIREMENTS_UNMET`), placement strategy (`PLACEMENT_STRATEGY_INVALID`),
vendor (`TARGET_VENDOR_INVALID`), engine (`ENGINE_INVALID`),
extra_args parsing (`EXTRA_ARGS_INVALID`), mounts (`MOUNT_CONTAINER_PATH`/`MOUNT_HOST_PATH`/
`MOUNT_TARGET_DUPLICATE`/`MOUNT_TARGET_NESTED`), JSON parameters (`QUICK_PARAM_JSON_INVALID`),
container path parameters (`QUICK_PARAM_PATH_INVALID`), Docker arguments that duplicate platform-managed settings or dedicated fields
(`DOCKER_ARG_PLATFORM_MANAGED`/`DOCKER_ARG_USE_SHM_FIELD`/`DOCKER_ARG_USE_ENV_FIELD`/
`DOCKER_ARG_USE_MOUNT_FIELD`/`DOCKER_ARG_USE_ENTRYPOINT_FIELD`), setting both shared memory size and `--ipc=host`
(`SHM_SIZE_WITH_HOST_IPC`), shared memory size below the CPU memory tier (`SHM_BELOW_KV_CPU_TIER`), parameter conflicts (`LAUNCH_CONFIG_CONFLICT`/
`LAUNCH_CONFIG_UNSET_CONFLICT`), and numeric bounds
(`FIELD_INVALID`, with English fallback text in `detail`). The response also includes the contract version `schema_version` (currently 1).

In addition to `errors[]`, the evaluation response includes the following fields as needed:

| Field | Meaning |
|---|---|
| `form_values` | Complete values for the form to display: the effective configuration (including dictionary defaults and template projections) and the `extra_args` text after recognized parameters have been moved into structured fields |
| `defaults` | Form defaults: `model_mount` (`/model`), `master_port` (29500), and `engine` (the default engine) |
| `eligible_nodes` | Each node is evaluated against the draft's node requirements (including vendor; the manually selected node itself is not a requirement, because the dropdown is used to select it), the template's GPU predicate, and online status. When `eligible=false`, `reasons[]` provides the reasons (`node_offline`, `node_requirements_unmet`, `vendor_mismatch`, `chip_mismatch`; when the node requirements themselves are invalid, every node has `node_requirements_invalid`, rather than all nodes appearing eligible). The node dropdown renders only this list |
| `weight_sources` | Weight sources declared by the template spec (`provider`/`repo_id`/`revision`/`subfolder`), plus `suggested` (server recommendation) and `registered` (a registration already exists in this cluster, identified by repo id). After selection, the client still calls the registration API |
| `preflight` | Only for `nnodes>1`: request-level preflight results (node count, duplicates, port range, online status, and routable addresses). Deep network probing remains in the separate preflight RPC (retained by the CLI); the frontend no longer orchestrates it |
| `eviction` | Returns candidates when GPU memory is currently insufficient but space can be freed by putting awake deployments to sleep. Each entry includes `instance_uuid`, `deployment_id`, `deployment`, `gpu_indexes`, and `frees_gib` |

The server fills parameter defaults from the current engine dictionary, including engine-specific defaults. Parameters whose dependencies are not enabled receive no default.
Clients use `layout` (`quick`, `expert`, `hidden`; an empty string means `expert`) and `order` from `/engines` to arrange the parameter form.

`quick_params_unset` can contain only keys in the current engine dictionary. If the same request also supplies a value for that key in
`quick_params` or `extra_args`, it returns an unset conflict. The existing `null`
semantics remain unchanged; `null` cannot replace an explicit unset. Composite engine parameters can be removed from a template only when the adapter can recognize them without loss
and fully disable the corresponding configuration. Unknown connectors or mixed configurations return a conflict to avoid accidentally removing other capabilities.

Evaluation in automatic mode returns `selected_node`, per-node `candidates`, and the selected node's `plan`.
When creating the deployment, submit `selected_node.uuid` as `node_uuid`, together with `plan.plan.gpus`
and `plan.plan.tp`, and retain the original constraints, such as labels. Creation revalidates the request. If the original node, GPUs, or constraints are no longer valid,
it returns a conflict requiring reevaluation. This confirm-what-was-evaluated flow applies only to a single node and a single replica; automatic placement of multi-node and PD requests does not use it (see [Automatically Placed Multi-Node and PD Deployments](#自动放置多机与-pd-部署)). Evaluation with an explicit node and multi-replica
evaluation for legacy requests still use `plan_scope=per_replica`; this does not imply that the entire replica group can be allocated successfully.

In template details and evaluation responses, `engine_decides_param_keys` lists dictionary keys for which the current effective launch leaves defaults to the engine
instead of filling them for the user: the launch entrypoint or the engine version in the image may not recognize the flag (see the rules in
[Launch Configuration](../explanation/launch-config.md)). Only keys with dictionary defaults are listed. The form displays them as
“Auto.” You can still explicitly enable or disable them; explicit values are emitted as usual and are not rejected. The engine adapter makes this determination;
it cannot be inferred from whether a template specifies a flag. A user's entrypoint override for a new deployment takes precedence over the template entrypoint. Manual commands are not interpreted as
structured engine parameters.

**Submit parameters according to the selected engine's dictionary.** If a request explicitly supplies a value for a key in
`quick_params` but the selected engine's dictionary has no flag for that key (or the key is a
platform parameter that applies only to other engines), the value is rejected (`invalid quick_params: <key> is not supported by <engine>`).
Evaluation returns HTTP 200, `ok=false`, and a field error; deployment creation and clone configuration diff calculation return 400. The only exception is the key's **neutral default value**, which is equivalent to omitting it, such as
`performance_mode=balanced` for SGLang.

Unsupported explicit combinations include:

- SGLang: `enable_chunked_prefill` (both polarities), `rope_scaling`, multimodal JSON keys
  (`limit_mm_per_prompt`, `mm_processor_kwargs`, `media_io_kwargs`, `allowed_local_media_path`),
  `shared_kv_dir`, `kv_offload_read_threads`/`kv_offload_write_threads`, `kv_disk_limit_gib`,
  `async_scheduling=true`, and all vLLM-specific keys in the `performance_advanced` group.
- vLLM: `log_level` (except `info`; this engine's logging level is controlled through env), `enable_metrics=true`,
  `speculative_num_steps`, and `speculative_eagle_top_k`.
- callable: does not publish `param_specs` and accepts no dictionary parameters other than neutral values.

Scripts and third-party clients cannot directly reuse vLLM parameters for SGLang. `engine_decides_param_keys`
(which only removes defaults based on the entrypoint and engine version) complements this rule: the former does not reject any value; the latter rejects values the **engine** cannot express.


### PD Disaggregation Deployments (`placement.pd`)

```json
{"placement": {"pd": {
  "mode": "vllm-nixl",
  "roles": {
    "prefill": {"tp": 1, "units": [{"node_uuid": "n1", "gpus": [0]}]},
    "decode":  {"tp": 2, "pp": 1, "units": [{"node_uuid": "n2", "followers": [{"node_uuid": "n3"}]}]}
  },
  "kv_load_failure": "fail",
  "fallback": "decode"
}}}
```

- `mode`: one of the engine profile's `pd_modes` (`vllm-nixl`, `vllm-ascend-mooncake`, `sglang-nixl`). All unknown fields are rejected.
- Each `unit` is a replica; `gpus` can be omitted. `followers` makes the replica span multiple nodes (ranks 1…N), in which case the role must specify `tp`.
  For each rank, the GPU count is `tp × pp` divided by the number of ranks.
- Allowed values and defaults for `fallback` and `kv_load_failure` depend on the mode: for `vllm-nixl` and `vllm-ascend-mooncake`, `fallback`
  is `decode` (default) or `fail`, and `kv_load_failure` is `fail` (default) or `recompute`. For `sglang-nixl`, decode cannot
  complete requests on its own, so `fallback` only accepts `fail`, and there is no `kv_load_failure`. Values disallowed by the mode return `PD_FALLBACK_INVALID` /
  `PD_KV_LOAD_FAILURE_INVALID`.
- In `GET /api/v1/engines`, each engine's `pd_modes[]` provides these rules: `name`, `vendors`, `roles`, `scalable_to_zero` (roles that can scale to 0
  while running), `tp_rule`, `max_units_per_role`, `fallbacks`, `kv_load_failures` (the first entry is the default), and `unverified`.
- Specifying it together with `nnodes`, `node_uuids`, or `replicas > 1` returns `PD_SHAPE_CONFLICT`; use `placement_mode: "auto"` with units that omit `node_uuid` for automatic placement (see [Automatically Placed Multi-Node and PD Deployments](#自动放置多机与-pd-部署)). Other validation error codes start with
  `PD_` (field `placement.pd`). When the deployment is sleepable (`sleepable`) and KV uses the host RDMA network through UCX, creation and evaluation return
  `PD_SLEEP_RDMA_UNSUPPORTED` (`params`: `mode`, `network`).
- The `pd` field in deployment details contains `mode`, `fallback`, `kv_load_failure`, `state` (`not_ready|partial|running`), `short`,
  and `roles[]` (`role`, `tp`, `pp`, `desired`, `ready`, `may_be_empty` (the mode allows this role to be empty, meaning it can sleep independently), and `units[]`, including `followers[]`). Instances carry `role`.
  Each unit also has `failed_attempts`, `last_failure`, and `retry_exhausted` (the controller has given up rebuilding it; use the retry endpoint above to retry).
- The `POST /api/v1/evaluate` response also includes `pd.roles[].units[]`: placement, addresses, GPU memory breakdown, and commands for each replica's leader and its `followers[]`.

## Benchmarks

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/benchmarks` | List benchmark tasks | JWT |
| GET | `/api/v1/benchmarks/meta` | Benchmark dictionary (dataset/metric metadata) | JWT |
| POST | `/api/v1/benchmarks` | Create a benchmark task | JWT |
| POST | `/api/v1/benchmarks/probe` | Probe an external benchmark target (connectivity/authentication) | JWT |
| POST | `/api/v1/benchmarks/models` | List models available for benchmarking | JWT |
| POST | `/api/v1/benchmarks/datasets/import` | Import a benchmark dataset | JWT |
| POST | `/api/v1/benchmarks/tokenizers/import` | Import a benchmark tokenizer | JWT |
| GET | `/api/v1/benchmarks/{id}` | Benchmark task details | JWT |
| GET | `/api/v1/benchmarks/{id}/curve` | Benchmark load curve data | JWT |
| GET | `/api/v1/benchmarks/{id}/report` | Download the benchmark report | JWT |
| DELETE | `/api/v1/benchmarks/{id}` | Delete a benchmark task | JWT |
| GET | `/api/v1/benchmarks/{id}/points/{seq}/detail` | Download detailed data for a load point | JWT |
| POST | `/api/v1/benchmarks/{id}/cancel` | Cancel a running benchmark | JWT |

## Profiles / GPUs (Estimation Calibration and GPU Inventory)

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/gpus` | Cluster GPU inventory and latest metrics, using a 20-second snapshot. Supports `page`/`page_size`, `q`, `node`, `vendor`, and `model`. `allocation_state` is `available/partial/allocated/unusable/unknown`, distinct from health level. `q` matches substrings of node names/UUIDs, GPU model names, GPU UUIDs, and PCIe addresses; the other filters accept comma-separated values. For `unusable` reasons, see `schedulable/health_status/ownership/foreign_processes`. `sort` is `node/model/vendor/allocation_state/occupants/utilization/memory/temperature/power/fan/bdf/numa`; paginated results include the full `overview` and `facets`. See [GPU Filters and Allocation States](#gpu-筛选与分配状态) | JWT |
| GET | `/api/v1/dgpus` | dGPU instance inventory | JWT |

### GPU Filters and Allocation States

Without pagination parameters, all results are returned. With `page` or `page_size`, the response is `{items,page,page_size,total,overview,facets}`.
`overview` and `facets{nodes[{uuid,name}],models,vendors}` cover all GPUs and are unaffected by filters.

- `q`: a substring of a node name/UUID, GPU model name, GPU UUID, or PCIe address.
- `node` (node UUID), `vendor`, `model`, and `allocation_state`: multiple values are comma-separated.
- `sort`: `node`, `model`, `vendor`, `allocation_state`, `occupants`, `utilization`, `memory`, `temperature`, `power`, `fan`, `bdf`, or `numa`; entries without readings sort first.

`allocation_state` indicates a GPU's allocation state and must not be treated as a health level:

- `available`, `partial`, `allocated`: distinguished by occupancy.
- `unusable`: accepts no tasks at all; see the GPU's `schedulable`, `health_status`, `ownership`, and `foreign_processes` for reasons.
- `unknown`: the node is offline, and readings are from its last report. `available_bytes` is 0, and the GPU is excluded from the allocatable count. Existing allocations and hardware health information are retained.

`overview.health` counts GPUs by health level: `ok`, `warning`, and `error`. GPUs with warnings may still serve workloads; GPUs passed through or occupied by processes outside the platform may also have health `ok`.
States refresh with the 20-second snapshot.

## API Keys (Gateway Keys)

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/apikeys` | List API keys (without plaintext values), including revoked keys. Revoked keys include `revoked_at` (UTC time); active keys omit this field | JWT |
| POST | `/api/v1/apikeys` | Create an API key. `name` must not be empty after trimming leading and trailing whitespace; otherwise, returns 400 `API_KEY_NAME_REQUIRED`. The plaintext value is returned only once, in the creation response | JWT |
| DELETE | `/api/v1/apikeys/{prefix}` | Revoke an API key by prefix: authentication stops working immediately, but the record remains to identify historical usage; the prefix is not reused. Returns `{"status":"revoked"}`. Revoking an unknown or already revoked key makes no changes and returns no error. After all keys are revoked, the gateway allows anonymous access, as it does when no keys exist | JWT |

## Platform Tokens (Platform API Tokens)

Platform API tokens replace login sessions when calling management APIs and MCP. They are distinct from the gateway keys above, which are used only for `/v1`. Plaintext tokens start with `gwp_` and appear only once, in the creation response. The server stores only SHA-256 hashes. Each user can view and manage only their own tokens, and can do so only within a login session.

After trimming leading and trailing whitespace, a name must be 1–128 UTF-8 bytes (not characters); otherwise, returns 400 `PLATFORM_TOKEN_NAME_INVALID`.

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/platform-tokens` | Current user's tokens: `{items:[{id,name,prefix,scope,created_at,expires_at,last_used_at}], total}`; `last_used_at` updates at most once per minute | JWT (login session) |
| POST | `/api/v1/platform-tokens` | Create a token: `{name, scope?: read\|write, expires_in_days?: 0–3650}` (scope defaults to read; 0 days means no expiration), returns 201 `{token, item}`; invalid `scope` returns 400 `PLATFORM_TOKEN_SCOPE_INVALID` | JWT (login session) |
| DELETE | `/api/v1/platform-tokens/{id}` | Revoke (delete) your own token, effective immediately; returns 204. Returns 404 if the token does not exist or belongs to someone else | JWT (login session) |

## Providers (Cloud Instance Rental)

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/providers/kinds` | Supported cloud provider types, including each provider's credential fields (`credential_fields[].default_value` is an optional creation default for a non-sensitive field; `placeholder` is only a hint), default `server_dial` listening address, `server_dial_supported` (false for AutoDL: Pro instances expose only HTTP ports), and `server_dial_auto_port` (true for Vast and RunPod: the driver automatically chooses the listening port and connection address according to the platform's port mappings) | JWT |
| GET | `/api/v1/providers` | List configured cloud provider accounts | JWT |
| POST | `/api/v1/providers` | Add a cloud provider account | JWT |
| GET | `/api/v1/providers/{id}` | Read back for editing: saved endpoint/public_key and non-secret extra fields declared in the dictionary. Does not fill in provider defaults or return secrets. Credential read failures return 500; the list remains available | Login session (platform tokens and AI/MCP cannot read) |
| PUT | `/api/v1/providers/{id}` | Update a cloud provider account | JWT |
| DELETE | `/api/v1/providers/{id}` | Delete a cloud provider account | JWT |
| POST | `/api/v1/providers/{id}/sync` | Sync instances under this account: insert/update listed instances and remove records for instances no longer present in the cloud (retain associated nodes and set out-of-band recovery to `stopped`). Listing failures return 424 and leave instance records unchanged | JWT |
| POST | `/api/v1/providers/{id}/instances` | Rent a new instance; cloud provider errors return 424 (501 if unsupported) | JWT |
| GET | `/api/v1/providers/{id}/instances` | List instances under this account | JWT |
| DELETE | `/api/v1/providers/{id}/instances/{externalID}` | Release/delete an instance; remove its record after the cloud provider confirms. Cloud provider errors return 424 (501 if unsupported), leaving the instance record unchanged | JWT |
| POST | `/api/v1/providers/{id}/instances/{externalID}/power` | Power on/off; cloud provider errors return 424 (501 if unsupported) | JWT |
| POST | `/api/v1/providers/{id}/instances/{externalID}/resize` | Change the instance specification; cloud provider errors return 424 (501 if unsupported) | JWT |
| POST | `/api/v1/providers/{id}/install` | Install the agent on a rented instance; connection parameters are the same as for node creation. If the Pod needs to restart, returns 409 first and modifies it only after explicit permission. See [Cloud Instance Installation and Restart Confirmation](#云实例安装与重启确认) | JWT |

For cloud provider endpoints, any cloud provider API or instance SSH error returns 424 Failed Dependency, with the reason in the `{error}` body. These endpoints do not use 502/504: CDNs and tunnels such as Cloudflare replace origin 502/504 bodies with their own error pages, losing the reason.

### Cloud Instance Installation and Restart Confirmation

`POST /api/v1/providers/{id}/install` installs the agent on a rented instance. Requirements and error codes for `server_url` are the same as for [node creation](#创建节点的连接参数).

`install_dir` defaults to `/opt/gpuworks` when omitted. An explicit value must be an absolute path beginning with `/` and must not be the root directory; `~` is not expanded. Otherwise, returns 400 `INSTALL_DIR_INVALID` (`dir`) before creating the node. If the specified directory is not writable, installation fails without fallback. The actual installation directory is recorded on the node for out-of-band restarts and reinstallation.

`no_systemd: true` makes the Linux agent use nohup without registering `gpuworks-agent.service`, following the same rules as [SSH Checks and Installation](#ssh-检查与安装).

- Vast and RunPod have `server_dial_auto_port=true`: when using `connect_mode=server_dial` with `server_dial_address` empty, an already mapped TCP port with no listener is selected automatically, and both the listening port and public connection address are saved.
  An explicit `server_dial_listen` takes precedence for port selection, but that port must already be mapped.
- AutoDL has `server_dial_supported=false` and does not support this mode.
- Alibaba Cloud Assistant does not handle `server_dial`; automatic selection uses SSH instead. When Cloud Assistant is online, it adds the platform public key to root's `authorized_keys` before installation through SSH.

When RunPod needs an exposed port or the platform public key, it first returns 409 `INSTANCE_RESTART_REQUIRED`; the Pod has not yet been modified.
`params.need` is `port`, `ssh_key`, or `ssh_key_port`; `agent_dial` may also require the public key to be added.
After confirmation, retry with `allow_instance_restart:true` to modify and restart the Pod. **All ports are renumbered**.
Installation failures return 424 `{error}`, with reasons requiring action, such as SSH login or instance authorization issues.

## Web Terminal

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/nodes/{uuid}/terminal` | Open a web terminal session (WS upgrade; host or container target) | JWT(admin) |
| GET | `/api/v1/nodes/{uuid}/terminal/settings` | Terminal capabilities and limits for this node (kill switch, concurrency limit, etc.) | JWT(admin) |
| GET | `/api/v1/terminal/sessions` | Terminal session audit records (metadata only; terminal content is not retained) | JWT(admin) |

## Networks

See [Configure Node Networks](../how-to/configure-networks.md).

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/networks` | Network list (including attachments, resolved node count, and count of nodes with notices) and role/type dictionaries (`types`: `host_ethernet` / `host_rdma` / `accelerator_rdma` / `accelerator_hccs`; `type_roles`: roles each type can serve). The first entry is the management network (`id` 0, `builtin: true`) | JWT |
| POST | `/api/v1/networks` | Create a network (name, role, type, CIDR, and interface name wildcard). Accelerator RDMA can serve only the compute role; otherwise, returns `NETWORK_TYPE_ROLE_INVALID` | JWT(admin) |
| GET | `/api/v1/networks/{id}` | Network details: attachments, resolved nodes (interfaces/IPs/HCAs/hints and the nodes' user-defined `labels`), and the latest internal test. `id` 0 is the management network, whose nodes include `roles_off`; it cannot be modified, deleted, or attached (returns 404), but internal tests can be started | JWT |
| PATCH | `/api/v1/nodes/management-roles` | Batch-update management network roles: `{"node_uuids":[...],"allow":[...],"deny":[...]}`. Changes only the listed roles, with all nodes in one transaction; returns `{"updated":N}` (the number of nodes actually changed). Returns 404 `MANAGEMENT_ROLES_NODE_NOT_FOUND` if a node does not exist | JWT(admin) |
| PUT | `/api/v1/nodes/{uuid}/management-roles` | Set the roles that this node's management network does not serve (`{"roles_off":[...]}`); returns the node's network view. Applies only to new instances | JWT(admin) |
| PUT | `/api/v1/networks/{id}` | Modify a network | JWT(admin) |
| DELETE | `/api/v1/networks/{id}` | Delete a network and its attachments and test records (running instances are unaffected) | JWT(admin) |
| POST | `/api/v1/networks/{id}/attachments` | Attach to a node (optionally with interface/IP/HCA overrides) or a node label | JWT(admin) |
| PUT | `/api/v1/networks/{id}/attachments/{attachmentID}` | Modify overrides for a node attachment | JWT(admin) |
| DELETE | `/api/v1/networks/{id}/attachments/{attachmentID}` | Remove an attachment | JWT(admin) |
| POST | `/api/v1/networks/{id}/tests/estimate` | Use the same request body without starting a test: returns the resolved node count and node-pair count. Card tests also return `card` (`rounds`, `probes`, maximum duration `seconds`, and limit `max_seconds`). Requests that would be rejected when starting a test are rejected here with the same errors | JWT(admin) |
| POST | `/api/v1/networks/{id}/tests` | Start an internal test (`kind`: tcp / rdma_tool / card_ping / card_matrix; rdma_tool requires a host RDMA network, while card_ping (pings between accelerator cards with matching indices) and card_matrix (a matrix of all cards) require an accelerator RDMA or accelerator HCCS network. HCCS card tests use `hccn_tool -hccs_ping`; RoCE card tests use `-ping`. Returns `NETWORK_TEST_TOO_LONG` if rounds × 3 seconds exceeds 10 minutes. `scope`: all / nodes / label; `id` 0 tests the management network and supports only tcp). Returns 202 and a run record (`node_uuids` lists the tested nodes, fixed when the test starts and available during execution even before `pairs` exists). Executed by the controller | JWT(admin) |

## Settings / Network Presets

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/updates` | Check for updates to the server, image catalog, and template catalog. `refresh=true` forces a new check; without a forced check, `auto_check=false` reads only the previous result. Returns `update_available`; the server also includes `self_upgrade` and `self_upgrade_blocker`. Still returns 200 and `available=false` when no source is available, with `unavailable_reason` set to `unpublished/invalid/unreachable`. See [Update Check Results](#更新检查结果) | JWT(admin) |
| POST | `/api/v1/updates/prefetch` | The login page asks the server to start checking for updates in the background: requires no login, always returns an empty 204 response, and reveals no results. Does nothing if a recent check exists (a success within 1 hour or a failure within 5 minutes), a check is already running, or automatic checks are disabled. The number of outbound checks made by the server therefore stays the same regardless of call frequency | None |
| PUT | `/api/v1/updates/channel` | Switch the update channel with `{"channel":"release"\|"nightly"}`: writes `update_channel` to `server.yaml` and takes effect immediately; returns `{channel}`. Returns 409 `UPDATE_CHANNEL_READ_ONLY` if server.yaml does not exist, or 400 `UPDATE_CHANNEL_INVALID` for other values | JWT(admin) |
| PUT | `/api/v1/updates/auto-check` | Enable or disable automatic checks for new versions with `{"enabled":true\|false}`: writes `auto_check_updates` to `server.yaml` (removes the key when enabled) and takes effect immediately; returns `{enabled}`. When disabled, `prefetch` no longer checks for updates or fetches notices. Automatic checks include the installation ID, server version, and agent version distribution; node counts and GPU models are included only when `usage_report` is enabled (see [Configuration](configuration.md#镜像目录与镜像拉取)). Returns 409 `UPDATE_AUTO_CHECK_READ_ONLY` if server.yaml does not exist, or 400 `UPDATE_AUTO_CHECK_INVALID` if the value is missing or not Boolean | JWT(admin) |
| GET | `/api/v1/usage-report` | User Experience Improvement Program status `{enabled, editable, last_day?, last_attempt?: {at, day, ok, error?}, next_due_at?}`: whether you participate (`usage_report` in `server.yaml`), whether you can change participation in the console, the UTC day covered by the most recently delivered report, this process's latest delivery attempt, and the estimated time of the next report ([User Experience Improvement Program](usage-report.md)) | JWT(admin) |
| PUT | `/api/v1/usage-report/enabled` | Join or leave with `{"enabled":true\|false}`: writes `usage_report` to `server.yaml` (removes the key when joining), takes effect immediately, and returns the status. Returns 409 `USAGE_REPORT_READ_ONLY` if server.yaml does not exist, or 400 `USAGE_REPORT_INVALID` if the value is missing or not Boolean | JWT(admin) |
| GET | `/api/v1/usage-report/preview` | Raw content of the next report (`application/json`, identical to the bytes actually sent, without consuming in-process counters). Returns 404 `USAGE_REPORT_UNAVAILABLE` if the server does not participate in the improvement program | JWT(admin) |
| GET | `/api/v1/notices` | Header notification bell `{notices: [{id, severity, kind, title?, body?, url?, published_at, read}], unread}`: publisher-signed notices applicable to this installation (`kind` is `security` / `release` / `event`; `title`/`body` provide plain text by language). Sorted by urgency, then by time; `read` and `unread` are calculated per caller | JWT |
| POST | `/api/v1/notices/read` | Mark notices as read with `{"ids": [...]}` (1–128 IDs, each ≤64 bytes; otherwise, 400 `NOTICE_IDS_INVALID`). Records only the caller's own read status; repeated marking has no side effects. Returns 204 | JWT |
| POST | `/api/v1/updates/server` | Download a release package, back up and replace the server, and restart. On acceptance, returns 202 `{from,to,backup}`; confirms or rolls back on the next startup. See [Server Upgrade Restrictions](#服务端升级限制) | JWT(admin) |
| POST | `/api/v1/updates/server/upload` | Same as above, but the request body is an uploaded release archive (512 MiB limit). Returns 400 `SERVER_UPGRADE_ARCHIVE_INVALID` if it is not a usable release package (including versions predating console upgrades: they cannot confirm or roll back their own startup, so `detail` advises using `gpuworks upgrade --archive` instead) | JWT(admin) |
| GET | `/api/v1/settings` | Current values of all settings (excluding `telemetry_export` and `branding`, which are read and written only through their dedicated endpoints) | JWT |
| PUT | `/api/v1/settings/{key}` | Modify a single setting; `telemetry_export` and `branding` return 400 `SETTING_HAS_OWN_ROUTE`. Only administrators can modify `deployment` (pending scheduling timeout, instance startup timeout, and event retention; otherwise, 403 `ADMINISTRATOR_REQUIRED`). The request body overlays the current values and changes only the keys it contains. If the merged values are out of range, returns 400 `FIELD_INVALID` without writing | JWT(admin) |
| GET | `/api/v1/security/settings` | Security switch: `playground_preview_network` (whether the playground HTML preview can make network requests; defaults to `false`) | JWT |
| PATCH | `/api/v1/security/settings` | Modify the switch above, changing only fields present in the body | admin |
| GET | `/api/v1/branding` | Branding: `product_name`, `console_title`, `login_description`, and `logo` (a base64 `data:` URL, PNG/JPEG/WebP/SVG). All four fields are always present; an empty string uses the built-in default (the client renders default text in the UI language). No login is required because the login page must display these before login | None |
| PATCH | `/api/v1/branding` | Modify branding, changing only fields present in the body; an empty string restores that field's default. After trimming leading and trailing whitespace, `product_name` allows up to 32 characters, `console_title` up to 64, and `login_description` up to 200 (only this field allows line breaks); control characters are prohibited. The decoded `logo` is limited to 128 KiB. Invalid input returns 400 `FIELD_INVALID` (`field`, `detail`) without writing. Returns all saved values | admin |
| GET | `/api/v1/telemetry/export` | OTLP export configuration and status for gateway traces: `enabled/endpoint/headers/sample_ratio/trace_link`; only header names are returned. `status` includes cumulative `exported/dropped/failed` since server startup, current `queued`, the latest error `last_error`, and the configuration error `setting_error`. See [Trace Export Settings and Status](#trace-导出设置与状态) | JWT |
| PUT | `/api/v1/telemetry/export` | Save the entire configuration and apply it immediately. Unlisted headers are deleted; missing or `null` header values retain their stored values. For fields and errors, see [Trace Export Settings and Status](#trace-导出设置与状态) | JWT(admin) |
| POST | `/api/v1/telemetry/export/test` | Send a test span named `gpuworks test span` using the draft settings in the request body (same as PUT; does not save and does not require `enabled`). Sends only once, without retries: 200 `{ok, trace_id, traces_url, error}`. If `ok=false`, `error` contains the Collector's response or a connection error. Invalid draft settings return the same 400 errors as PUT | JWT(admin) |
| GET | `/api/v1/pull/source-rules` | Source rules in application order: `{id, position, kind, match, endpoints[], allow_upstream, control_plane, note, node_requirements, matched_nodes}`. `kind` is derived from `match`: a value with `http://`/`https://` is a site rule `url` (a model site such as `https://huggingface.co`); otherwise, it is an image rule `image`. `control_plane` makes the control plane also use this rule to resolve addresses when fetching model metadata (available only for site rules) | JWT |
| POST | `/api/v1/pull/source-rules` | Create a source rule. `allow_upstream` is required; `control_plane` defaults to `false`. For a site rule, `match` is the site's root address (without a path), and endpoints are http(s) addresses that can include paths. Invalid rules return 400 `PULL_SETTING_INVALID` (`field`: `match`, `endpoints`, `control_plane`, `note`, `requirements`) | JWT(admin) |
| PUT | `/api/v1/pull/source-rules/{id}` | Replace an entire rule | JWT(admin) |
| PUT | `/api/v1/pull/source-rules/order` | Reorder: `{"ids":[...]}` lists every rule exactly once | JWT(admin) |
| DELETE | `/api/v1/pull/source-rules/{id}` | Delete a rule | JWT(admin) |
| POST | `/api/v1/pull/source-rules/test` | Test a site rule using a draft `{match,endpoints,allow_upstream,node_uuid,repo}` without saving. `repo` defaults to `Qwen/Qwen3-0.6B`; returns the status and elapsed time for each address. See [Source Rule Connectivity Test](#来源规则连通测试) | JWT(admin) |
| GET | `/api/v1/pull/sync-groups` | Sync group list: `{id, name, node_requirements, rate_limit_kib_s, matched_nodes}` | JWT |
| POST | `/api/v1/pull/sync-groups` | Create a sync group (node conditions select members; rate limiting is optional) | JWT(admin) |
| PUT | `/api/v1/pull/sync-groups/{id}` | Replace an entire sync group | JWT(admin) |
| DELETE | `/api/v1/pull/sync-groups/{id}` | Delete a sync group | JWT(admin) |
| GET | `/api/v1/pull/metadata-sources` | `?source=` takes `huggingface` or `modelscope`: addresses the console browser uses to read model metadata, consisting of candidates from site rules with “Used by the management node for model metadata” selected (in order, with the original site last unless the rule disables upstream fallback): `{source, bases[]}`. `bases` is empty if rules prohibit every address for the site. Other `source` values return 400 `FIELD_INVALID` | JWT |
| GET | `/api/v1/pull/proxies` | Proxy entries in matching order (a node uses the first entry that selects it): `{id, position, http_proxy, https_proxy, no_proxy, username, has_password, control_plane, note, node_requirements, matched_nodes}`. Addresses contain no login credentials, and passwords are never returned. `control_plane` routes the control plane's outbound requests through this proxy too (uses the first selected entry) | JWT |
| POST | `/api/v1/pull/proxies` | Create a proxy entry: `{http_proxy, https_proxy, no_proxy, username, password, control_plane, note, node_requirements}`, with at least one proxy address. Use scheme://host:port (`http`/`https`/`socks5`/`socks5h`; defaults to `http` if omitted); including login credentials in the address returns 400. `no_proxy` uses the same syntax as the NO_PROXY environment variable. Passwords are stored encrypted. Invalid input returns 400 `PULL_SETTING_INVALID`. After saving, each online node receives one update containing only the network portion | JWT(admin) |
| PUT | `/api/v1/pull/proxies/{id}` | Replace an entire proxy entry; omit `password` to retain the stored value, or supply an empty string to clear it | JWT(admin) |
| PUT | `/api/v1/pull/proxies/order` | Reorder: `{"ids":[...]}` lists every entry exactly once | JWT(admin) |
| DELETE | `/api/v1/pull/proxies/{id}` | Delete a proxy entry | JWT(admin) |
| GET | `/api/v1/pull/credentials` | Credentials (one entry per host): `{id, host, username, has_secret}`. Image registries use a username and password; model sites use only a token (`username` is empty, and the token is sent as `Authorization: Bearer` for downloads and metadata retrieval). Passwords and tokens are never returned | JWT(admin) |
| POST | `/api/v1/pull/credentials` | Create credentials (`host`, `username`, `secret`), stored encrypted. `host` is normalized to a host without a scheme or path (Docker Hub variants normalize to `docker.io`), with one entry per host | JWT(admin) |
| PUT | `/api/v1/pull/credentials/{id}` | Replace credentials; an empty `secret` retains the stored value | JWT(admin) |
| DELETE | `/api/v1/pull/credentials/{id}` | Delete credentials | JWT(admin) |
| POST | `/api/v1/pull/credentials/{id}/test` | Test credentials from the control plane (through its outbound proxy): credentials with a username log in to an image registry at `/v2/`; token-only credentials are validated with Hugging Face's `GET /api/whoami-v2`. Returns 200 `{ok, message}`; rejection returns 422 `CREDENTIAL_LOGIN_REJECTED` (`host`), and connection failure returns 502 `CREDENTIAL_LOGIN_FAILED` (`host`, `detail`) | JWT(admin) |
| GET | `/api/v1/pull/registry-credentials` | Removed: this route and its `POST`, `PUT/DELETE {id}`, and `POST {id}/test` return 410 `SETTING_REMOVED` (`route` is `/api/v1/pull/credentials`) | JWT(admin) |
| GET | `/api/v1/server/listen` | Server listen addresses: saved `console`/`agent`/`gateway` addresses, currently open `running` sockets, `restart_pending` (saved addresses require a restart to take effect), `config_path`, and `editable`/`overridden` (not editable if the server starts without server.yaml or is overridden by `--addr`) | JWT(admin) |

### Update Check Results

`GET /api/v1/updates` queries release sources concurrently and uses the first index whose signature passes verification. Results for the same channel are reused for 1 hour; `refresh=true` checks again immediately.

- `channel`, `channel_editable`: the current channel and whether switching is allowed.
- `auto_check`: the automatic check switch. When `false`, returns only the result of the last manual check; if no check has ever run, returns an empty response without `sources` or `unavailable_reason`. A forced refresh still performs a check.
- Source information: the winning source and `{url,ok,latency_ms,error}` for sources that have responded. Slow sources that have not responded are omitted; the UI does not display source details.
- `server`, `image_catalog`, `template_catalog`: each includes `current`, `latest`, and `update_available`. Templates also include `compatible`; the server also includes `installable`, `self_upgrade`, `self_upgrade_blocker`, and `install_dir` (the installation directory containing `server.yaml`; omitted when there is no such directory).

The server marks `update_available` only when the channel version is newer. The two catalogs must also differ from the current content and have a release date no earlier than the current build, to avoid marking old catalogs as updates.
When no source is available, the response is still 200 with `available=false`; `unavailable_reason` distinguishes the cases:

- `unpublished`: a source is reachable, but the channel has no index.
- `invalid`: content fails signature verification or parsing, for example, because a login gateway intercepts it.
- `unreachable`: no source responds, for example, due to network, proxy, DNS, timeout, or server errors.

### Server Upgrade Restrictions

`POST /api/v1/updates/server` downloads the release package for the current platform, validates it against the signed index, backs up and replaces the server, and restarts. Returns 202 `{from,to,backup}`; confirms or rolls back on the next startup.

- 409 `SERVER_UPGRADE_UNAVAILABLE`: self-upgrade is unavailable; `reason` is `no_service_manager`, `not_installed`, `not_installed_binary`, or `not_writable`.
- 409 `SERVER_ALREADY_AT_RELEASE`: already at the target version; 409 `SERVER_NEWER_THAN_RELEASE`: the current build is newer than the channel version.
- 404 `UPDATE_NOT_PUBLISHED`: the channel has no package for the current platform.
- 502 `UPDATE_DOWNLOAD_FAILED`: no source provides a package matching the size and SHA-256 in the index. `file` identifies the file, and `detail` gives each source's failure reason; the running service is not replaced.

### Trace Export Settings and Status

`GET /api/v1/telemetry/export`

OTLP export settings and runtime status for gateway request traces: `enabled`, `endpoint` (the Collector's OTLP/HTTP address), `headers` (lists only `{name}`; values are never returned), `sample_ratio` (sampling ratio for traces initiated by the gateway, 0..1, defaulting to 1 if never set), and `trace_link` (a link to an external tracing system containing `{trace_id}`; can be empty).

`status`: `active`, `traces_url` (the actual destination address), cumulative `exported`/`dropped` (discarded because the queue is full)/`failed` (rejected or delivery failed) since server startup, `queued`, `last_error` and `last_error_at`, `last_export_at` (Unix milliseconds, or `null` if absent), and `setting_error` (why stored settings cannot be applied, such as decryption failure).

`PUT /api/v1/telemetry/export`: save all settings and apply them immediately: `{enabled, endpoint, headers:[{name, value}], sample_ratio, trace_link}`. A header without `value` (or with `null`) retains the stored value for the same name; stored headers not listed are deleted. Omitted `sample_ratio` defaults to 1. Header values are stored encrypted.

An invalid address returns 400 `TELEMETRY_ENDPOINT_INVALID`; an invalid, duplicate, or exporter-managed header name returns 400 `TELEMETRY_HEADER_INVALID` (`name`); a new header without a value returns 400 `TELEMETRY_HEADER_VALUE_REQUIRED` (`name`); an out-of-range ratio returns 400 `TELEMETRY_SAMPLE_RATIO_INVALID`; a link that is not an http(s) address containing `{trace_id}` returns 400 `TELEMETRY_TRACE_LINK_INVALID`. Unknown fields return 400.

### Source Rule Connectivity Test

`POST /api/v1/pull/source-rules/test` accepts `{match,endpoints,allow_upstream,node_uuid,repo}` to test a draft without saving it.
Each call targets one node; when you select 1–5 nodes in the console, it makes separate calls in parallel. `repo` defaults to `Qwen/Qwen3-0.6B`.

Probes use the same proxy, `no_proxy`, and credentials as actual downloads and access each address in parallel, with a 10-second timeout per address:

- Hugging Face-compatible sites: first fetch `/api/models/<repo>/revision/main`, then read the first byte of `/<repo>/resolve/main/config.json`; gated repositories may reject access only on the file request.
- `modelscope.cn`: access `/api/v1/models/<repo>`.

Returns `{source,repo,node_uuid,node_name,outcomes:[{base,status,code,latency_ms,error}]}`.
`status` is `ok`, `auth_required` (401/403; credentials required), `http_error`, or `unreachable`.
Invalid drafts return 400 `PULL_SETTING_INVALID`; a nonexistent node returns 404, and an offline node returns 409 `NODE_OFFLINE`.
If a node cannot execute the probe, returns 502 `SOURCE_PROBE_FAILED` (`detail`), for example, because the agent version is too old.

### Removed Settings Endpoints

`PUT /api/v1/settings/image_registry_rewrites`, `image_registry_mirrors`, and image acceleration preset settings return 410 `SETTING_REMOVED`; image sources are now expressed through `/pull/source-rules` (in the table above in this section). `PUT /api/v1/settings/network` and `network:<node-id>` also return 410 `SETTING_REMOVED` (`route` is `/api/v1/pull/proxies`): proxies are now entries selected by node labels, and URL rewrites are now site rules. During upgrades, old global and per-node settings are converted automatically (global rewrites become site rules without node restrictions that apply to the control plane; node overrides become rules and proxy entries restricted to that node and placed first, preserving each node's effective proxy). Network acceleration presets (`GET /api/v1/network/presets`) have been removed.

## Metrics / Logs

### Logs, Metrics, and Event Timelines

Logs and event timelines use cursors to query earlier records, while metrics use time ranges; the `page` pagination described above does not apply:

| Endpoint | Queries and Pagination |
|---|---|
| `GET /api/v1/logs/search` | `limit` defaults to 200, with a maximum of 5000; `cursor_ts` and `cursor_id` mark the pagination position; `with_total=1` includes the total number of matches |
| `GET /api/v1/metrics/query` | `range` is the query duration and `step` is the sampling interval, both in Go duration strings, defaulting to `6h` and `1m`, respectively. Filter by labels such as `node`, `gpu`, `gpu_uuid`, `instance`, `benchmark`, and `point` |

`GET /api/v1/instances/{uuid}/events` and `GET /api/v1/deployments/{id}/events` return historical event timelines,
sorted newest first, with the response `{"items": [...]}`. `limit` defaults to 100, with a maximum of 500; for the next page, pass
`before=<last-id-on-previous-page>`. An invalid cursor returns `400 EVENT_CURSOR_INVALID`; an invalid page size returns `400 EVENT_LIMIT_INVALID`.

Timelines **merge consecutive duplicate events when read**: for consecutive records within the same instance that have identical `kind`, `code`, `state`, and `phase`,
only the latest record is returned. Large numbers of repeated progress reports in older databases therefore appear as fewer state milestones.
Records with `code` carry detailed values in `params` and no longer also return the raw `data`; historical records from before the code mechanism still include `data`.


| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/metrics/query` | Query time-series metrics (`metric`, `range`, `step`, and `node`/`gpu`/`gpu_uuid`/`instance`/`benchmark`/`point`/`iface`/`hca`/`role` matchers). Returns `{"items": [{"labels": {…}, "points": [[unix_ms, value], …]}], "total"}`, with one series per label combination. If no data exists and the metric name is absent from the metric catalog (`GET /api/v1/monitor/metrics`), returns 400 `METRIC_UNKNOWN`; `params.similar` provides the closest registered names. A metric in the catalog with no data in the time window returns an empty result normally | JWT |
| GET | `/api/v1/logs/search` | Search/tail logs (`q`, `instance` prefix, `instances` (a comma-separated set of exact UUID matches, up to 64, mutually exclusive with `instance`), `node`, `level`, `source`, `from`/`to`, `cursor_ts`/`cursor_id`, `limit`, `with_total`; paginated in reverse arrival order. An omitted time boundary leaves that direction unrestricted; when both are omitted, queries all logs that have not been cleaned up. The list and total use the same range; by default, the page explicitly passes the last 24h). Returns `{"items": [{"id", "instance", "node", "level", "source", "time_ms", "line"}], "total", "total_capped", "truncated"}` (the last three fields appear as needed) | JWT |
| GET | `/api/v1/logs/context` | Context window around a single match (`instance`, `ts`, `id`, and `before`/`after` from 0–500), equivalent to grep -C. Returns `{"items": [log entries], "match_index"}` | JWT |
| GET | `/api/v1/logs/histogram` | Counts by time bucket × level (same filters as search, including `instances`, plus `bucket` in milliseconds, 1s–24h). Returns `{"items": [{"start_ms", "counts": {"error": N, …}}], "total"}`; buckets align to UTC | JWT |
| GET | `/api/v1/monitor/overview` | Cluster snapshot for the monitoring center (`range` ∈ `15m\|1h\|6h\|24h\|168h`, default `1h`, or a custom `from`+`to` range as described below; signal rows report only facts—values, details, and recent trends, without severity; service table includes each deployment's `health` check, sorted by health: failed, timed out, no available replicas, checking, healthy, disabled, then by name within each category; GPU memory bar, cluster tok/s, and capacity events) | JWT |
| GET | `/api/v1/monitor/deployments/{id}` | Service monitoring snapshot; `range` is `15m/1h/6h/24h/168h` (default `1h`), or use `from`+`to`. Includes requests, latency, replicas, `health`, `log_instances` for log queries, and metric `sources` (engine/gateway); PD also includes fallback, Prefill statistics, and `kv_transfer` under `pd`. Unknown deployments return 404; deleted deployments return 404 `MONITOR_DEPLOYMENT_DELETED` with `deployment_id`, `name`, and `served_name` in `params`. See [Service Monitoring Response](#服务监控返回值) | JWT |
| GET | `/api/v1/monitor/deployments/{id}/requests` | Per-request view: `view=live` (default) or `history`; `limit` is 1–500 (default 50 for live), with optional `key` and `instance` filters. History also supports `range` or paired `from`+`to` (Unix milliseconds), `cursor`, and `min_ms/max_ms`; `results` accepts `completed/length/client_closed/interrupted/error`, with multiple values separated by commas. Records include `result/segments/pd/trace_id`; paginate with `next_cursor`. See [Per-Request Queries](#逐请求查询) | JWT |
| GET | `/api/v1/monitor/requests/lookup` | Find finished requests across deployments; specify exactly one of `trace_id` (32 hexadecimal characters) or `request_id`. Optional paired `from`+`to` (Unix milliseconds, up to 24 hours), and `limit` of 1–500 (default 100); returns `requests/deployments/window`. Without a time range, locates requests by the gateway trace time or searches the last hour; requests discarded by sampling cannot be found. See [Find Requests Across Deployments](#跨部署查找请求) | JWT |
| GET | `/api/v1/monitor/deployments/{id}/latency` | End-to-end latency, stage durations, and generation speeds for complete responses within `range`: `requests` is the count; `end_to_end` and `segments` include `mean_ms/p50_ms/p95_ms`; `speeds` includes `prefill_tok_s/thinking_tok_s/answering_tok_s/decode_tok_s`. Unaffected by log sampling. See [Latency Aggregation Rules](#延迟汇总口径) | JWT |
| GET | `/api/v1/monitor/distributions` | Distribution of requests that finish within `range`: `{x, y, n}` for two grids, `input_output` (input bucket × output bucket) and `uncached_ttft` (uncached input bucket × time-to-first-token bucket); `token_buckets` gives bucket lower bounds and `ttft_bounds_ms` gives time-to-first-token bucket upper bounds. `coverage` gives the total request count, count with usage, and count reporting cache hits; `deployment` is optional and defaults to the entire cluster; unknown deployments return 404 | JWT |
| GET | `/api/v1/monitor/metrics` | Metric catalog; returns `{items:[{series,name,source,kind,unit,quantiles,labels,description}],total}`. `source` is `engine`, `gateway`, `gpu`, `dgpu`, `node`, `network`, `kv_disk`, or `benchmark`; inference histograms expand into quantile gauges (`quantiles:true`), `_sum`, and `_count`. See the [Inference Metric Glossary](inference-metrics.md) for field definitions | JWT |
| GET | `/api/v1/monitor/series` | Metric browser (`metric` required; `range` ∈ `15m\|1h\|6h\|24h\|168h`, default `1h`, or `from`+`to` (`step` defaults to approximately 240 points, in multiples of 10 seconds); `by` ∈ `node\|deployment\|instance\|gpu`; `fn` ∈ `avg\|sum\|max\|rate`, default `avg`; `step` defaults to a value derived from `range` and cannot be lower than that value; returns 400 if results exceed the series/point limits; unknown metrics return `[]`). Series with a `node` label also carry a display-only `node_name`, and series with a `deployment` label carry `deployment_name`; for a deleted node or deployment these are the names it had when deleted, with `node_deleted` / `deployment_deleted` set to `"true"`; labels neither side recognizes are left unchanged | JWT |

Custom time ranges for monitoring charts: `overview`, `deployments/{id}`, `deployments/{id}/latency`, `distributions`, and `series` all accept paired `from`+`to` values (Unix milliseconds), mutually exclusive with `range`. The interval must be between 1 minute and 30 days, and `to` cannot be in the future; otherwise, returns 400 `MONITOR_RANGE_INVALID` (`detail`). Charts, distributions, latency breakdowns, and signals are calculated for the interval ending at `to`, without caching. Current state, including service lists, replicas, and health checks, still reflects the present.

### Service Monitoring Response

`GET /api/v1/monitor/deployments/{id}` returns request reconciliation, a time-to-first-token breakdown, charts, and replica information.
`range` supports `15m`, `1h` (default), `6h`, `24h`, and `168h`; alternatively, use paired `from`+`to` values. Unknown deployments return 404; deleted deployments return 404 `MONITOR_DEPLOYMENT_DELETED` (`params`: `deployment_id`, `name`, `served_name`).

- `log_instances`: UUIDs of live replicas and replicas recently stopped within the window, up to 64; usable with `/logs/search?instances=...`.
- `sources`: indicates whether `ttft`, `e2e`, `prompt_tokens`, `generation_tokens`, and `running` come from `engine` or `gateway`; signals without data are omitted. See [Inference Metrics](inference-metrics.md#网关指标) for definitions.
- `health`: same as the [health check](#健康检查) in deployment details.

PD deployments also return `pd`:

- `effectiveness_pct`, `fallback_rate_pct`, `fallbacks` (counts by fallback reason), `prefill_waiting_avg`, and `avg_prompt_tokens`.
- `prefill_stage_avg_ms` and `prefill_stage_p95_ms`: mean and P95 time the gateway waits for prefill, both over the last 5 minutes; P95 comes from the request segment histogram.
- `kv_transfer.exposed`: whether the engine exports KV transfer metrics; for `vllm-ascend-mooncake`, this is `false`, and all other transfer fields are `null`.
- Other `kv_transfer` fields: per-transfer `avg_ms`, `p95_ms`, and `avg_mb`; transfer speed `mb_per_s`; and counts of `failed` (decode pull failure) and `expired` (prefill KV lease expires without being pulled) within the window. Values are `null` when no data is available.

### Find Requests Across Deployments

For `GET /api/v1/monitor/requests/lookup`, specify exactly one of `trace_id` or `request_id`.
A trace ID is 32 hexadecimal characters, case-insensitive, and queries all requests in that trace; a request ID queries the corresponding request.
`limit` is 1–500, default 100; the time range is determined in this order:

1. If you provide `from` and `to`, queries use the received time. Provide both together (Unix milliseconds, no more than 24 hours apart).
2. If no range is specified and the gateway generated the trace, first search from 10 minutes before to 1 minute after the trace time; if no matches are found, search the last hour.
3. Otherwise, search the last hour.

Returns `{as_of,from_ms,to_ms,window,requests,deployments}`.
`window` identifies the window source: `requested`, `trace_time`, or `default`.
`requests` has the same structure as the per-request view, ordered by received time, newest first; `deployments` contains `{id,name,deleted}` for the associated deployments: `name` is the served model name; a deleted deployment has the name it had when deleted and `deleted: true`; old deployments deleted before names were kept have an empty name.
Normal requests discarded by sampling because they exceed the per-second log budget cannot be found. Invalid parameters return 400.

### Per-Request Queries

For `GET /api/v1/monitor/deployments/{id}/requests`, select a `view`:

- `live` (default): requests in progress, ordered by longest wait for the first token, then thinking, then answering. `counts` gives the `waiting`, `thinking`, and `answering` counts for all in-flight requests.
- `history`: finished requests returned newest first; pass `next_cursor` as `cursor` for the next page.
  Use `range` to query a period ending now, or provide paired `from` and `to` (Unix milliseconds).
  The response's `from_ms` and `to_ms` give the actual range; `range_total` is the total number of requests finished at the gateway within that range, counted in 5-minute buckets and unaffected by filters or log sampling.

Both views support `limit` (1–500, default 50 for live), `key` (a single API key), and `instance` (a single replica).
History also supports end-to-end duration filters `min_ms` and `max_ms`, and comma-separated `results`:
`completed` for normal completion, `length` for length truncation, `client_closed` for client exit, `interrupted` for connection interruption, and `error` for errors.
Unknown deployments return 404; invalid parameters or cursors return 400.

Key fields in each record mean the following:

- `result` is the request outcome; `segments[{kind,ms}]` gives stage durations, which sum to the end-to-end duration or elapsed in-flight duration.
  See [Request Stages](request-timeline.md#2-阶段) for stage names, boundaries, and calculations.
- `pd` has a value only for PD requests and is `null` otherwise. It includes `prefill_instance_uuid`, `prefill_ms`, `prefill_queue_ms`, `prefill_compute_ms`, and `fallback`;
  on fallback, the prefill instance is empty, and unreported durations are `null`. With concurrent handoff, `prefill_ms` is `null`; with sequential handoff, it is also `null` while the request is still in prefill. Once forwarded to decode, this stage duration can be returned even if the request has not yet finished.
  Requests still in prefill appear in the live view with `dispatch_ms` set to `null`. See [PD Requests](request-timeline.md#6-pd-分离部署) for details.
- `trace_id` is a 32-character hexadecimal W3C trace ID: it inherits a valid `traceparent` from the caller, or is generated by the gateway otherwise. `deployment_id` identifies the deployment, and `weight` is the sampling weight.
- `received_ms` is an absolute Unix timestamp in milliseconds; all other timestamps are relative to when the gateway receives the request. Events that have not occurred or were not observed have `null` values. See [Timestamp Definitions](request-timeline.md#1-记录了哪些时间点).

### Latency Aggregation Rules

`GET /api/v1/monitor/deployments/{id}/latency` aggregates complete responses within `range` (normal completion or length truncation):

- `requests`: number of complete responses; `end_to_end{mean_ms,p50_ms,p95_ms}`: end-to-end duration, or `null` when there are no requests.
- `segments[]`: ordered by stage; each entry contains `kind`, `mean_ms`, `requests`, `p50_ms`, and `p95_ms`.
  Means include all complete responses, treating an absent stage as 0, so the stage means sum to the end-to-end mean; counts and quantiles include only requests with that stage, and quantiles use quarter-octave buckets.
- `speeds`: `prefill_tok_s`, `thinking_tok_s`, `answering_tok_s`, and `decode_tok_s`, calculated as total qualifying tokens divided by total duration, or `null` when no data is available.

Statistics are unaffected by log sampling; see [Latency and Speed Calculation Rules](request-timeline.md#4-汇总端到端延迟拆解与速度) for inclusion criteria.
Unknown deployments return 404; invalid `range` values return 400.

## Analytics (Usage Analysis)

For long-term statistics and data retention, see [ADR 0055](../adr/0055-long-term-statistics.md).

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/data-retention` | Data retention settings: `{value, limits, server_time_zone}`. Fields in `value`: `metrics_hours` and `logs_hours` (raw metric samples and container logs); `request_log_days` and `request_log_max_mb` (per-request records in ux.db, cleaned up when either the age or capacity limit is reached; the current day is retained); `usage_hourly_days` and `usage_daily_days` (token usage in the main database); `stats_hourly_days` and `stats_daily_days` (service quality, GPU utilization, and GPU-hours in stats.db); the `quality`, `gpu`, and `gpu_time` switches; and the statistics time zone, `time_zone` (an IANA name). `limits` gives `{min, max}` for each numeric field | JWT |
| PUT | `/api/v1/data-retention` | Replace all settings and apply them immediately: update the ux.db limits and run cleanup and daily aggregation once. The body must include every field in `value`, with no extra fields. Values outside `limits`, a daily retention period shorter than hourly retention, or an unknown time zone return 400 `FIELD_INVALID` (`field`, `detail`) without saving. Data outside shortened retention periods is deleted in background batches. Returns the same response as GET. `PUT /settings/data_retention` returns 400 `SETTING_HAS_OWN_ROUTE`; `GET /settings` excludes this setting | admin |
| GET | `/api/v1/data-retention/status` | Current data status: `stores[]` includes `kind` (`metrics`/`logs`/`requests`/`usage`/`quality`/`gpu`/`gpu_time`), `file` (the containing file; `main` means the main database), `file_bytes` (file size; `null` for the main database; `quality`, `gpu`, and `gpu_time` share stats.db and report the same size), `hourly_rows`/`daily_rows` (long-term statistics row counts; `null` for raw data), and `earliest_ms` (earliest queryable time). Also returns `live_since_ms` (when live accumulation began; earlier data comes from the one-time upgrade backfill), `backfill` (`usage_done`/`usage_through_ms`/`quality_done`/`quality_through_ms` for backfill progress), `retain`/`rollup` (`at_ms` and `error` for the latest cleanup and daily aggregation), and `usage_flush` (`at_ms`, `rows`, `ms`, and `error` for the latest token usage write) | JWT |

Statistics queries require JWT authentication. The following time range parameters and `range` response fields apply to `usage`, `quality`, `gpus`, and `gpu-time`. The `key-usage` endpoint reports today and the last 7 days; it does not require `from` or `to` and does not return `range`. Common parameters for the four range-based endpoints are `from` and `to` (required, Unix milliseconds), and `granularity` (`hour` or `day`). When omitted, granularity is hourly for ranges of up to 2 days and daily for longer ranges. Hourly queries support at most 7 days; daily queries support at most 1825 days. Daily ranges expand to whole days in the statistics time zone. Aggregated days use daily data; days not yet aggregated (today, or days that elapsed while the server was stopped) are calculated from hourly data.

Each range-based response includes `range`: `from_ms`, `to_ms`, `granularity`, `time_zone`, `earliest_ms` (the earliest retained bucket at this granularity), `live_since_ms`, and `buckets` (bucket start times, corresponding one-to-one with `series`). Buckets before `live_since_ms` come from the one-time upgrade backfill: sampled rows are weighted, and historical `/v1/messages` and `/v1/responses` requests have no token counts because the gateway did not recognize their usage at that time. Invalid parameters return 400 `ANALYTICS_RANGE_INVALID`, `ANALYTICS_RANGE_TOO_LONG`, or `ANALYTICS_VALUE_INVALID` (`field`, `detail`).

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/analytics/usage` | Token usage. Filters: `deployment` (deployment ID), `key` (API key ID; `none` for requests without a key), and `source` (`api` or `playground`). `totals` and `series[]` contain counts: `requests`, `no_usage` (requests without usage), `prompt_tokens` (total input), `completion_tokens`, `reasoning_tokens` (reported by `reasoning_reported` requests), and `cached_tokens` (cache hits reported by `cache_reported` requests; hit rate = `cached_tokens` ÷ `cache_reported_prompt`). `rows[]` has one row per key × user × deployment × source, with `api_key_name`/`api_key_prefix`/`api_key_revoked`/`api_key_deleted` (keys deleted before revocation began retaining records), `user_name`, `deployment_name`/`served_name`/`deployment_deleted`, `source`, and `first_ms`/`last_ms` (first and last buckets used) | JWT |
| GET | `/api/v1/analytics/quality` | Service quality; optional `deployment` filter. `totals`, `series[]`, and `rows[]` (by deployment) include result counts (`completed`, `length`, `client_closed`, `interrupted`, `error`), `ttft_p50_ms`/`ttft_p95_ms` (time to first token for complete streaming responses), `e2e_p50_ms`/`e2e_p95_ms` (end-to-end time for complete responses), and `output_tokens_per_sec`. Quantiles are reconstructed from quarter-octave buckets, with approximately 10% error; missing values are `null` | JWT |
| GET | `/api/v1/analytics/gpus` | GPU utilization. `group_by` is `model` (default), `node`, or `gpu` and determines table and heatmap rows. `metric` is `util` (default), `power`, `temp`, or `mem` and selects the heatmap value, averaged within each bucket. Filters: repeatable `label=key=value` (same matching as the node list, against current user and system labels; deleted nodes no longer match) and `model` (GPU model). `card_count` and `by_model[]{model, cards}` give the current cards covered by the filters. With at most 256 cards, `detail: true` and `cards[]` has one row per card: `node_name`, `node_deleted` (the node is deleted; `node_name` is the name it had when deleted), `gpu_index`, `gpu_uuid`, `gpu_model`, `present` (still in the inventory, including when the node is offline), `deployments` (currently using it), `util_avg`/`util_max`, `mem_used_avg_bytes`/`mem_used_max_bytes`/`mem_total_bytes`, `power_avg_watts`, and `temp_avg_celsius`/`temp_max_celsius`. Above 256 cards, `detail: false` and `cards` is empty. Cards of nodes deleted within the time range are listed too. When grouped by model or node, `groups[]` has one row per group: `key`, `label`, `node_deleted` (when grouped by node, a deleted node; its `cards` and `nodes` are 0), `cards`, `nodes`, `models[]`, `deployments[]{name, cards}`, `unassigned` (cards not allocated to a deployment), and the same statistics, taken from node/model aggregates. `heat[]` contains `{key, label, node_deleted, values[]}`; when grouping more than 100 nodes, it includes only the 50 highest and 50 lowest nodes for the selected metric, while `heat_total` gives the total row count. Grouping by card with more than 256 cards returns 400 `ANALYTICS_TOO_MANY_CARDS` | JWT |
| GET | `/api/v1/analytics/gpu-time` | GPU-hours; optional `deployment` filter. `rows[]` has one row per deployment × GPU model: `gpu_hours` (dGPU usage weighted by its memory share), `whole_gpu_hours`, `avg_instances` (average replica count up to now), `max_instances`, `prompt_tokens`/`completion_tokens` for the deployment in the same period, and `gpu_hours_per_million_output`/`gpu_hours_per_million_tokens` (`null` without token data). `series[]` gives each deployment's GPU-hours per bucket. `capacity[]` gives each GPU model's `total_gpu_hours` (platform-usable cards on online nodes), `schedulable_gpu_hours`, and `allocated_gpu_hours` | JWT |
| GET | `/api/v1/analytics/key-usage` | Counts for each API key with usage today and in the last 7 days, including today: `{items:[{prefix, today, last_7_days}], time_zone}`. Used by the API Tokens page | JWT |

## AI Helper (AI Assistant)

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/ai/configs` | List AI Helper LLM connection configurations | JWT |
| POST | `/api/v1/ai/configs` | Add an LLM connection configuration. `base_url` is filled up to `/v1`: trailing slashes and a pasted `/chat/completions` are dropped before storing, and an address that is not http(s) or has no host returns 400 `FIELD_INVALID` with `params.field` `base_url`. Without a `base_url`, a known `preset` supplies one; with neither, the configuration is still stored and the AI Helper reports it as unusable | JWT |
| PATCH | `/api/v1/ai/configs/{id}` | Partially update an LLM connection configuration: only the `name`/`preset`/`base_url`/`model`/`is_default` in the request change. A missing `api_key` or an empty string keeps the stored key (including a legacy plaintext one); a non-empty one is encrypted and stored. Setting the default clears it on the others. Answers the same row the list shows (six fields, no key). 404 for a missing configuration, 400 `FIELD_INVALID` for an invalid value | JWT |
| DELETE | `/api/v1/ai/configs/{id}` | Delete an LLM connection configuration | JWT |
| POST | `/api/v1/ai/configs/test` | Try the unsaved `base_url`/`api_key`/`model` a form holds, storing nothing. A 10 second timeout, through the same outbound policy the assistant uses. Answers `{outcome, status, detail, base_url, latency_ms}`, where `outcome` separates `ok`, `connection` (DNS, dial or timeout), `blocked` (refused by the outbound policy), `auth` (401/403), `path` (a 404 that names no model: the wrong address), `model` (the endpoint does not serve that model) and `http` (any other status, or not a completion). `detail` is the endpoint's own words, shortened and with the key removed. An edit dialog sends its `id` with an empty `api_key` to reuse the stored key, only while `base_url` still matches the stored address | JWT |
| GET | `/api/v1/ai/available` | Whether AI Helper is available (whether an LLM is configured) | JWT |
| POST | `/api/v1/ai/ask` | Ask AI Helper to perform a task; supports regular responses and SSE. See [AI Ask Request and Response](#ai-提问的请求与响应) for all fields and conditions | JWT |
| GET | `/api/v1/ai/instances/{uuid}/findings` | Known failures matched in a failed instance's logs: `{items:[{code, evidence:[{line}]}], total}`; `code` is stable, and the frontend renders the text. No LLM required; returns 404 if the instance does not exist | JWT |
| GET | `/api/v1/ai/models/{id}/fit` | Physical fit of the model on each accelerator card type in the online cluster; does not represent currently free resources. Returns `weight_bytes`, `dtype`, and `rows` per card type: `cards/unusable` (usable/faulty card counts), `dtype_supported`, `fast_link_cards` (maximum number of cards in a high-speed interconnect group such as NVLink), `fits`, minimum `tp`, and remaining `headroom_bytes`; no LLM required; returns 404 if the model does not exist. See [Model Fit Results](#模型适配结果) | JWT |

### Model Fit Results

`GET /api/v1/ai/models/{id}/fit` evaluates the physical capabilities of each card type in the online cluster, not currently free resources; no LLM is required, and a nonexistent model returns 404.
Returns the model's `model_id`, `weight_bytes`, `dtype`, `gpu_memory_utilization`, and `rows[]`:

- Card type and capacity: `vendor`, `name`, `memory_bytes`, `nodes`, `cards` (usable card count), `unusable` (faulty card count based on UI health levels), and `max_cards_per_node`.
- Precision and interconnect: `dtype_supported` indicates whether the card supports computing at the model's precision, with optional explanation in `dtype_note`; `fast_link_cards` is the maximum number of cards in a high-speed interconnect group within a single node, where 1 means PCIe only and 0 means unknown. Up to 8 nodes are sampled for each card type.
- Placement estimate: `fits`, `tp`, `per_gpu_bytes`, `usable_bytes`, and `headroom_bytes`.
  `tp` is the smallest power of 2 needed to fit the weights and engine reservation within a single node; if they do not fit, `fits:false` and `tp:0`.

### AI Ask Request and Response

`POST /api/v1/ai/ask` asks AI Helper to perform a task. The `task` values and target fields are:

| `task` | Target Specified in the Request |
|---|---|
| `instance.diagnose` | `instance_uuid` |
| `model.fit` | `model_id` |
| `deployment.unscheduled` | `deployment_id` |
| `node.health` | `node_uuid` |
| `ask` | Requires `question`; optionally include one of the targets above |

Specify targets by reference; the server assembles the context. `question` is optional except for the `ask` task, where it is required. For follow-up questions, include `history:[{role:user|assistant,content}]`, keeping at most the latest 10 rounds.

After assembling the context, the server runs an agent loop with read-only tools. The tools read this API's GET endpoints in-process as the user asking the question; see [Use AI Helper](../how-to/use-ai-helper.md). A regular response is `{answer, steps:[{tool,args,ms,error?}], calls, usage:{prompt_tokens,completion_tokens}}`.

With the `Accept: text/event-stream` request header, the response uses SSE streaming:

- `step`: `{id,tool,args,status:running|done|error,ms,error?}`, one event when each tool call starts and one when it ends.
- `text` / `reasoning`: `{delta}`.
- The final event is `done` (`{calls,usage,first_text_ms,total_ms}`) or `error` (`{error}`).
- When idle, sends a `: ping` comment every 10s.

Failures before the model call, such as an unconfigured LLM or a nonexistent target, still return HTTP status codes. Error conditions are:

| Condition | Response |
|---|---|
| No LLM configured | 503 `AI_ENDPOINT_NOT_CONFIGURED` |
| Invalid `task` | 400 `AI_TASK_INVALID` |
| Missing target or multiple targets provided | 400 `AI_TARGET_REQUIRED` |
| The same user already has 2 questions being processed | 429 `AI_ASK_BUSY`; `params.limit` gives the limit |
| Instance, model, deployment, or node does not exist | 404 |
| LLM endpoint failure | 502 |

### MCP

| Method | Path | Description | Authentication |
|---|---|---|---|
| POST | `/api/v1/mcp` | MCP service (Streamable HTTP, JSON responses only, no SSE; `GET` returns 405). JSON-RPC 2.0, single messages; supports `initialize`, `ping`, `tools/list`, `tools/call`, and notifications (returning 202). Tools are the same read-only tool set as AI Helper, reading this API as the caller with the same redaction and response shaping; tool failures return a result with `isError: true`. Each user can have at most 4 concurrent tool calls. Returns 403 `MCP_DISABLED` when the service is disabled. See [Connect Through MCP](../how-to/use-mcp.md) | JWT or platform token (`read` is sufficient) |
| GET | `/api/v1/mcp/status` | MCP service status: `{enabled, path, protocol_version, tools}` | JWT |
| PUT | `/api/v1/mcp/settings` | Enable or disable the MCP service: `{enabled}`; returns the new status; disabled by default | JWT(admin) |

Deterministic estimator outputs remain fully available without an LLM configured; `ai/ask` is available only after an LLM is configured; MCP does not require an LLM.

## Playground

| Method | Path | Description | Authentication |
|---|---|---|---|
| GET | `/api/v1/playground/models` | List deployed models callable in Playground, excluding external AI Helper models in global settings; `html_preview.allow_network` is the HTML preview network policy (from security settings) | JWT |
| GET | `/api/v1/playground/openapi.json` | OpenAPI description of the Playground proxy | JWT |
| ANY | `/api/v1/playground/v1/*` | Internal Playground proxy (protected by management JWT, not a `/v1` gateway key) | JWT |

For the boundary between Playground and the `/v1` gateway, and external model naming `external:<config-id>:<model>`, see
[`openai-gateway.md`](openai-gateway.md).

## Out of Scope

- For data plane `/v1/*` endpoints (OpenAI-compatible chat/embeddings/rerank/audio/image/video/realtime endpoints, etc.), see
  [`openai-gateway.md`](openai-gateway.md); they are not repeated here.
- Agent-side yamux subprotocols (`ctrl`/`metrics`/`logtail`/`tunnel`/`model.read`/`model.write`/
  `image.read`/`image.write`/`terminal`) are not HTTP routes. They belong to the `internal/proto` control plane protocol and are outside this document's scope.

## Route and Contract Maintenance

The following conventions maintain the endpoint inventory; they are not prerequisites for calling the API:

- Management routes are registered by `internal/server/api.Router` using the chi router.
  `TestRouterInventoryMatchesCompleteContract` in `internal/server/api/router_inventory_test.go` asserts each entry in the actual route tree;
  update this test whenever you add or remove a route.
- Management authentication is checked by `managementAuth` in `internal/server/api/auth.go`; administrator operations also pass through `managementAdmin`.
  Product errors use `messages.ProductError`; 130+ stable error codes are defined in `internal/server/api/errors.go` and are not listed individually here.
- `make check-api-conventions` checks field naming and list structures at the type level; see [API Conventions](api-conventions.md).
- During production assembly, `internal/server/app` always injects non-nil
  `ProxyV1`, `ProxyModels`, and `PlaygroundV1` handlers for `/v1/*`, `/v1/models`, and `/api/v1/playground/v1/*`. Route inventory tests use zero-value `Deps{}`, so these three gateway routes
  and the SPA fallback `/*` are excluded from their assertions, but are always mounted in production.
