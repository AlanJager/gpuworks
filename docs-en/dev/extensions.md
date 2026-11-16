# Extension Points: How a Separately Built Server Composes

Another Go module can use the gpuworks server as a library and compile it, together with its own code, into a new server binary (a "separately built server" below). This page describes the interfaces the open-source code leaves for that, the thread each one is called on, the rules they must follow, and what the open-source code does not do. The irreversible decisions are in [ADR 0070](../adr/0070-extension-tiers-and-edition-boundary.md); the terms (extension module, feature key, feature availability, extension-held cordon) are defined in `CONTEXT.md`.

Extension docs, examples and tests describe each mechanism in its own terms; sample publishers, editions and plugins use the fictional name `acme`, as sample domains use `example.com`.

## Four tiers

A capability goes in the lowest tier that can carry it:

| Tier | Runs where | Fits |
|---|---|---|
| T0 Core | server, agent | Domain primitives every build needs; the community edition itself |
| T1 Compiled module | inside the server process, in Go; its console part is a UI module overlaid at build time | Capabilities that decide on every management request, inside an insert or status transaction, or must work before any node exists |
| T2 Server plugin | a child process of the server, over stdio | Adapters to outside systems with no UI and no state of their own |
| T3 App | container workloads on nodes | Its own UI, data and domain workflow, using GPUs only through platform-scheduled workloads |

This page covers only what T0 leaves for T1. The T2 and T3 interfaces do not exist yet.

## How a build composes

A separately built server has its own `main`, which calls `cli.Main` once:

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
| `MountAPI(e)` | Mounts an `api.Extension`'s routes under `/api/v1/<name>/` |
| `DeclareFeatures(...)` / `ProvideFeatures(src)` | Declares feature keys; provides the feature source (at most one across all modules) |
| `AddBannerSource(b)` | Banners at the top of every console page |
| `GuardNodeInsert(g)` | A guard on every new node's insert |
| `HoldScheduling(h)` | A scheduling holder, keyed by its source; a repeated source is refused |
| `ReapplySchedulingHolds()` | The holder's answers changed: asks for the holds to be applied to every node again, rather than at each node's next status report |
| `NodeFleet(ctx)` | Every node's hold facts (`store.NodeHoldFacts`), ordered by ID (enrollment order), in one query on the read pool |

The `Host` hands out neither the `*store.Store` nor a database handle. A registration after `Init` returns is refused (`ReapplySchedulingHolds` and `NodeFleet` are not registrations and may be called at any time): methods with an error result return `app.ErrHostClosed`, the others panic. If `Init` fails, `New` fails and closes the database, so the same data directory starts again at once.

After every `Init` has run, the server releases extension-held cordons that no module of this build owns (so they do not outlive a changed build or a removed module), applies the holders' answers to every node once if any holder is registered, then builds the feature gate (`featuregate.New`) and hands it to the API.

## Reapplying holds and the node fleet

A holder is otherwise asked on every status report, so a node whose answer changed can wait up to a minute to be cordoned or released. A module whose answers changed (for example, a rebuilt snapshot holds a different set of nodes) calls `ReapplySchedulingHolds()`, and the server runs `ApplySchedulingHolds` over every node again:

- It never blocks and allocates nothing, and may be called from any goroutine, **except from inside a hook** (`SchedulingHolder.Hold`, an insert guard): a pass asks every holder again, so a holder that asked for one from `Hold` would keep the whole fleet in passes.
- Requests are coalesced: any number made while a pass is pending or running make at most one more pass, and passes are at least 2 seconds apart, so a holder whose answers flap costs at most one fleet-wide writer transaction every 2 seconds, never one per event.
- The pass runs in the same `services()` loop as the modules' `Service`, off the caller's goroutine, and stops with the server; a failed pass is logged as an error and retried with a backoff that starts at 2 seconds and doubles up to a minute. With no holder registered it does nothing.

A holder whose answers depend on the whole fleet (for example, choosing nodes in enrollment order) learns the fleet from these two, rather than by recording the nodes it sees in `Hold`:

- `NodeFleet(ctx)`: every node's hold facts, in one query on the read pool. It is for boot and for a recompute after the fleet changed, coalesced; never per report or per event, and never from inside a hook. Subscribe to the topic below before the first read, so no change between them is missed.
- The bus topic `store.NodeFleetTopic`: one event for each node insert (all three insert paths), each delete, and each status report that changed the node's hold facts (its card count or vendor; its ID, UUID, enrollment time and Kubernetes cluster never change), keyed by the node's ID. Read it with `store.NodeFleetChangeOf(ev)`, which gives `{NodeID, Kind}` (`NodeInserted`, `NodeDeleted`, `NodeFactsChanged`); do not parse the reason text of the `node` topic. A routine report that changed no hold fact publishes nothing, so publishers cost what they did. Like every bus topic, a subscriber that falls behind gets only each node's latest event, so it treats any event as "this node, and so the fleet, changed" and recomputes from `NodeFleet`, coalesced, rather than counting events. A node ID is never reused after its node is deleted.

`Service.Run` is started by the same `services()` loop in `App.Run` as the health checks, card failure records, usage report and notice refresh, after the controller starts. It returns when `ctx` is done; an error it returns is logged and the service is not restarted. One App can be `Run` more than once (an in-place restart), so `Run` must tolerate being called again.

## Tables and migrations

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

A separately built server's UI modules are copied to `ui/src/extensions/<id>/` (git-ignored), registered in `ui/src/extensions/index.ts`, and may import only the components, hooks and API client that `ui/src/extensions/sdk.ts` re-exports (the `ee_ui_internal_imports` budget is 0). The build sets `GPUWORKS_UI_OUT_DIR` to write elsewhere; with UI modules checked out and the variable unset, `make ui` and `make release-packages` refuse to build, so nothing is overlaid onto the community edition. The modules' texts ship in their own locale bundles merged into `common`, checked by the same i18n check through `GPUWORKS_I18N_EXTRA_LOCALES` / `GPUWORKS_I18N_EXTRA_MESSAGES`.

## Codes and texts

Failures and refusals an extension produces follow [ADR 0067](../adr/0067-runtime-outcomes-as-codes.md) too: UPPER_SNAKE codes, interpolated params, an English fallback. An extension's codes and their en/zh texts live in the extension's own code and UI modules, not in the open-source `messages` or locale files; without the overlay, the console shows the English fallback.

## Upgrades and installation

- A distribution index names its edition inside the signed payload; an index whose edition does not match the build's channel is refused, and the community edition accepts only indexes that name no edition or `community`.
- `upgrade` compares `gpuworks edition` of the installed binary and the staged one and refuses a different edition unless given `--allow-edition-change`; a console upgrade to another edition answers 409 `SERVER_EDITION_MISMATCH`.
- `upgrade` runs the install hooks of the binary doing the upgrade; after a cross-grade, run `install` with the new binary as prompted.
- Going back from an extended build to the community edition, the `extensions:` section of `server.yaml` only logs a warning, the extension tables are left as they are, and extension-held cordons are released at startup.

## What the open-source code does not do

- It interprets no feature key and never branches on the edition name.
- It never hands extensions the Store or a raw database handle.
- It has no `init()` registration and no new package globals: everything comes in through `cli.Options` and `app.Config`.
- It never interrupts running workloads: the gate acts only on new requests and writes, and holders cordon without draining.

<!-- translation-source-sha256: a3991a1ac2492ca90ae779214df33a1898ac3492408ecdf823f705ed657a43f8 -->
