# Install a Three-Member Control-Plane Cluster

This guide explains how to install the control plane as a cluster of three members: when one member is down, the other two keep serving the management API, the console, and the gateway. For behaviour and limits, see [Control-plane cluster behaviour and limits](../explanation/control-plane-cluster.md).

**Use case**: the management plane must not stop when the control-plane machine is down, and you have three Linux hosts and one stable entry. To install a single control plane, see [Install and upgrade the control plane](install-server.md).

**Prerequisites**:

- Three Linux amd64 hosts, each with an IPv4 address the others can reach; loopback addresses are not accepted.
- One stable entry, one of: an existing highly available load balancer, or a keepalived VIP. Do not use both. The entry forwards requests to the three members' HTTP port and must pass the WebSocket Upgrade of `/api/v1/agent/connect` through as is: no buffering, no retry. Agents and users reach only this entry; member addresses are not a supported entry.
- SSH from the management machine to the three hosts (no password, `BatchMode`), with the three host keys already in the management machine's `known_hosts`; installation uses `StrictHostKeyChecking=yes`.
- To install as a systemd service, the SSH user can `sudo` without a password and the hosts run systemd.
- These ports are free and mutually reachable on the three hosts: HTTP `8800`, enrollment `9443`, database `9001`, member sessions `9002` (the database port plus one).

## 1. Build the Two Artifacts

The default build has no cluster runtime and behaves as before. A cluster needs a server built with the `dqlite` tag and the database helper `gpuworks-dqlite`; only the helper uses cgo.

```bash
make dqlite-deps                # pinned static native libraries and a patched go-dqlite, into tmp/dqlite/
make dqlite-helper              # bin/gpuworks-dqlite-linux-amd64 (static, cgo)
make server-linux-amd64-dqlite  # bin/gpuworks-linux-amd64-dqlite (pure Go)
```

`make dqlite-deps` needs `musl-gcc`, installs no system package, and downloads no Go toolchain; see `scripts/dqlite/README.md` in the repository for details. After changing `go.mod`, run it again and then rebuild both artifacts.

## 2. Write cluster.yaml

```yaml
version: 1
defaults:
  dir: /opt/gpuworks/server
  ssh:
    user: ops
    key: /home/ops/.ssh/id_ed25519
ports:
  http: 8800
  enrollment: 9443
  database: 9001
nodes:
  - name: a
    address: 10.0.0.11
    ssh: {target: 10.0.0.11}
  - name: b
    address: 10.0.0.12
    ssh: {target: 10.0.0.12}
  - name: c
    address: 10.0.0.13
    ssh: {target: 10.0.0.13}
```

- `version` must be `1`; there must be exactly three `nodes`, and each `address` is a distinct IPv4 address.
- Each node can override `defaults` with its own `dir` and `ssh` (`target`, `user`, `key`, `port`).
- The installation directory must not exist on any of the three hosts; installation does not overwrite an existing directory.
- You can also run `gpuworks cluster install --configure`, which writes this file from your answers without any remote operation.
- The optional top-level key `storage_tier: standard` or `storage_tier: slow-storage` sets the [storage tier](#storage-tiers); without it, installation chooses from its measurement. `--storage-tier` on the command line takes precedence over this key.

## 3. Preflight

```bash
gpuworks cluster check --config cluster.yaml --binary bin/gpuworks-linux-amd64-dqlite
```

The preflight verifies directories, ports, and the paths between nodes (enrollment, database, member sessions) on the three hosts over SSH, and removes its temporary programs and listeners when it ends. It does not use `--password-file`.

After the network checks pass, the preflight also measures synchronous write latency for 30 seconds on the filesystem that will hold each node's data (a 4 KiB write followed by `fdatasync`, at most 40 a second; the measurement file is removed afterwards) and recommends a [storage tier](#storage-tiers) from the slowest node. `--storage-probe 0` skips it and `--storage-probe 2m` lengthens it, up to 5 minutes. In the preflight the tier is a recommendation only: it does not change whether the preflight passes, and a node that could not be measured is shown as not measured.

## 4. Install

```bash
gpuworks cluster install --config cluster.yaml \
  --binary bin/gpuworks-linux-amd64-dqlite \
  --database-helper bin/gpuworks-dqlite-linux-amd64 \
  --password-file admin-password
```

- `--password-file` is the initial administrator password file: a regular file with mode `600`, one line, at least 8 bytes. The password cannot be given on the command line; in a terminal, omitting the flag prompts for it.
- `--no-service` writes no systemd unit and runs detached processes that do not come back after a host reboot; use it for a preview only.
- Installation runs, in order: preflight, distribution of both artifacts, initialization of the first node, joining the other two, starting the control plane, waiting for the cluster to synchronize, and a business check. A failure at any step stops it; data and logs already created on the hosts are kept, and the error is in `logs/install-node-errors.log` under the installation directory concerned.
- The first start applies every migration to a new database and writes the built-in catalogs, so it is clearly slower than later starts, and slower still on hosts with slow storage. The overall deadline is `--timeout`, 15 minutes by default.
- The preflight that installation runs measures storage in the same way and settles the storage tier from it; see the next section. `--storage-probe 0` skips the measurement.

### Storage Tiers

Every commit in the cluster waits for a synchronous write on a majority of members. The database holds a new election after it has gone some time without hearing from the leader; the storage tier sets that time, and it must be the same on all three members. The tier is settled at installation, recorded by the first node, and taken from the cluster by the other members when they join.

| Tier | Wait before a follower starts an election | Storage it is for | Recovery target after the leader process is killed |
|---|---|---|---|
| `standard` | 150–300 ms | Synchronous writes never stall for 150 ms or more | 10 seconds |
| `slow-storage` | 0.75–1.5 s | Synchronous writes stall for hundreds of milliseconds to seconds (shared network storage, for example) | 6 seconds |

Installation chooses the tier from the measurement and states the reason in its progress output:

| Measurement (slowest node) | Without `--storage-tier` | With `--storage-tier` |
|---|---|---|
| p99 at most 100 ms and no stall of 150 ms or more | `standard` | The given tier |
| p99 above 100 ms, or any stall of 150 ms or more | `slow-storage` | The given tier, with a warning when it is `standard` |
| p50 above 150 ms, or a maximum above 5 s | Stops before any remote write | The given tier, with a warning |
| Not measured (skipped, or the measurement file could not be created on a node) | `standard`, with a notice | The given tier |

The p99 alone is not enough: one measured host had a p99 of 18.5 ms and still showed a single 2.25-second synchronous write in 30 seconds, and one such stall can cost the leader its role. The tier therefore also looks at the maximum and at the number of stalls of 150 ms or more.

The recovery targets come from measurements on three 4-core members on shared network storage (`fdatasync` p50 about 4 ms, p90 20–80 ms, p99 0.5–1.9 s), with the leader killed 5 times per tier: `standard` recovered in 0.78–2.11 seconds and `slow-storage` in 1.71–3.07 seconds. The sample is small and says nothing about the tail; the numbers are not a guarantee for other storage. On that same storage:

- On `standard` an idle cluster changed its database leader by itself about every 5–6 minutes; on `slow-storage` it did so once in 27 minutes. `slow-storage` makes these changes rarer and does not remove them.
- When the storage of all three members stalls for seconds at once, requests fail on either tier. A longer election wait does not help with that.
- The first start takes minutes on slow storage.

There is no command that changes the tier of an installed cluster. It takes stopping all three members together, setting `storage_tier` in `<data directory>/cluster-enrollment/config.json` on every member to the same value (for `standard`, remove the key), and starting them together; members on different tiers disagree on election timing. `gpuworks cluster-status` shows the tier this member has recorded.

Each member's installation directory holds:

| Path | Content |
|---|---|
| `<dir>/gpuworks` | The server |
| `<dir>/gpuworks-dqlite` | The database helper, started by the server and exiting with it |
| `<dir>/server.yaml` | Listen address, data directory, and log; no credential |
| `<dir>/data` | Data directory, including the member certificate and the database |
| `<dir>/logs/server.log` | Server log; the systemd unit's standard error also goes to the journal |

The systemd unit is named `gpuworks-cluster-<first 16 hexadecimal characters of the SHA-256 of the absolute installation directory>.service`.

## 5. Confirm Readiness

Against each member's HTTP port:

```bash
curl -s http://10.0.0.11:8800/api/v1/readyz
```

A ready member answers 200 with exactly `{"status":"ready"}`. A member that has not opened business yet answers 503 with the body `member business service is not ready`. The health check of the load balancer or keepalived should use this path and keep only members that answer 200 in the forwarding pool.

To see the cluster state as one member observes it, run on that member:

```bash
gpuworks cluster-status --data /opt/gpuworks/server/data
```

The output includes the member's role, the number of voters, whether the cluster tolerates a fault, the state of each listener, whether business is open, and the current leader.

## Member-Side Commands

`cluster install` calls the commands below on each host; use them directly only for manual repair. They exist only in a Linux build with the `dqlite` tag; for their flags, see the [CLI reference](../reference/cli.md#gpuworks-cluster).

- `gpuworks cluster-init`: on the stopped first node, create the cluster identity and the enrollment listener configuration.
- `gpuworks cluster-address`: on the stopped first node, change the database address to the one the member certificate names.
- `gpuworks cluster-join`: join a new member to the cluster with an invitation token.
- `gpuworks cluster-status`: report, read-only, the state this member sees.

<!-- translation-source-sha256: 7d66bc078f98019486cc54adea9c46b4b1b911db3ea0f1ca8f8dba4b3e360d15 -->
<!-- translation-localized-code-block: 42fbdbc4c1196d6c771a0edf7a829c3a52548d1e50dfa1e0c5c23ed3ba5d11df 17037dbd919039cbb319e17ff34bae914301d5861fb18d6edad5ba7035dc86ef -->
