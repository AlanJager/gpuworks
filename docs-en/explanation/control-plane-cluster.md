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
- **Business is served by the member that leads the database.** Any other member that receives a business request (management API, console, `/v1` inference, an agent connecting) hands it over unchanged on the mutual-TLS channel between members, so the answer through the entry is the same whichever member it lands on. `/healthz` and `/api/v1/readyz` are answered by the member that received them. A member that cannot observe a leader for the moment answers business requests with 503, which the entry may retry.
- **A write sent to a member that cannot commit it**: it answers 503, nothing is committed, and the entry can send the request to another member.
- **A member that has not opened business** (just started, cut off from the majority, or with materials not yet verified): business requests and `/api/v1/readyz` both answer 503 with the body `member business service is not ready`.
- **A short storage stall**: a member admits requests on the membership it last confirmed. The confirmation is renewed in the background every few seconds and no request waits for it, so a storage stall of a few seconds neither refuses nor delays a request for authorization. While a renewal gets no answer (a storage stall, a timeout, no database leader) the member keeps admitting requests, and closes business only when no renewal has succeeded for 60 seconds after the start of the last successful confirmation. A pass that copies the members' key files to the other members and runs out of time, or finds no database leader, does not close the member either; files that differ from what was verified, are missing or cannot be read do.
- **After a member is revoked or cut off**:
  - Writes: before its first write, a member's write transaction holds the database writer and then reads the member's own admission in the same transaction (a transaction of the ordinary pool holds the writer from its begin and is checked at its first write; a transaction of the scheduling pool takes the writer and is checked when it begins). A check that itself fails (writer taken, no leader, a dropped connection, a timeout) is not a refusal and is returned to the caller as it is. A revoked member that is still a voter and still reaches the leader is refused before it commits, and then closes its own business. A member cut off from the majority could not commit anyway. Three kinds of write do not pass this check: the writes that decide membership itself (claiming an invitation, the first member, a revocation, and the confirmation of membership and of transport authorization; a confirmation asked for by a revoked member is refused by the confirmation itself); loading a backup image or an import into an empty database; and the server's own startup work when it opens the database (schema check or migration, default data). What a member records while serving (copies, receipts and manifests of the key files, the action-grant roster, its enrollment address) does pass it. A store that an operator tool opens directly on a database that is out of service (import, restore, address activation) and the cluster upgrade session do not act for a member and are not checked either; a read-only transaction is not checked and cannot write.
  - Reads and inference requests: when the database answers that the member was revoked, the member closes at once, normally at its next renewal after the revocation, within a few seconds. When its renewals get no answer, at most 60 seconds after the start of its last successful confirmation; until then it answers from the data it last saw.
  - Connections between members: admitted on a member list confirmed within the last 15 seconds, and for at most 30 seconds when it cannot be confirmed. The member that commits a revocation drops its list at once. A connection that is already open is checked only when it is opened.

## Limits

These are limits of this release.

- **In-flight streams are not resumed.** On a failover, streamed answers and image or model transfers that are already open end with the member that holds them; the client has to start again. A failure after a request has been forwarded is not retried.
- **When the leader changes, agents stay connected and inference requests in progress are cut.** An agent's connection stays on the member that accepted it, which attaches it to the leader. After the lead moves to another member, that member attaches it to the new leader, and the agent only receives its full desired state once more. For that time (about one to a few seconds, as measured) the node may briefly show as connecting, inference requests through it get 503, and calls awaiting an answer and open streams (log tails, terminals, transfers) are cut. When the member that accepted an agent's connection stops, that agent reconnects. Running instances are not affected. All business traffic passes through one member: the cluster adds availability, not capacity.
- **Revoking a member key at run time applies to new database connections only.** Established connections are not cut.
- **There is no recovery-time guarantee.** Writes resume after a leader election and after members reopen business; the time depends on the storage's synchronous write latency and on the network. It grows clearly on slow disks; no number of seconds is guaranteed here. The [storage tier](#storage-tiers) settled at installation sets the election wait; what it lists is a recovery target, not a guarantee.
- **A member is not upgraded, restarted or stopped with the single-server commands.** `gpuworks upgrade`, `gpuworks restart`, `gpuworks stop` and the upgrade in the console act on one server only; they do not know the member's own service unit, its database helper or the other members. On a cluster member's installation they refuse and change nothing. A cluster is upgraded with `gpuworks cluster upgrade` of the edition that provides the cluster; a member is started and stopped through its own service unit.
- **Fewer than three members is not supported, and members are not removed by hand.** With two members the database helper keeps a single voter and the other member stops receiving data, so one more failure stops the cluster. When a member's host fails, leave it in the membership: two of the three voters remain and still commit writes. This release has no procedure for replacing a member. When a member has left or lost its vote, or a voter cannot be reached, the cluster status report says `Overall: degraded` with the reason instead of `ready`; a cluster that is still being formed does not count: within five minutes of its newest admission, with no member ever revoked.
- **Backups are manual, and a restore stops the whole cluster.** A cluster takes no backups by itself. One member can write the complete database image, the master key, the cluster identity and the files the database refers to into one archive while the cluster keeps serving; for a restore every member is stopped, the member that took the backup rebuilds its data directory from the archive, and the other two join again. Writes and login sessions made after the archive do not exist after a restore. The backup of the single-server `gpuworks upgrade` does not apply to a cluster: the replicated database refuses the snapshot method it uses.
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
- **Progress in the window**: every member answers `GET /api/v1/cluster/upgrade-progress` on its business listener, whether its business is open or closed, without a login (no session can be checked in the window, because sessions are recorded in the business store). It holds only a state code (`idle`, `waiting`, `window`, `migrating`, `unobserved`), whether this member's business is open, the applied and target migration counts, and the total and ready member counts; it names no address, release or error text, and requests within one second share one read. On a 503 with the product code above the console switches to its "The control plane is being upgraded" screen, polls this document, and reloads by itself once a member serves again. A browser that opens the console during the window gets a self-contained progress page from the member; the status is still 503.
- **Abort** is available only while no migration has run; the maintenance phase returns to `serving`, and the members then have to go back to the earlier program. Once a migration has run there is no way back except restoring the backup taken before the upgrade, so a backup is required before upgrading.
- **The steps are served by each member's own enrollment listener** to a caller that presents a cluster member certificate, that is, a command run on a member host as the installation's account. Management sessions are unavailable while business is closed, so the steps are not management API routes.

Current limits:

- A member reports its program each time it starts, and repeats the report every few seconds while it waits for the upgrade. When a member is replaced by another version inside the window and started, its report changes and the later steps are refused. An old version without this feature does not report, so that case goes unnoticed; once the migrations have run it refuses to start because the schema is newer than it.
- A member that is down keeps its last report. As long as most members repeated theirs within the last 30 seconds the steps go on, and the member catches up when it returns.
- What replaces the programs member by member and calls these steps in order is `gpuworks cluster upgrade` of the edition that provides the cluster; a build of this repository has no such command. A console entry and progress at the entry layer do not exist yet.

<!-- translation-source-sha256: d893a7614f922930a0120a0577e4098bee4fbdbc0a805c47575855a11736c20c # Control-Plane Cluster Behaviour and Limits -->
<!-- translation-source-sha256: 759f9f888c6cdaa0e610709be4e93ac8423ae04edf6c39dc34a64a1eae8fa022 ## Composition -->
<!-- translation-source-sha256: 97f6cbf094cb556021bff3904701e3ff1712a458e12443820fcfd62b1571ef71 ## Behaviour on Failure -->
<!-- translation-source-sha256: 0c107885f72821f97efd1713f64d70cb0abf5ddbedd10e0a4dc63a4733bcaebb ## Limits -->
<!-- translation-source-sha256: c56a989bc2555e6ad4a6cf5dd7ffe407203e6a9f32497c41ea4ce8ca3322cf66 ## Storage Tiers -->
<!-- translation-source-sha256: 5b963d9b6cab0af7f80ccbda255cde725e9cbf149f23b48fa1e3bc2159cd2c1d ## Protocol Version of the Database Helper -->
<!-- translation-source-sha256: eaf8bd1fe2717d28bd1bb30280880649a75da600dfd209d3b68e148280a845e9 ## Upgrading the Cluster's Schema -->
