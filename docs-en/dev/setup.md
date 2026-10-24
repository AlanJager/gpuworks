# Local Development Environment

## Dependencies

- Use the Go version specified in `go.mod` (currently Go 1.26.9, consistent with the CI image `golang:1.26`).
- Node.js and npm; use the frontend dependency versions specified in `ui/package.json` and the lockfile.
- GNU Make. Builds or L3/L4 tests may also require Docker, SSH, `curl`, and the target vendor's CLI.

```bash
go version
node --version
npm --version
make demo
```

A local server started with `make run-server` listens on `:8800` by default and writes data to `./data`. Parallel development instances use 8800, 8802,
8804, and 8806 in order. Use `password` for manual testing; do not use this test password in production.
`make demo` uses a separate `127.0.0.1:8099` address and a temporary directory, and generates an administrator password for the current run.
When the script exits, it cleans up the processes and temporary directory it created.

## Start the Backend and Frontend Separately

Open a terminal at the repository root, start the backend, and leave it running:

```bash
make run-server
```

Open another terminal, enter `ui/` from the repository root, and start the frontend development server with hot reloading:

```bash
cd ui
npm install
npm run dev
```

The regular `make ui` build first runs i18n checks, then writes the artifacts to `internal/server/webui/dist`. The frontend development
server is only for hot reloading. To validate the embedded artifacts, run `make ui` before starting the Go server.

## Local Secrets

Store addresses, SSH parameters, and cloud keys only in the gitignored `secrets/` directory. You can create
`secrets/test-environments.md` to record your local environments, but do not commit real values. For non-sensitive descriptions of environment variables, see the
[Configuration Reference](../reference/configuration.md).

## Deploy to a Long-Running Acceptance Environment

Record long-running acceptance servers and physical machines (addresses, SSH methods, and accounts) in the gitignored `secrets/test-environments.md`,
which stays out of the repository. To update these servers, use `gpuworks install` to reinstall into the same directory. It copies itself, rewrites the service, and checks health,
so you do not need to write a systemd unit manually.

`install` does not create backups (only `upgrade` does). Before replacing the binary, make a copy of the old binary
`/opt/gpuworks/server/gpuworks` on the acceptance machine. To roll back, rebuild from the target commit and install again.

Run the build command from the repository root:

```bash
make server-linux-amd64          # Includes the UI and embedded agents; do not substitute go build
```

Run the installation and health check commands on the Linux amd64 acceptance machine. If you build on another machine, copy the artifact to the acceptance machine first,
then replace `./bin/gpuworks-linux-amd64` below with the path to the copied artifact:

```bash
./bin/gpuworks-linux-amd64 install --dir /opt/gpuworks/server --addr :8800
curl --fail http://127.0.0.1:8800/healthz
```

If the service fails to start after deployment, check `systemctl status gpuworks` and `journalctl -u gpuworks` first.

## Common Checks

```bash
make test
make test-integration
make vet
git diff --check
```

For complete requirements on test layers, race detection, build tags, and physical hardware, see the [Testing Guide](testing.md).
