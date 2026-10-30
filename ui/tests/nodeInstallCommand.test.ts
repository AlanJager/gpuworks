import assert from "node:assert/strict";
import { test } from "node:test";
import { buildInstallCommand } from "../src/nodeInstallCommand.ts";

test("custom agent-dial URL replaces both bootstrap URLs", () => {
  const command = buildInstallCommand({
    baseCommand: "curl -fsSL http://172.20.16.58:8199/install.sh | sh -s -- --server http://172.20.16.58:8199 --token token-1",
    token: "token-1",
    connectMode: "agent_dial",
    serverURL: "http://sa1.tunnelfrp.com:10001/",
    origin: "http://172.20.16.58:8199",
  });
  assert.equal(command, "curl -fsSL 'http://sa1.tunnelfrp.com:10001/install.sh' | sh -s -- --server 'http://sa1.tunnelfrp.com:10001' --token 'token-1'");
  assert.equal((command.match(/--server /g) ?? []).length, 1);
  assert.ok(!command.includes("172.20.16.58"));
});

test("default URL preserves the API command and server-dial options", () => {
  const base = "curl -fsSL http://control.example/install.sh | sh -s -- --server http://control.example --token token-2";
  assert.equal(buildInstallCommand({
    baseCommand: base,
    token: "token-2",
    connectMode: "agent_dial",
    serverURL: "http://control.example/",
    origin: "http://control.example",
  }), base);
  assert.equal(buildInstallCommand({
    baseCommand: base,
    token: "token-2",
    connectMode: "server_dial",
    serverURL: "http://control.example",
    origin: "http://control.example",
    installDir: "/srv/gpuworks",
  }), `${base} --install-dir '/srv/gpuworks'`);
  assert.equal(buildInstallCommand({
    baseCommand: base,
    token: "token-2",
    connectMode: "agent_dial",
    serverURL: "http://control.example",
    origin: "http://control.example",
    installDir: "/srv/gpuworks",
    noSystemd: true,
  }), `${base} --install-dir '/srv/gpuworks' --no-systemd`);
  // The server dials the host: the agent gets no address of its own.
  const serverDialBase = `${base} --connect-mode server_dial --listen :3389`;
  assert.equal(buildInstallCommand({
    baseCommand: serverDialBase,
    token: "token-2",
    connectMode: "server_dial",
    serverURL: "http://control.example",
    origin: "http://control.example",
  }), serverDialBase);
});

test("custom command arguments stay single shell words", () => {
  const command = buildInstallCommand({
    baseCommand: "ignored",
    token: "token-1",
    connectMode: "agent_dial",
    serverURL: "https://control.example/o';touch /tmp/pwned",
    origin: "http://localhost:8800",
    installDir: "/srv/gpu works",
  });
  assert.equal(command, "curl -fsSL 'https://control.example/o'\\'';touch /tmp/pwned/install.sh' | sh -s -- --server 'https://control.example/o'\\'';touch /tmp/pwned' --token 'token-1' --install-dir '/srv/gpu works'");
});

// The default directory is the node's (/opt/gpuworks on Linux, ~/gpuworks on
// a Mac), so only an empty field leaves it out; a typed /opt/gpuworks is kept.
test("an empty install directory is left to the node, a typed one is passed", () => {
  const options = { baseCommand: "base", token: "t", connectMode: "agent_dial" as const, serverURL: "", origin: "http://control.example" };
  assert.equal(buildInstallCommand({ ...options, installDir: "  " }), "base");
  assert.equal(buildInstallCommand({ ...options, installDir: "/opt/gpuworks" }), "base --install-dir '/opt/gpuworks'");
});
