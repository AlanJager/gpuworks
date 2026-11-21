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

<!-- translation-source-sha256: a98610c733b5eb87c6006156530028802a90374992cd919a6f9a2cc633138534 -->
