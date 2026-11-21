# Control-Plane Cluster Behaviour and Limits

This page explains how a three-member control-plane cluster behaves when members fail, and what this release does not do.

The community distribution carries neither the cluster runtime nor any cluster command: the cluster engine stays in this repository behind the `dqlite` build tag, and the default build leaves it out. The installation commands, the database helper `gpuworks-dqlite`, and its native build come with a distribution that ships the cluster runtime; see that distribution's documentation for the installation steps.

## Composition

- Each of the three members runs one `gpuworks` server. The server is a pure Go program; the database is carried by the helper `gpuworks-dqlite` that it starts. The three helpers use mutual TLS with member certificates and replicate one database.
- All three members are voters; a write needs two of them to confirm it.
- Users and agents reach only one stable entry (a load balancer or a keepalived VIP). Member addresses are not a supported entry.
- The default build and macOS remain a single SQLite node and do not join a cluster.

## Behaviour on Failure

- **One member lost**: the other two keep serving. The entry should forward requests to members that answer 200 on `GET /api/v1/readyz`.
- **The majority lost**: the remaining member refuses management writes with 503; it does not shrink the membership to keep writing.
- **Scheduling follows the database leader**: only the member that holds the database leader runs the scheduling and control loops; after the leader changes, the new leader takes over. There is no separate scheduling lease.
- **A write sent to a member that cannot commit it**: it answers 503, nothing is committed, and the entry can send the request to another member.
- **A member that has not opened business** (just started, cut off from the majority, or with materials not yet verified): business requests and `/api/v1/readyz` both answer 503 with the body `member business service is not ready`.

## Limits

These are limits of this release.

- **In-flight streams are not resumed.** On a failover, streamed answers and image or model transfers that are already open end with the member that holds them; the client has to start again. A failure after a request has been forwarded is not retried.
- **An agent is served by the member that holds its connection.** Members do not forward agent sessions to each other: an operation that needs the agent but arrives through another member is not guaranteed to work. After the agent reconnects through the entry, the new member serves it.
- **Revoking a member key at run time applies to new database connections only.** Established connections are not cut.
- **There is no recovery-time guarantee.** Writes resume after a leader election and after members reopen business; the time depends on the storage's synchronous write latency and on the network. It grows clearly on slow disks; no number of seconds is guaranteed here. The [storage tier](#storage-tiers) settled at installation sets the election wait; what it lists is a recovery target, not a guarantee.
- **A member is not upgraded, restarted or stopped with the single-server commands.** `gpuworks upgrade`, `gpuworks restart`, `gpuworks stop` and the upgrade in the console act on one server only; they do not know the member's own service unit, its database helper or the other members. On a cluster member's installation they refuse and change nothing. A member is upgraded, started and stopped one at a time, following the procedure of the edition that provides the cluster.
- **Fewer than three members is not supported, and members are not removed by hand.** With two members the database helper keeps a single voter and the other member stops receiving data, so one more failure stops the cluster. When a member's host fails, leave it in the membership: two of the three voters remain and still commit writes. This release has no procedure for replacing a member. When a member has left or lost its vote, or a voter cannot be reached, the cluster status report says `Overall: degraded` with the reason instead of `ready`; a cluster that is still being formed does not count: within five minutes of its newest admission, with no member ever revoked.
- **The first start is slow.** Migrations on a new database commit one by one and are synchronized to the majority.
- **Only Linux amd64 members are supported**; the helper is the only artifact that uses cgo.

## Storage Tiers

Every commit in the cluster waits for a majority of members to finish one synchronous write. The database holds a new election after it has not reached the leader for some time; the storage tier sets that time, and the three members must agree on it. The tier is settled at installation and recorded by the first node as `storage_tier` in `<data directory>/cluster-enrollment/config.json` (`standard` writes no key); the other members get it from the cluster when they join.

| Tier | Wait before a follower starts an election | Suitable storage | Recovery target after the leader process is killed |
|---|---|---|---|
| `standard` | 150–300 ms | Synchronous writes never stall for 150 ms or more | 10 seconds |
| `slow-storage` | 0.75–1.5 s | Synchronous writes stall for hundreds of milliseconds to seconds (such as shared network storage) | 6 seconds |

The recovery targets come from a small measured sample and are no guarantee for other storage. When the storage of all three members stalls for seconds at once, requests fail in both tiers.

## Protocol Version of the Database Helper

When the server starts the helper it reads the helper's one-line ready announcement, in which `protocol` is the protocol version the helper implements. The server accepts only the version it supports; with a different version, or with no version in the announcement, it refuses to start the database and the error names both versions. Install the helper and the server as a pair.

## Upgrading the Cluster's Schema

A version that brings new migrations cannot be rolled out one member at a time: the three members share one database, and a migration changes the schema for all three at once. The cluster does this step in a short maintenance window.

- **A member on the new program waits first.** The new version of the server starts on the older schema: it keeps serving its database replica, keeps its business closed, and records its migration list in the database. The other members go on serving business, so while members are replaced one by one, business stays available until the last one is replaced. A program older than the database schema still refuses to start, and so does one that lacks an extension module the database already has.
- **The database leader performs three steps**, and each requires all three members to have reported the same migration list:
  1. Begin: the maintenance phase goes from `serving` to `upgrading`. From then on no member accepts a business write, including a member still running the old program.
  2. Migrate: the pending migrations of the core and of each extension module run once, in order, each committed in one transaction with its marker. When the new version brings one more module, its tables are created in this step too. After a leader change or a failure the step is repeated on the new leader, and migrations already committed are skipped.
  3. Finish: the maintenance phase returns to `serving`; the members open business again on their own, without a restart.
- **Answers in the window**: every member answers business requests (including `/api/v1/readyz`) with 503 and the product code `CLUSTER_UPGRADE_IN_PROGRESS`, with `Retry-After`. `/healthz` is unchanged.
- **Abort** is available only while no migration has run; the maintenance phase returns to `serving`, and the members then have to go back to the earlier program. Once a migration has run there is no way back except restoring the backup taken before the upgrade, so a backup is required before upgrading.
- **The steps are served by each member's own enrollment listener** to a caller that presents a cluster member certificate, that is, a command run on a member host as the installation's account. Management sessions are unavailable while business is closed, so the steps are not management API routes.

Current limits:

- A member reports its program each time it starts, and repeats the report every few seconds while it waits for the upgrade. When a member is replaced by another version inside the window and started, its report changes and the later steps are refused. An old version without this feature does not report, so that case goes unnoticed; once the migrations have run it refuses to start because the schema is newer than it.
- A member that is down keeps its last report. As long as most members repeated theirs within the last 30 seconds the steps go on, and the member catches up when it returns.
- The command and console that replace the programs member by member, pick the leader and show progress at the entry do not exist yet; the steps above are called one by one.


<!-- translation-source-sha256: 9b7df160cf8f13b33a9d255826e1d6c77157294000f87f2299dbbabd85f6197e -->
