# Control-Plane Cluster Behaviour and Limits

This page explains how a three-member control-plane cluster behaves when members fail, and what this release does not do. For installation, see [Install a three-member control-plane cluster](../how-to/control-plane-cluster.md).

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
- **There is no recovery-time guarantee.** Writes resume after a leader election and after members reopen business; the time depends on the storage's synchronous write latency and on the network. It grows clearly on slow disks; no number of seconds is guaranteed here. The [storage tier](../how-to/control-plane-cluster.md#storage-tiers) settled at installation sets the election wait; what it lists is a recovery target, not a guarantee.
- **The first start is slow.** Migrations on a new database commit one by one and are synchronized to the majority.
- **Only Linux amd64 members are supported**; the helper is the only artifact that uses cgo.

<!-- translation-source-sha256: a6ee751b76162798f7e8961c99308bdfe209045c28f1f0513d5ace5657b3dfce -->
