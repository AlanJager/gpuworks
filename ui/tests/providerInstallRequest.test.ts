import assert from "node:assert/strict";
import { test } from "node:test";

import { buildProviderInstallRequest } from "../src/providerInstallRequest.ts";

test("agent_dial defaults use auto SSH and do not mix node addresses", () => {
  assert.deepEqual(buildProviderInstallRequest({
    externalID: "vast-1",
    serverURL: "http://control.example:8800",
    sshKeyPEM: "private-key",
    password: "",
    sshConnectionID: "auto",
    connectMode: "agent_dial",
    serverDialListen: "0.0.0.0:18800",
    serverDialAddress: "203.0.113.8:18800",
  }), {
    external_id: "vast-1",
    server_url: "http://control.example:8800",
    ssh_key_pem: "private-key",
    password: undefined,
    ssh_connection_id: "auto",
    connect_mode: "agent_dial",
    server_dial_listen: undefined,
    server_dial_address: undefined,
    allow_instance_restart: undefined,
    install_dir: undefined,
    no_systemd: undefined,
  });
});

// The install directory and the systemd switch reach the request only when
// the operator changed them; defaults leave the server to choose.
test("install directory and no_systemd are sent only when set", () => {
  const base = {
    externalID: "ecs-1",
    serverURL: "http://control.example:8800",
    sshKeyPEM: "",
    password: "",
    sshConnectionID: "auto",
    connectMode: "agent_dial" as const,
    serverDialListen: "",
    serverDialAddress: "",
  };
  const request = buildProviderInstallRequest({ ...base, installDir: " /data/gpuworks ", noSystemd: true });
  assert.equal(request.install_dir, "/data/gpuworks");
  assert.equal(request.no_systemd, true);
  const defaults = buildProviderInstallRequest({ ...base, installDir: "  ", noSystemd: false });
  assert.equal(defaults.install_dir, undefined);
  assert.equal(defaults.no_systemd, undefined);
});

// The server dials the agent, which never contacts the server: the request
// names no server, only where the agent listens and where it is reached.
test("server_dial sends the listen and compute node addresses and no server", () => {
  const request = buildProviderInstallRequest({
    externalID: "vm-1",
    serverURL: "http://control.example:8800",
    sshKeyPEM: "private-key",
    password: "",
    sshConnectionID: "direct",
    connectMode: "server_dial",
    serverDialListen: " 0.0.0.0:6006 ",
    serverDialAddress: "connect.example.com:37145",
  });
  assert.equal(request.server_dial_address, "connect.example.com:37145");
  assert.equal(request.server_dial_listen, "0.0.0.0:6006");
  assert.equal(request.server_url, undefined);
});
