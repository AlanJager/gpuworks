import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n } = await createI18n(["providers", "common"], "en");
const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { default: ProviderInstallModal } = await import("../src/components/ProviderInstallModal.tsx");
const { api } = await import("../src/api.ts");
type ProviderInstance = import("../src/api.ts").ProviderInstance;

const original = { ...api };
afterEach(() => { cleanup(); Object.assign(api, original); });

const direct = { id: "direct", type: "direct", label: { en: "Direct", zh: "直连" }, host: "47.0.0.1", port: 22, user: "root", install_supported: true };
const instance: ProviderInstance = {
  external_id: "i-1", name: "gpu-1", gpu_model: "NVIDIA A10", gpu_count: 1, power: "running", price_per_hr: 0,
  region: "cn-shanghai", public_ip: "47.0.0.1", ssh_port: 22, ssh_user: "root", ssh_connections: [direct], kind: "vm", node_id: 0,
};

function mountRecording() {
  const requests: Record<string, unknown>[] = [];
  Object.assign(api, {
    installAgent: async (_id: number, body: Record<string, unknown>) => {
      requests.push(body);
      return { status: "installing", connect_mode: "agent_dial", checks: [] };
    },
  });
  render(
    <I18nextProvider i18n={i18n}>
      <ProviderInstallModal providerId={1} providerKind="aliyun" inst={instance} onClose={() => {}} onDone={() => {}} />
    </I18nextProvider>,
  );
  return requests;
}

function install() {
  fireEvent.click(screen.getByRole("button", { name: /^(loading)?Install$/ }));
}

// Left alone, the advanced settings send nothing: the server installs into
// /opt/gpuworks under systemd where it can.
test("an untouched install sends no install directory and keeps systemd", async () => {
  const requests = mountRecording();
  install();
  await waitFor(() => assert.equal(requests.length, 1));
  assert.equal(requests[0].install_dir, undefined);
  assert.equal(requests[0].no_systemd, undefined);
});

// The operator puts the agent on a data disk and runs it without the unit;
// the help line says what turning systemd off means.
test("the install directory and the systemd switch reach the request", async () => {
  const requests = mountRecording();
  fireEvent.click(screen.getByText("Advanced Settings"));
  const dir = await screen.findByRole("textbox", { name: "Installation directory" }) as HTMLInputElement;
  assert.equal(dir.placeholder, "/opt/gpuworks");
  fireEvent.change(dir, { target: { value: " /data/gpuworks " } });
  const systemd = screen.getByRole("switch", { name: "Start via systemd" });
  assert.equal(systemd.getAttribute("aria-checked"), "true");
  assert.ok(screen.getByText(/registered as the gpuworks-agent systemd service/));
  fireEvent.click(systemd);
  assert.equal(systemd.getAttribute("aria-checked"), "false");
  assert.ok(screen.getByText(/does not start at boot/));
  install();
  await waitFor(() => assert.equal(requests.length, 1));
  assert.equal(requests[0].install_dir, "/data/gpuworks");
  assert.equal(requests[0].no_systemd, true);
});

// The server refuses a directory that is not absolute; the reason is shown
// in the operator's language and the form stays for a retry.
test("a refused install directory is explained and the form stays", async () => {
  const { APIError } = await import("../src/apiError.ts");
  Object.assign(api, {
    installAgent: async () => {
      throw new APIError("INSTALL_DIR_INVALID", { dir: "~/gpuworks" }, "invalid install_dir \"~/gpuworks\"", 400);
    },
  });
  render(
    <I18nextProvider i18n={i18n}>
      <ProviderInstallModal providerId={1} providerKind="aliyun" inst={instance} onClose={() => {}} onDone={() => {}} />
    </I18nextProvider>,
  );
  install();
  await screen.findByText("Installation failed");
  assert.ok(screen.getByText("The installation directory “~/gpuworks” is invalid: use an absolute path starting with /, other than the root directory."));
  assert.ok(screen.getByRole("button", { name: /Try again/ }));
  assert.ok(screen.getByText("Advanced Settings"));
});
