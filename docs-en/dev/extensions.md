# Extension Points: The Module Contract and Installable Extensions

Inside the core, the gpuworks server composes modules through one set of interfaces (`app.Module` and `app.Host`). Tests and forks use this contract today: another Go module can use the server as a library and compile it, together with its own modules, into a new server binary (a "separately built server" below); official builds carry no module. Official and third-party extensions are to be installed as signed plugin packages and connected through the same contract by a built-in plugin host module ([ADR 0071](../adr/0071-installable-extensions.md)). The plugin host is not built yet; see [Installable extensions](#installable-extensions).

This page describes the interfaces the open-source code leaves, the thread each one is called on, the rules they must follow, and what the open-source code does not do. The irreversible decisions are in [ADR 0070](../adr/0070-extension-tiers-and-edition-boundary.md) and ADR 0071; the terms (extension module, feature key, feature availability, extension-held cordon, plugin package, pushed table) are defined in `CONTEXT.md`.

Extension docs, examples and tests describe each mechanism in its own terms; sample publishers, editions and plugins use the fictional name `acme`, as sample domains use `example.com`.

## Tiers

| Tier | Runs where | Carries |
|---|---|---|
| T0 Core | server, agent | Every general capability and every enforcement point: all decisions taken per request, insert, status report, claim or placement live here; the community edition itself, free by definition |
| T1 Built-in module | inside the server process, in Go | Core-internal composition through `app.Module` / `app.Host`; `cli.Options.Extensions` stays for tests and forks and is not a distribution path for vendors or third parties |
| T2 Package server part | a child process of the server | Decisions that need I/O, state, workflows or integrations, pushed as tables the core enforces (not built yet) |
| T3 Package node part | core workloads and jobs on nodes | Products that use GPUs; no plugin code runs in the agent (not built yet) |
| T4 Policy part | inside the server | Reserved only, not built |

Every interface on this page belongs to T1. The design of T2–T4 is in ADR 0071.

## How a build composes

A separately built server (a fork or a test) has its own `main`, which calls `cli.Main` once:

```go
func main() {
	os.Exit(cli.Main(os.Args[1:], cli.Options{
		Edition:      "acme",
		Extensions:   app.Extensions{Modules: []app.Module{audit.New(), sso.New()}, UI: console.FS()},
		Distribution: distribution.Profile{Edition: "acme", Sources: sources, Keys: keys},
		InstallHooks: []selfinstall.InstallHook{devices.Hook{}},
		Commands:     []cli.Command{statusCommand},
	}))
}
```

The zero value of `cli.Options` is the community server that `cmd/gpuworks` runs:

- `Edition` is written to `version.Edition` and is for display only: `gpuworks edition`, `/healthz`, the build info, the usage report and the console manifest read it. Open-source code never branches on it (the `edition_branches` budget is 0).
- `Extensions.Modules` go to `serve`. `migrate`, `reset-password`, `support-bundle` and extra commands also open the database with the modules' migrations (extra commands use `cli.Env.OpenMaintenanceStore`).
- `Extensions.UI` replaces the embedded community console and has the same layout as `internal/server/webui/dist`; when it is nil the embedded build is served.
- `Distribution` is the build's own distribution channel: update checks, notices, catalog updates and upgrades read it, and an index signed for another edition is refused. Its edition must equal `Edition`, or the binary panics at startup.
- `InstallHooks` run in order on `install` and `upgrade` and in reverse on `uninstall`, for host steps that need root, such as device permissions.

Official builds pass the zero value. The edition scoping of `Edition` and `Distribution` serves forks only, and the official release path no longer extends it (ADR 0071).

## Modules and the Host

```go
type Module interface{ Name() string }                     // [a-z][a-z0-9_]{1,31}
type Migrator interface{ Migrations() store.MigrationSet } // optional
type Initializer interface{ Init(ctx context.Context, h *app.Host) error }
type Service interface{ Run(ctx context.Context) error }
```

A module's name is its only identity: its `extensions.<name>` section in `server.yaml`, its migration namespace and the `module` label on its log records all use it. An invalid or repeated name, or a migration set whose namespace is not the module's name, stops the server before the database is opened.

`Init` runs once, in `app.New`, in module order, after the API dependencies are built and before the router. A module reaches the server only through the `Host`:

| Method | Purpose |
|---|---|
| `Log()` / `Bus()` / `DataDir()` / `InstallID()` | A logger with `module=<name>`, the event bus, the data directory, the installation ID |
| `Section(v)` | Strict decode of `extensions.<name>` in `server.yaml`; without the section `v` stays as it is |
| `Tables()` | The module's own tables (`store.ExtensionTables`); nil for a module without a migration set |
| `DeriveKey(purpose)` | A 32-byte key derived from the master key with HKDF-SHA256 and bound to the module name |
| `MountAPI(e)` | Mounts an `api.Extension`'s routes under `/api/v1/x/<name>/` |
| `DeclareFeatures(...)` / `ProvideFeatures(src)` | Declares feature keys; provides the feature source (at most one across all modules) |
| `AddBannerSource(b)` | Banners at the top of every console page |
| `GuardNodeInsert(g)` | A guard on every new node's insert |
| `HoldScheduling(h)` | A scheduling holder, keyed by its source; a repeated source is refused |
| `ReapplySchedulingHolds()` | The holder's answers changed: asks for the holds to be applied to every node again, rather than at each node's next status report |
| `NodeFleet(ctx)` | Every node's hold facts (`store.NodeHoldFacts`), ordered by ID (enrollment order), in one query on the read pool |
| `NodeIdentity(ctx, ids, salt)` | The salted hardware identity of the named nodes (`store.NodeIdentity`); raw serials never leave the core. See [Node identity and node inventory](#node-identity-and-node-inventory) |
| `NodeInventory(ctx, cursor, limit)` | A page of the node inventory in enrollment order (`store.NodeInventoryPage`): names, enrollment times, each node's own cards (index, model, whether it has a serial) |

The `Host` hands out neither the `*store.Store` nor a database handle. A registration after `Init` returns is refused (`ReapplySchedulingHolds`, `NodeFleet`, `NodeIdentity` and `NodeInventory` are not registrations and may be called at any time): methods with an error result return `app.ErrHostClosed`, the others panic. If `Init` fails, `New` fails and closes the database, so the same data directory starts again at once.

Extension routes live under `/api/v1/x/<name>/`: `api.RouteSpec.Pattern` is relative to `/api/v1` and is `/x/<name>` or starts with `/x/<name>/`, and is a clean path (no `.`, `..`, `//` or trailing `/`). A route outside the module's own namespace, an unclean pattern, or two patterns of one method that differ only in parameter names (`{id}` and `{name}`) panic while the router is built. The core registers no route under `/api/v1/x/`, so a module name may equal a core path segment (such as `nodes`) and never collides with a path the core adds later.

After every `Init` has run, the server releases extension-held cordons that no module of this build owns (so they do not outlive a changed build or a removed module), applies the holders' answers to every node once if any holder is registered, then builds the feature gate (`featuregate.New`) and hands it to the API.

## Reapplying holds and the node fleet

A holder is otherwise asked on every status report, so a node whose answer changed can wait up to a minute to be cordoned or released. A module whose answers changed (for example, a rebuilt snapshot holds a different set of nodes) calls `ReapplySchedulingHolds()`, and the server runs `ApplySchedulingHolds` over every node again:

- It never blocks and allocates nothing, and may be called from any goroutine, **except from inside a hook** (`SchedulingHolder.Hold`, an insert guard): a pass asks every holder again, so a holder that asked for one from `Hold` would keep the whole fleet in passes. Hooks request no other recompute either.
- Requests are coalesced: any number made while a pass is pending or running make at most one more pass, and passes are at least 2 seconds apart, so a holder whose answers flap costs at most one fleet-wide writer transaction every 2 seconds, never one per event.
- The pass runs in the same `services()` loop as the modules' `Service`, off the caller's goroutine, and stops with the server; a failed pass is logged as an error and retried with a backoff that starts at 2 seconds and doubles up to a minute. With no holder registered it does nothing.

A holder whose answers depend on the whole fleet (for example, choosing nodes in enrollment order) learns the fleet from these two, rather than by recording the nodes it sees in `Hold`:

- `NodeFleet(ctx)`: every node's hold facts, in one query on the read pool. It is for boot and for a recompute after the fleet changed, coalesced; never per report or per event, and never from inside a hook. Subscribe to the topic below before the first read, so no change between them is missed.
- The bus topic `store.NodeFleetTopic`: one event for each node insert (all three insert paths), each delete, and each status report that changed the node's hold facts (its card count or vendor; its ID, UUID, enrollment time and Kubernetes cluster never change), keyed by the node's ID. Read it with `store.NodeFleetChangeOf(ev)`, which gives `{NodeID, Kind}` (`NodeInserted`, `NodeDeleted`, `NodeFactsChanged`); do not parse the reason text of the `node` topic. A routine report that changed no hold fact publishes nothing, so publishers cost what they did. Like every bus topic, a subscriber that falls behind gets only each node's latest event, so it treats any event as "this node, and so the fleet, changed" and recomputes from `NodeFleet`, coalesced, rather than counting events. A node ID is never reused after its node is deleted.

`Service.Run` is started by the same `services()` loop in `App.Run` as the health checks, card failure records, usage report and notice refresh, after the controller starts. It returns when `ctx` is done; an error it returns is logged and the service is not restarted. One App can be `Run` more than once (an in-place restart), so `Run` must tolerate being called again.

## Node identity and node inventory

A module that needs to recognize "the same machine, the same cards" (an inventory export, a support bundle) reads salted digests through the `Host`, never raw serials:

- **Host system UUID**: the agent reports `host_system_uuid` in the hello of every handshake: the firmware system UUID (`/sys/class/dmi/id/product_uuid` on Linux, `IOPlatformUUID` on macOS), normalized by `proto.NormalizeHostSystemUUID` (lower case; placeholders, all zeros, all F and malformed values dropped). On Linux only root can read that file, so a non-root agent reports none; older agents report none either and connect as before. The server normalizes it again on receipt and writes `nodes.host_system_uuid` only when it differs from the node's row, so a routine reconnect adds no write; an empty value never clears one already recorded. It describes the machine, not the node: several logical nodes on one machine report the same value.
- **Card serials**: a card's id is the `GPUInfo.UUID` its probe reported. `accelerator.CardSerial` tells a serial the hardware carries, which survives a move to another slot or host, from an id the probe made up from a slot or an index; only serials take part in identity:

  | Vendor | Reported id | Stable |
  |---|---|---|
  | NVIDIA, PPU, Kunlunxin | The card UUID from the vendor CLI | Yes |
  | AMD | `amd-<unique_id>` (the ASIC serial) | Yes; `amd-<PCI address>` when the driver has no `unique_id`, no |
  | Hygon | `GPU-<unique id>` (hy-smi or sysfs) | Yes; `hygon-<PCI address>` or `hygon-<index>` without one, no |
  | Ascend | `ascend-<index>` | No (npu-smi's table gives no serial) |
  | Apple | `APPLE-<IOPlatformUUID>` | Yes, and the same machine as the host system UUID |
  | Fake probers | `GPU-FAKE-<n>`, `FAKE-<vendor>-<n>` | No (test values, the same on every node) |

- `NodeIdentity(ctx, ids, salt)` answers `{NodeID, Host, Cards, CardsWithoutSerial}` for each named node that still exists, in ascending ID order:
  - Each item of `Host` and `Cards` is a `store.IdentityPart{H, Tail}`. `H` is the first 16 hex characters (8 bytes) of HMAC-SHA256 keyed with the caller's salt over the kind, a zero byte and the raw value, the kind being `host` or `card`; `Tail` is the raw value's last 4 characters, for a person to check against the machine, and is empty when the raw value is shorter than 8 characters, so a tail is never the whole value. One salt gives the same digests every time; different salts give unrelated `H` values. `Tail` is not salted: every caller sees the same raw tail.
  - `Cards` holds only the cards the node may use (`accelerator.CardUsable`) that carry a serial, ordered by `H`; usable cards whose id was made up are only counted in `CardsWithoutSerial`. So two logical nodes on one machine share `Host` and each has only its own cards.
  - Partial answers: `Host` is the zero value (`Known()` is false) until the agent reports a host UUID; a node that does not exist (never did, or was deleted) is left out of the result, so the caller compares IDs; a repeated ID counts once.
  - `ids` holds 1 to 500 nodes, else `store.ErrNodeReadBatch`; the salt is 1 to 256 bytes, else `store.ErrIdentitySalt`. A call out of bounds is refused, never cut short.
- `NodeInventory(ctx, cursor, limit)` returns one page `store.NodeInventoryPage{Nodes, Next}` in ID (enrollment) order: each node's ID, UUID, display name (the name an operator gave it, otherwise its hostname), enrollment time, `HostReported` (whether its host UUID is known) and the cards it may use as `{Index, Model, Serial}`, with no raw serial. Pass 0 as `cursor` for the first page and the previous page's `Next` after that; a `Next` of 0 means this was the last page. `limit` is 1 to 500, else `store.ErrNodeReadBatch`. Node IDs are never reused, so a node enrolled or deleted between pages neither shifts nor repeats the others.
- Scale: both are pinned reads, two indexed queries each on the read pool (nodes by primary-key lookup or range, cards through the `gpus` `(node_id, ident)` index), at most 500 nodes per call. Call `NodeIdentity` for the nodes a page shows, not for the whole fleet on every event. Identity changes (a new host UUID, a swapped card) are not published on `store.NodeFleetTopic`; a module that must follow them reads again when it reads the page.

## Tables and migrations

Extension migrations serve compiled modules only (forks and tests). Plugin packages are to use storage in their own data directory, and the tables the core persists for plugins are core tables; neither uses this mechanism (ADR 0071).

Migration sets (`store.MigrationSet`) run after the core migrations, in the same `store.OpenWith`:

- Each step and its marker are written to `namespace_migrations` in one transaction. The table is created only when sets are passed; a community open never creates it and writes no schema.
- Every table, index, view and trigger a step creates, changes or drops must carry the set's prefix in its name and in the table it belongs to. Core tables never use the reserved prefix `ee_`.
- A namespace in the database that this build does not know only logs a warning and does not stop startup. A known namespace newer than the binary stops it, and so does a recorded step that is missing from the binary or renamed.
- Core sweeps over every table may reach extension tables, so extension tables store times in UTC and their migrations only add.

`ExtensionTables.Write` runs one transaction on the single writer connection; `Read` runs one read transaction on the read pool (an in-memory database has no read pool and reads on the writer). An extension reads and writes only its own prefixed tables, and reaches core data only through Store methods, the API and the hooks below.

## Hooks and call threads

| Hook | Called where | How often |
|---|---|---|
| `NodeEnrollmentGuard` | Inside the writer transaction of the three node insert paths (`CreateNodePending`, `EnrollKubeNode`, `RegisterNode`) | Every new node insert |
| `SchedulingHolder.Hold` | Inside the writer transaction of every status report and scheduling change; for every node at startup and in every `ApplySchedulingHolds` (including the passes modules ask for) | Every report of every node |
| `featuregate.Source.Availability` | Every request to an extension route, every console manifest request | Every request |
| `BannerSource.Banners` | Every console manifest request | Every request |
| `Service.Run` | A background loop | Continuously |

When an insert guard refuses, the API answers 403 with the guard's code. The instance rental and provider install paths run a pre-check that takes no writer lock first, so no machine is rented only to be refused. A node a holder holds is cordoned and never drained; an operator cannot make it schedulable (409 with the holder's code), but can still cordon or drain it.

## Purity rules

There is a single writer connection (`SetMaxOpenConns(1)` in `store/database_open.go`). A hook called inside a writer transaction (insert guards, scheduling holders) that calls the Store, `Tables()` or anything waiting on them waits for the connection it holds itself: a deadlock. These hooks therefore must:

- not call the Store or `Tables()`, do no I/O, and take no contended lock;
- be O(1): one atomic load plus one map lookup, with no allocation when they do not hold;
- answer from a snapshot the module maintains elsewhere (a `Service`, a bus subscription, an API write), and call `ReapplySchedulingHolds` there when the snapshot changes;
- return a prebuilt `*messages.ProductError` when they refuse.

Feature sources and banner sources are called on request threads and likewise answer from snapshots, never scanning nodes, deployments or tables. The contract tests in `testenv/extmodule` show what breaking the rule does: a guard that writes its own table cannot proceed until the insert transaction ends.

## Availability

Open-source code speaks only of availability: a feature source answers each feature key with full, read-only, locked or hidden, optionally with limits, a link and a coded notice.

- An extension route (`api.RouteSpec`) either names a `Feature` or is marked `Community`, exactly one of the two. The route inventory (`api.RouteInventory`) prints each extension route's access level, action and feature.
- Full: passes. Read-only: reads pass, and so do writes marked `WriteWhenReadOnly` (writes that only remove or relax); other writes get 403 `FEATURE_READ_ONLY`. Locked: 403 `FEATURE_UNAVAILABLE`. Hidden, or a locked feature declared with concealed presentation: 404. When the source supplies its own notice, its code is returned.
- Without a module that provides a feature source, every declared feature is locked (`FEATURE_PROVIDER_ABSENT`).
- Community paths never consult the gate, and running workloads are never interrupted by a change of availability.
- Extension GET routes are not readable by the AI assistant and MCP unless marked `AIReadable`.

The console renders `GET /api/v1/console/manifest` (fields in the [API reference](../reference/api.md)) and derives no availability itself: full shows as usual; read-only passes `readOnly` to the component; locked keeps the menu entry with a lock and shows the locked page with the server's link; a hidden key, or one the manifest does not list, shows nothing.

## Console overlay

The build-time overlay serves forks only; plugin package UIs are to be loaded at runtime (ADR 0071, not built yet).

A separately built server's UI modules are copied to `ui/src/extensions/<id>/` (git-ignored), registered in `ui/src/extensions/index.ts`, and may import only the components, hooks and API client that `ui/src/extensions/sdk.ts` re-exports (the `ee_ui_internal_imports` budget is 0). The build sets `GPUWORKS_UI_OUT_DIR` to write elsewhere; with UI modules checked out and the variable unset, `make ui` and `make release-packages` refuse to build, so nothing is overlaid onto the community edition. The modules' texts ship in their own locale bundles merged into `common`, checked by the same i18n check through `GPUWORKS_I18N_EXTRA_LOCALES` / `GPUWORKS_I18N_EXTRA_MESSAGES`.

The entries a UI module can add:

- Pages (`routes`), side menu entries (`navItems`, which need an icon), settings tabs (`settingsTabs`) and top bar controls (`headerItems`).
- Account menu entries: a `navItems` entry whose `group` is the reserved `account` goes to the account menu at the top right, after the built-in entries and before signing out, ordered by extension id, as text only. The account menu has no room for a locked entry, so a locked one is left out like a hidden one. `account` cannot be the id of a new menu group.
- Admin-only: each kind of entry above can be marked `admin: true` and then follows the role of the user the session check read. A user who is not an administrator sees no menu entry, tab or control, and the page answers as not found; a session whose user could not be read is treated the same way. This shapes only what is shown: the server's route access levels still guard the API.

Banners: a banner whose link opens the current page (query and fragment aside) is left out on that page, which states the matter itself.

The SDK carries a major version, `SDK_MAJOR` (1 for now), which goes up only when an export is removed or changes meaning; adding an export leaves it alone, and `ui/tests/consoleExtensionsSdk.test.ts` pins the export list. Files go through the SDK's `downloadFile`, `saveBlob` and `sendForm`:

- `downloadFile` asks with the session and names the file from the answer's `Content-Disposition`, or else from the name the caller gives. An error answer decodes by its code into a localized `APIError`; the error body is never saved as the file.
- `saveBlob` hands a Blob to the browser as a download.
- `sendForm` sends a multipart form and decodes errors as `sendJSON` does.

A UI module's tests are copied to `ui/tests/ext-<id>/` (git-ignored) and run with `UI_EXTRA_TESTS='tests/ext-<id>/*.test.tsx' npm test`. They sit outside `ui/src/extensions/<id>/`, so their imports of `node:test` and testing-library do not count against the import budget, while the `c8 --all --include='src/**'` of `npm run test:coverage` still counts every source file of the module.

## Codes and texts

Failures and refusals an extension produces follow [ADR 0067](../adr/0067-runtime-outcomes-as-codes.md) too: UPPER_SNAKE codes, interpolated params, an English fallback. An extension's codes and their en/zh texts live in the extension's own code and UI modules, not in the open-source `messages` or locale files; without the overlay, the console shows the English fallback.

## Upgrades and installation

The edition checks below matter only to forks: official builds name no edition, and ADR 0071 removes this machinery from the official release path once the first availability authority runs as a plugin.

- A distribution index names its edition inside the signed payload; an index whose edition does not match the build's channel is refused, and the community edition accepts only indexes that name no edition or `community`.
- `upgrade` compares `gpuworks edition` of the installed binary and the staged one and refuses a different edition unless given `--allow-edition-change`; a console upgrade to another edition answers 409 `SERVER_EDITION_MISMATCH`.
- `upgrade` runs the install hooks of the binary doing the upgrade; after a cross-grade, run `install` with the new binary as prompted.
- Going back from an extended build to the community edition, the `extensions:` section of `server.yaml` only logs a warning, the extension tables are left as they are, and extension-held cordons are released at startup.

## Installable extensions

ADR 0071 decides that official and third-party extensions are installed as signed plugin packages (`.gwpkg`) instead of being compiled into the binary. The plugin host module is not built yet; the points below say how it will connect to the contract on this page:

- The plugin host is a built-in module that registers the existing hooks through the `Host` (feature source, node insert guard, scheduling holder, banners) and answers them from tables plugins push and the core validates and persists; in official builds it will be the only caller of `ProvideFeatures`.
- A plugin is a child process of the server that talks to the core over `gwx/1` on inherited fds, never enters a hot path and never gets the Store; its routes live under `/api/v1/x/<id>/`, the same prefix as compiled modules.
- Whether a feature key is gated is declared by the plugin package itself; grant tables accept pushes only from the authority its publisher certificate names; while a plugin is down its restrictions stay enforced, its grants clamp to read-only after a bounded grace, and timelines keep firing on extclock.
- The hook rules on this page (hooks inside the writer transaction are pure, holders cordon without draining, running workloads are never interrupted) apply to the plugin host as well.

The wire formats of manifests, certificates, trust updates, `gwx/1` messages and pushed tables are frozen in `pkg/extapi`; see [Plugin Protocol and Wire Formats](../reference/plugin-protocol.md). The sections below describe the process and protocol parts that are built (not yet wired into `serve`); installation and trust chain enforcement, pushed tables and the plugin host module are documented as they are built.

### Trampoline: `gpuworks plugin-exec`

The server starts every plugin through its own binary: `gpuworks plugin-exec --profile <json> -- <executable> serve` (`internal/server/pluginexec`, a subcommand not meant for people). The trampoline applies the sandbox as the process-to-be and then `execve`s the plugin, so the host applies the sandbox and the plugin cannot undo it:

| Step | Linux | macOS |
|---|---|---|
| Parent-death signal | `PR_SET_PDEATHSIG(SIGKILL)`, and a check that the parent is still the server, or nothing starts | None; the SDK exits on fd EOF and by polling `getppid` |
| Privileges | `PR_SET_NO_NEW_PRIVS` | — |
| Process group | A group of its own, which the server signals as a whole | Same |
| Resources | `RLIMIT_CORE=0`, `RLIMIT_NOFILE=4096`, nice 5; `RLIMIT_NPROC` counts per user, so it is not set while the plugin shares the server's user, and the cgroup's `pids.max` bounds processes | Same (no cgroup) |
| Descriptors | fd 3 and 4 must be sockets; stdin and stdout are `/dev/null`; stderr is the log | Same |
| Environment | Only `PATH`, `TZ` and the proxy and CA variables are kept, plus `HOME` = the data directory, `TMPDIR`, `LANG=C.UTF-8` and `GWX_*` | Same |
| cgroup | When the server's cgroup is delegated (`subtree_control` writable), creates `<service cgroup>/plugins/<id>`, writes `memory.max`, `memory.high`, `cpu.weight=50` and `pids.max=512`, and moves in; when it cannot, it only warns and the plugin starts anyway | — |
| Landlock | ABI ≥ 1: the package directory readable and executable, the data and tmp directories with every right, CA files, `resolv.conf`, `hosts` and time zones read-only, approved device nodes read-write; from ABI 6 also no signals to processes outside the domain (the server and other plugins) | None |

A package that requires Landlock (certificate `min_isolation: landlock`) does not start on a host without it: the trampoline exits with code 78 and writes `PLUGIN_ISOLATION_UNAVAILABLE` to stderr. The trampoline's other exit codes: 64 for a bad command line, 71 when applying the sandbox failed, 127 when the plugin could not be executed.

### Supervision

`internal/server/pluginhost` runs one supervisor per package. The goroutine that starts a plugin stays locked to its OS thread for the plugin's whole life: on Linux the parent-death signal belongs to the thread that forked the child, and the Go runtime retires the thread of a locked goroutine that ends, so without the lock another goroutine ending could take the plugin with it.

| Item | Default |
|---|---|
| `initialize` deadline | 10 seconds |
| Health probe | Every 10 seconds, 3 seconds each; after 3 misses in a row the plugin is killed and restarted (`PLUGIN_UNHEALTHY`) |
| Crash restart | From 1 second, doubling, up to 5 minutes; 10 minutes of healthy running starts again from 1 second |
| Crashloop | 5 crashes within 10 minutes enter `crashloop` (`PLUGIN_CRASHLOOP`), retried every 5 minutes after that, so unattended offline sites recover by themselves |
| Stop | `POST /gwx/v1/shutdown`, SIGTERM to the process group 10 seconds later, SIGKILL 5 seconds after that |
| Call concurrency | At most `max_concurrency` (default 8) calls per plugin at once; beyond that `PLUGIN_BUSY` at once, never queued; health probes take no slot |
| Calls without a deadline | 60 seconds |
| stderr | Forwarded to the server log with `plugin=<id>`, JSON lines keep their level; 50 lines a second with bursts of 500, the excess counted and reported; the last 1 MiB kept for diagnostics |

A `resident` plugin starts as soon as it is added; an `on_demand` plugin starts on its first call, stops after 10 idle minutes and may not push state (`PLUGIN_STATE_ON_DEMAND`). Only `running` and `degraded` count as up; every other state (`starting`, `backoff`, `crashloop`, `stopping`, `stopped`, `idle`, `failed`) counts as down. A plugin that refuses the handshake for a reason retrying cannot change (such as `PLUGIN_CAPABILITY_MISSING`), answers for another build (`PLUGIN_HANDSHAKE_MISMATCH`) or picks a capability version that was not offered or approved (`PLUGIN_CAPABILITY_NOT_OFFERED`) enters `failed` and is not restarted until it is started again.

### gwx/1 connections and the host API

Each plugin gets two socketpairs. On fd 3 the server is the HTTP/2 client: first `initialize` (the server's capabilities, the approved grant, the tables it holds, and a principal key fresh per spawn), checking that the answer's `id`, `version` and `manifest_sha256` match the package and that `use` stays within what was offered and approved; then health probes, and forwarded routes and service calls, each with `Gwx-Deadline-Ms`. On fd 4 the server serves the host API: `PUT`/`PATCH /host/v1/state/{kind}` goes to the pushed-state implementation (an interface), and `GET /host/v1/health`; 20 calls a second per plugin with bursts of 100 (`PLUGIN_HOST_RATE_LIMITED`). The owner is the package the connection was bound to at the handshake, and a push carries no identity field; a push that arrives before the handshake is accepted waits for the outcome and gets `PLUGIN_HOST_NOT_READY` if it fails.

### Plugin SDK: `pkg/pluginsdk`

Plugin authors use `pkg/pluginsdk` (a module of its own like `pkg/extapi`, depending only on the standard library and `pkg/extapi`): `pluginsdk.Main(cfg)` opens the two descriptors, answers the handshake (protocol and capability negotiation, refusing with `PLUGIN_PROTOCOL_UNSUPPORTED` or `PLUGIN_CAPABILITY_MISSING`), runs `Start` once the handshake is accepted, answers health, and hands every other call to the plugin's `http.Handler`; it exits on `shutdown`, when the server's end closes, or when its parent is gone. `PushState` skips a table the server already holds with the same digest and numbers pushes `max(own counter, held generation)+1`, so a plugin whose storage was restored or wiped is never refused as stale. `pkg/pluginsdk/plugintest` plays the server in process over real socketpairs, for a plugin's own tests.

### Pushed state

The core's part of the first five pushed tables (`availability`, `fleet_capacity`, `node_holds`, `banners`, `attestations`) is built, in `internal/server/pushstate`; it is not connected to a plugin host yet, and the host, the supervisor and the routes will work through it once they are built:

- **Intake**: `State.Push` takes the pusher's identity as the supervisor binds it to the connection (package id, publisher, certificate grants, approved permissions, the manifest's codes, process generation) and one push. Each `availability` row is accepted only from the key's authority (for `gate: authority`, the `authority_id` the declaring publisher's certificate names; for `gate: self`, the declaring package), and a bare key only from a package of the project publisher; `fleet_capacity` needs the certificate's grant of that privileged table, `attestations` needs the pusher to be the authority of its own namespace, the rest need the approved `push:<kind>`; a notice's code must be listed in the package's manifest. A generation not above the one held or about to commit is refused (`PLUGIN_STATE_STALE`), as is a push over the connection of a replaced process.
- **Writes**: pending pushes are coalesced per owner and kind (the later replaces the earlier, which is answered `superseded_by`); each tick (at most 1 Hz) writes every owner's changes in one writer transaction, then swaps one snapshot pointer; when the write fails nothing applies (`PLUGIN_STATE_NOT_SAVED`). Persistence uses the typed `pstate_*` tables a core migration creates: a timeline is rows of steps, a notice is a code column plus typed parameter rows, and no column holds JSON; display-only `display` data is not kept. Writes are diffs by entry (feature key, node id, banner id, attestation name).
- **Boot**: every pushed table is read at boot, before any plugin starts; a version marker (`pstate.<kind>`) newer than this build refuses the start with `PLUGIN_STATE_TOO_NEW`, a stored restriction that does not decode refuses it rather than loosen it, and a stored grant that does not decode is left out (absent).
- **Enforcement**: the feature source, the node insert guard, the scheduling holder (source `plugins`) and the banners are each one atomic load plus map lookups, with no allocation. A key no package answers for is `locked` with `FEATURE_PROVIDER_ABSENT`. `app.NewPushedStateHooks` adapts them to the `Host` hook types.
- **Time**: timelines fire at their instants on extclock (`max(wall clock, high-water mark)`), with the plugin dead too; the high-water mark is saved every minute and at shutdown. `State.CorrectClock` moves it by at most 366 days per correction, reports it for the audit and shows an administrator banner; steps are not consumed, so a correction backwards un-fires the steps after it.
- **Down**: `State.OwnerDown` records a persisted down start (a restart never renews it); grants clamp to `min(current, down_access)` with `PLUGIN_UNAVAILABLE` after the grace (1 hour by default, 60 seconds to 24 hours as the plugin pushes, only lowered by the site) plus a 2-minute boot extension; restrictions never lapse. A package that is installed but no longer verifies keeps its restrictions and banners and loses only its grants.
- **Overrides and uninstall**: `State.Override` only loosens one restriction entry (a node hold, or one fleet cap), is persisted and bound to the owner's generation; it is superseded, and reported, when the owner pushes that entry again. `State.Uninstall` deletes every row of the owner in one commit and asks for one pass of the scheduling holds.
- **Capacity selection**: `fleet_capacity` gives only caps; the core selects the nodes in enrollment order (`internal/server/fleetcap`: once a cap is full, every later node is held). It reads the node list once after coalesced node fleet topic changes, at least 2 seconds apart, so nodes that join while the plugin is down are placed by the same rule. A node not placed yet is held only if counting it would exceed a cap.

## What the open-source code does not do

- It interprets no feature key and never branches on the edition name; core code names no feature.
- It calls no out-of-process extension on any request, insert, status report, claim or placement path, and runs no extension code such as WASM inside the writer transaction.
- It never hands extensions the Store or a raw database handle.
- It has no `init()` registration and no new package globals: everything comes in through `cli.Options` and `app.Config`.
- It never interrupts running workloads: the gate acts only on new requests and writes, and holders cordon without draining.

<!-- translation-source-sha256: 9ad2bedd1b37dee16e113059cae724cc4e62b8eea3ad105fbbe16af6b210b46a -->
