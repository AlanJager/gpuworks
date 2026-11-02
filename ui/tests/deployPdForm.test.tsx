import assert from "node:assert/strict";
import { fakeFleet } from "./helpers/fakeFleet.ts";
import { fakeModels } from "./helpers/fakeModels.ts";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { fixturePdModes } from "./helpers/engineFixture.ts";
import { register } from "node:module";
import { pathToFileURL } from "node:url";
import { list } from "./helpers/listPage.ts";
register(
  pathToFileURL(
    new URL("./deploySleepableCssHook.mjs", import.meta.url).pathname,
  ),
);

// The PD deploy form: choosing "PD disaggregation" as the topology, adding
// and removing role instance rows (including by keyboard), the placement.pd
// payload it builds for evaluate, a field error the server attributes to
// "placement.pd", and evaluate's per-unit preview (a fitting and a
// non-fitting instance). Driven end to end through the real page, the way
// deployEvaluateFieldVisibility.test.tsx drives field-error attribution.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", {
  url: "http://localhost/",
});
for (const name of [
  "window",
  "document",
  "navigator",
  "HTMLElement",
  "HTMLTextAreaElement",
  "SVGElement",
  "Element",
  "Node",
  "MutationObserver",
  "ShadowRoot",
] as const) {
  Object.defineProperty(globalThis, name, {
    value: dom.window[name],
    configurable: true,
  });
}
Object.defineProperty(globalThis, "getComputedStyle", {
  value: (element: Element) => dom.window.getComputedStyle(element),
  configurable: true,
});
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: () => ({
    matches: false,
    addListener() {},
    removeListener() {},
    addEventListener() {},
    removeEventListener() {},
    dispatchEvent: () => false,
  }),
});
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
Object.defineProperty(globalThis, "ResizeObserver", {
  value: ResizeObserverStub,
  configurable: true,
});

const deployLocale = JSON.parse(
  fs.readFileSync(new URL("../src/i18n/locales/en/deploy.json", import.meta.url), "utf8"),
);
const commonLocale = JSON.parse(
  fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8"),
);
const imagesLocale = JSON.parse(
  fs.readFileSync(new URL("../src/i18n/locales/en/images.json", import.meta.url), "utf8"),
);
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deploy: deployLocale, common: commonLocale, images: imagesLocale } },
  defaultNS: "common",
  interpolation: { escapeValue: false },
});

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { apiErrorText, APIError } = await import("../src/apiError.ts");
const { api } = await import("../src/api.ts");

function errorText(code: string, params?: Record<string, unknown>) {
  return apiErrorText(
    (key: string, options?: Record<string, unknown>) => i18n.t(key, options),
    new APIError(code, params ?? {}, code, 200),
  );
}

const gpu = (index: number) => ({
  index,
  name: "H100",
  vendor: "nvidia",
  schedulable: true,
  arch_id: "Hopper",
  compute_capability: "9.0",
});
const nodeOne = {
  uuid: "node-1",
  name: "GPU Node 1",
  hostname: "gpu-node-1",
  state: "online",
  gpu_count: 2,
  gpu_vendor: "nvidia",
  gpus: [gpu(0), gpu(1)],
} as never;
const nodeTwo = {
  uuid: "node-2",
  name: "GPU Node 2",
  hostname: "gpu-node-2",
  state: "online",
  gpu_count: 2,
  gpu_vendor: "nvidia",
  gpus: [gpu(0), gpu(1)],
} as never;
const model = {
  id: 3,
  name: "Qwen/Qwen3-0.6B",
  source: "modelscope",
  source_id: "Qwen/Qwen3-0.6B",
  input_modalities: ["text"],
  weight_bytes: 1_500_000_000,
  nodes: [{ node_uuid: "node-1", state: "present" }],
} as never;
const engineProfile = {
  name: "vllm",
  sleep: { supported: false },
  quick_form_keys: [],
  param_specs: [],
  // Two modes, ascend listed first - exercises defaultPdMode's vendor match
  // (the nvidia vendor picked in openDeployPage) rather than a bare modes[0].
  pd_modes: fixturePdModes("vllm"),
} as never;

const apiAny = api as unknown as Record<string, unknown>;
const originals: Record<string, unknown> = {};
function stub(name: string, value: unknown) {
  originals[name] = apiAny[name];
  apiAny[name] = value;
}

// stubFleet answers the node endpoints as the server does (one searched page,
// single nodes, facets) and refuses listing every node.
function stubFleet(list: unknown[]) {
  const fake = fakeFleet(list as never);
  for (const name of ["nodes", "nodesPage", "node", "nodeFacets"] as const) stub(name, fake[name]);
  return fake;
}
// stubModels answers the model endpoints as the server does (one searched
// page, single models) and refuses listing every model.
function stubModels(list: unknown[]) {
  const fake = fakeModels(list as never);
  stub("models", fake.models_all);
  for (const name of ["modelsPage", "model"] as const) stub(name, fake[name]);
  return fake;
}
function resetStubs() {
  stubModels([model]);
  stubFleet([nodeOne, nodeTwo]);
  stub("engines", async () => list([engineProfile]));
  stub("inferenceTemplateIndex", async () => ({ items: [] }));
  stub("imageVersions", async () => list([]));
  stub("images", async () => list([]));
  stub("routingPolicies", async () => list([]));
}
resetStubs();
afterEach(() => {
  cleanup();
  resetStubs();
});
after(() => {
  closeChannels();
  dom.window.close();
});

const baseAnswer = {
  ok: true,
  warnings: [] as Array<{ code: string; params?: Record<string, unknown> }>,
  defaults: { model_mount: "/model", master_port: 29500, engine: "vllm" },
  form_values: { engine: "vllm" },
  image: "runner:vllm",
  command: ["vllm", "serve", "/model"],
  argv: [],
  template_deviated: false,
  template_match: true,
  eligible_nodes: [
    { uuid: "node-1", name: "GPU Node 1", eligible: true },
    { uuid: "node-2", name: "GPU Node 2", eligible: true },
  ],
  acquisition: { image: { image: "runner:vllm", cached: true } },
};

type PdRankAnswer = {
  node_uuid: string;
  node_name: string;
  address: string;
  fits: boolean;
  gpus?: number[];
  reason?: string;
  argv: string[];
  command: string[];
  followers?: PdRankAnswer[];
};
type PdRoleAnswer = { role: "decode" | "prefill"; units: PdRankAnswer[] };
type PdDraft = {
  mode: string;
  fallback?: string;
  kv_load_failure?: string;
  roles: Record<string, { tp?: number; pp?: number; units: Array<{ node_uuid?: string; followers?: Array<{ node_uuid?: string }> }> }>;
};

// pdRank is one rank of a PD unit as evaluatePD previews it: argv always, the
// command the entrypoint and argv joined, GPUs only when the rank fits.
function pdRank(node: string, name: string, argv: string[], extra: Partial<PdRankAnswer> = {}): PdRankAnswer {
  return {
    node_uuid: node,
    node_name: name,
    address: `10.0.0.${name.at(-1)}`,
    fits: true,
    gpus: [0],
    argv,
    command: ["vllm", "serve", ...argv],
    ...extra,
  };
}

const rankNodes = (unit?: { node_uuid?: string; followers?: Array<{ node_uuid?: string }> }) =>
  [unit?.node_uuid, ...(unit?.followers ?? []).map((follower) => follower.node_uuid)].join(",");

// pdEvaluate answers evaluate the way the server's evaluatePD does for the
// drafts these tests build. An ordinary draft gets the ordinary answer. A PD
// draft whose rows do not yet name the preview's nodes - these tests only
// ever leave a row's node unpicked - is rejected on placement.pd before any
// planning (PD_NODE_REQUIRED), with no preview. A PD draft naming them gets
// the per-rank preview with each role's TP/PP as the draft states them, ok
// only when every rank fits, and the top-level command describing the first
// decode instance. beforePreview lets a test hold that answer in flight.
function pdEvaluate(roles: PdRoleAnswer[], beforePreview?: () => Promise<void>) {
  return async (payload: Record<string, unknown>) => {
    const pd = (payload.placement as { pd?: PdDraft } | undefined)?.pd;
    if (!pd) return { ...baseAnswer };
    for (const role of roles) {
      const drafted = pd.roles[role.role]?.units ?? [];
      if (drafted.length !== role.units.length || drafted.some((unit, index) => rankNodes(unit) !== rankNodes(role.units[index]))) {
        return {
          ...baseAnswer,
          ok: false,
          command: undefined,
          errors: [{ field: "placement.pd", code: "PD_NODE_REQUIRED", params: { role: role.role } }],
        };
      }
    }
    await beforePreview?.();
    const ranks = roles.flatMap((role) => role.units.flatMap((unit) => [unit, ...(unit.followers ?? [])]));
    const firstDecode = roles.find((role) => role.role === "decode")!.units[0];
    return {
      ...baseAnswer,
      ok: ranks.every((rank) => rank.fits),
      argv: firstDecode.argv,
      command: firstDecode.command,
      replicas: roles.reduce((count, role) => count + role.units.length, 0),
      plan_scope: "per_instance",
      pd: {
        mode: pd.mode,
        fallback: pd.fallback,
        kv_load_failure: pd.kv_load_failure,
        roles: roles.map((role) => ({
          role: role.role,
          tp: pd.roles[role.role].tp ?? 1,
          ...(pd.roles[role.role].pp ? { pp: pd.roles[role.role].pp } : {}),
          units: role.units,
        })),
      },
    };
  };
}

async function openDeployPage() {
  const DeployPage = (await import("../src/pages/DeployPage.tsx")).default;
  render(
    <MemoryRouter initialEntries={["/deploy?model=3"]}>
      <I18nextProvider i18n={i18n}>
        <DeployPage />
      </I18nextProvider>
    </MemoryRouter>,
  );
  const vendor = await screen.findByRole("combobox", {
    name: new RegExp(deployLocale.basic.targetVendor),
  });
  fireEvent.mouseDown(vendor);
  fireEvent.click(await screen.findByTitle(deployLocale.basic.vendor.nvidia));
}

// A PD draft is placed by the platform until the deployer switches the same
// auto / manual control a single node has; the form opens on automatic, and
// most tests here are about naming the units' nodes, so they switch to manual.
async function choosePdTopology(placement: "auto" | "manual" = "manual") {
  const pdCard = await screen.findByText(deployLocale.workbench.node.pd);
  await waitFor(() => assert.ok(!pdCard.closest("button")?.hasAttribute("disabled")));
  fireEvent.click(pdCard);
  await screen.findByText(deployLocale.pd.mode);
  if (placement === "manual") {
    const pool = document.querySelector(".deploy-node-pool") as HTMLElement;
    fireEvent.click(within(pool).getByText(deployLocale.workbench.node.manual));
    await screen.findByText(deployLocale.workbench.node.manualUnitsDescription);
  }
}

function selectOption(combobox: HTMLElement, label: string) {
  const selector = combobox.closest(".ant-select")!.querySelector(".ant-select-selector") as HTMLElement;
  fireEvent.mouseDown(selector);
  // antd keeps a closed select's dropdown mounted (destroyPopupOnHide
  // defaults to false, and its hide transition never completes under
  // jsdom), so an earlier select's stale, still-visible dropdown can still
  // match a query by text. Each select's own dropdown portal is appended to
  // the document the first time it opens, so the most recently opened one -
  // the one this call just triggered - is always the last such node.
  const dropdowns = document.querySelectorAll(".ant-select-dropdown");
  const dropdown = dropdowns[dropdowns.length - 1];
  if (!dropdown) throw new Error("no dropdown opened");
  const options = [...dropdown.querySelectorAll(".ant-select-item-option")];
  const option = options.find((el) => el.textContent === label);
  if (!option) throw new Error(`option not found: ${label} (saw ${options.map((el) => el.textContent)})`);
  fireEvent.click(option);
}

// pickPdNode picks the node of a PD instance row (or, with a follower index,
// of one of its extra nodes) by the option's visible label.
function pickPdNode(role: "decode" | "prefill", label: string, follower?: number) {
  const suffix = follower ? ` ${follower}` : "";
  selectOption(
    screen.getByRole("combobox", { name: new RegExp(`^${deployLocale.pd.role[role]} instance 1 node${suffix}$`) }),
    label,
  );
}

test("choosing PD disaggregation builds placement.pd from the role sections, and leaves it out otherwise", async () => {
  const payloads: Array<Record<string, unknown>> = [];
  stub("evaluate", async (payload: Record<string, unknown>) => {
    payloads.push(payload);
    return { ...baseAnswer };
  });

  await openDeployPage();
  await waitFor(() => assert.ok(payloads.length > 0));
  assert.ok(
    payloads.every((payload) => !(payload.placement as Record<string, unknown> | undefined)?.pd),
    "an ordinary draft never sends placement.pd",
  );

  await choosePdTopology();

  const decodeNode = screen.getByRole("combobox", {
    name: new RegExp(`^${deployLocale.pd.role.decode} instance 1 node$`),
  });
  selectOption(decodeNode, "GPU Node 1 · 2 GPU");
  const prefillNode = screen.getByRole("combobox", {
    name: new RegExp(`^${deployLocale.pd.role.prefill} instance 1 node$`),
  });
  selectOption(prefillNode, "GPU Node 2 · 2 GPU");

  await waitFor(() => {
    const last = payloads.at(-1) as { placement?: { pd?: Record<string, unknown> } };
    const pd = last.placement?.pd as
      | { mode: string; fallback: string; kv_load_failure: string; roles: Record<string, { units: Array<Record<string, unknown>> }> }
      | undefined;
    assert.ok(pd, "the draft has a placement.pd once nodes are picked");
    assert.equal(pd!.mode, "vllm-nixl");
    assert.equal(pd!.fallback, "decode");
    assert.equal(pd!.kv_load_failure, "fail");
    assert.deepEqual(pd!.roles.decode.units, [{ node_uuid: "node-1" }]);
    assert.deepEqual(pd!.roles.prefill.units, [{ node_uuid: "node-2" }]);
  });

  // Switching the replica shape away drops placement.pd from the next draft:
  // the ordinary single/multi-node fields take over again.
  const singleCard = screen.getByText(deployLocale.workbench.node.single);
  fireEvent.click(singleCard);
  await waitFor(() => {
    const last = payloads.at(-1) as { placement?: { pd?: unknown } };
    assert.equal(last.placement?.pd, undefined);
  });
});

test("add and remove PD role instance rows, including by keyboard, with a floor of one row", async () => {
  stub("evaluate", async (payload: Record<string, unknown>) => ({ ...baseAnswer, __payload: payload } as never));
  await openDeployPage();
  await choosePdTopology();

  const addDecodeText = deployLocale.pd.addUnit.replace("{{role}}", deployLocale.pd.role.decode);
  const addDecode = (await screen.findByText(addDecodeText)).closest("button") as HTMLElement;
  const rowCount = () => document.querySelectorAll(".deploy-pd-unit-row").length;
  const decodeRows = () =>
    [...document.querySelectorAll(".deploy-pd-unit-row")].filter((row) =>
      row.querySelector(`[aria-label^="${deployLocale.pd.role.decode} instance"]`),
    );
  assert.equal(decodeRows().length, 1);

  fireEvent.click(addDecode);
  await waitFor(() => assert.equal(decodeRows().length, 2));

  // The second row's remove button is a plain <button>; Tab to it and press
  // Enter, the way a keyboard-only operator would remove a row.
  const removeSecond = screen.getByRole("button", {
    name: deployLocale.pd.removeUnit
      .replace("{{role}}", deployLocale.pd.role.decode)
      .replace("{{index}}", "2"),
  });
  removeSecond.focus();
  assert.equal(document.activeElement, removeSecond);
  await userEvent.setup({ document: dom.window.document }).keyboard("{Enter}");
  await waitFor(() => assert.equal(decodeRows().length, 1));

  // The floor of one instance per role: the sole remaining row's remove
  // button is disabled.
  const soleRemove = screen.getByRole("button", {
    name: deployLocale.pd.removeUnit
      .replace("{{role}}", deployLocale.pd.role.decode)
      .replace("{{index}}", "1"),
  });
  assert.ok(soleRemove.hasAttribute("disabled"));
  void rowCount;
});

test("setting a role's PP and adding a node to a unit sends both in placement.pd", async () => {
  const payloads: Array<Record<string, unknown>> = [];
  stub("evaluate", async (payload: Record<string, unknown>) => {
    payloads.push(payload);
    return { ...baseAnswer };
  });
  await openDeployPage();
  await choosePdTopology();

  const decodeNode = screen.getByRole("combobox", {
    name: new RegExp(`^${deployLocale.pd.role.decode} instance 1 node$`),
  });
  selectOption(decodeNode, "GPU Node 1 · 2 GPU");

  const ppInput = screen.getByRole("spinbutton", {
    name: deployLocale.pd.ppAria.replace("{{role}}", deployLocale.pd.role.decode),
  });
  await userEvent.setup({ document: dom.window.document }).type(ppInput, "2");

  const addNodeText = deployLocale.pd.addNode
    .replace("{{role}}", deployLocale.pd.role.decode)
    .replace("{{index}}", "1");
  fireEvent.click((await screen.findByText(addNodeText)).closest("button") as HTMLElement);
  await screen.findByText(
    deployLocale.pd.multiNodeTpHint.replace("{{role}}", deployLocale.pd.role.decode),
  );
  const followerNode = screen.getByRole("combobox", {
    name: new RegExp(`^${deployLocale.pd.role.decode} instance 1 node 1$`),
  });
  selectOption(followerNode, "GPU Node 2 · 2 GPU");

  await waitFor(() => {
    const last = payloads.at(-1) as { placement?: { pd?: { roles: Record<string, { pp?: number; units: Array<{ followers?: unknown[] }> }> } } };
    const decode = last.placement?.pd?.roles.decode;
    assert.ok(decode, "the draft has a decode role once a node is picked");
    assert.equal(decode!.pp, 2);
    assert.deepEqual(decode!.units[0].followers, [{ node_uuid: "node-2" }]);
  });
});

test("a field error the server attributes to placement.pd renders beside the PD section", async () => {
  const overlapParams = { node: "node-1", gpu: 0, role: "decode", other: "prefill" };
  let attempt = 0;
  stub("evaluate", async () => {
    attempt += 1;
    return {
      ...baseAnswer,
      ok: attempt > 1,
      errors:
        attempt > 1
          ? [{ field: "placement.pd", code: "PD_GPU_OVERLAP", params: overlapParams }]
          : [],
    };
  });
  await openDeployPage();
  await choosePdTopology();

  const decodeNode = screen.getByRole("combobox", {
    name: new RegExp(`^${deployLocale.pd.role.decode} instance 1 node$`),
  });
  selectOption(decodeNode, "GPU Node 1 · 2 GPU");

  const message = errorText("PD_GPU_OVERLAP", overlapParams);
  await waitFor(() => assert.ok(screen.getByText(message)));
  const pdSection = screen.getByTestId("pd-section");
  assert.ok(pdSection.textContent?.includes(message), "the PD_GPU_OVERLAP text renders inside the PD section");
});

test("evaluate's PD preview shows a fitting and a non-fitting unit, each with its command", async () => {
  const decode = pdRank("node-1", "GPU Node 1", ["/model", "--role", "decode"]);
  const prefill = pdRank("node-2", "GPU Node 2", ["/model", "--role", "prefill"], {
    fits: false,
    gpus: undefined,
    reason: "not enough VRAM on GPU Node 2",
  });
  stub("evaluate", pdEvaluate([
    { role: "decode", units: [decode] },
    { role: "prefill", units: [prefill] },
  ]));
  await openDeployPage();
  await choosePdTopology();
  pickPdNode("decode", "GPU Node 1 · 2 GPU");
  pickPdNode("prefill", "GPU Node 2 · 2 GPU");

  const preview = within(await screen.findByTestId("pd-preview"));
  await preview.findByText(deployLocale.pd.unitFits);
  assert.ok(preview.getByText(deployLocale.pd.unitDoesNotFit));
  assert.ok(preview.getByText("not enough VRAM on GPU Node 2"));
  assert.ok(preview.getByText("GPU Node 1"));
  assert.ok(preview.getByText("GPU Node 2"));
  assert.ok(preview.getByText("vllm serve /model --role decode"));
  assert.ok(preview.getByText("vllm serve /model --role prefill"), "a unit that does not fit still previews the command it would run");
  // The runtime command section answers the same evaluation, rank by rank.
  for (const label of ["Decode #1 · GPU Node 1", "Prefill #1 · GPU Node 2"]) {
    assert.ok(screen.getByRole("button", { name: deployLocale.catalog.copyRank.replace("{{rank}}", label) }));
  }
});

test("a re-evaluation withholds the previous PD preview until its own answer lands", async () => {
  let gate: Promise<void> | undefined;
  let release = () => {};
  stub("evaluate", pdEvaluate(
    [
      { role: "decode", units: [pdRank("node-1", "GPU Node 1", ["/model", "--role", "decode"])] },
      { role: "prefill", units: [pdRank("node-2", "GPU Node 2", ["/model", "--role", "prefill"])] },
    ],
    () => gate ?? Promise.resolve(),
  ));
  await openDeployPage();
  await choosePdTopology();
  pickPdNode("decode", "GPU Node 1 · 2 GPU");
  pickPdNode("prefill", "GPU Node 2 · 2 GPU");
  const preview = within(await screen.findByTestId("pd-preview"));
  await preview.findByText("vllm serve /model --role decode");

  gate = new Promise((resolve) => { release = resolve; });
  await userEvent.setup({ document: dom.window.document }).type(
    screen.getByRole("spinbutton", { name: deployLocale.pd.ppAria.replace("{{role}}", deployLocale.pd.role.decode) }),
    "2",
  );
  await preview.findByText(deployLocale.catalog.evaluationPending);
  assert.equal(preview.queryByText(deployLocale.pd.unitFits), null, "no fit verdict from the previous draft");
  assert.equal(preview.queryByText("vllm serve /model --role decode"), null, "no command from the previous draft");
  assert.ok(screen.getByText(deployLocale.runtimeCommand.generating));

  release();
  await preview.findByText(/PP 2/);
  assert.ok(preview.getByText("vllm serve /model --role decode"));
  assert.equal(preview.queryByText(deployLocale.catalog.evaluationPending), null);
});

test("evaluate's PD preview shows a role's PP and lists a unit's followers, each with its own command", async () => {
  const follower = pdRank("node-2", "GPU Node 2", ["/model", "--role", "decode", "--headless"]);
  stub("evaluate", pdEvaluate([
    { role: "decode", units: [pdRank("node-1", "GPU Node 1", ["/model", "--role", "decode"], { followers: [follower] })] },
    { role: "prefill", units: [pdRank("node-2", "GPU Node 2", ["/model", "--role", "prefill"], { gpus: [1] })] },
  ]));
  await openDeployPage();
  await choosePdTopology();
  pickPdNode("decode", "GPU Node 1 · 2 GPU");
  await userEvent.setup({ document: dom.window.document }).type(
    screen.getByRole("spinbutton", { name: deployLocale.pd.ppAria.replace("{{role}}", deployLocale.pd.role.decode) }),
    "2",
  );
  const addNodeText = deployLocale.pd.addNode
    .replace("{{role}}", deployLocale.pd.role.decode)
    .replace("{{index}}", "1");
  fireEvent.click((await screen.findByText(addNodeText)).closest("button") as HTMLElement);
  await screen.findByText(deployLocale.pd.multiNodeTpHint.replace("{{role}}", deployLocale.pd.role.decode));
  pickPdNode("decode", "GPU Node 2 · 2 GPU", 1);
  pickPdNode("prefill", "GPU Node 2 · 2 GPU");

  const preview = within(await screen.findByTestId("pd-preview"));
  await preview.findByText(`${deployLocale.pd.role.decode} · TP 1 · PP 2`);
  assert.ok(preview.getByText(`${deployLocale.pd.role.prefill} · TP 1`));
  assert.ok(preview.getByText(deployLocale.pd.followersLabel));
  assert.ok(preview.getByText("GPU Node 1"));
  assert.equal(preview.getAllByText("GPU Node 2").length, 2, "the follower rank and the prefill instance both run on GPU Node 2");
  assert.ok(preview.getByText("vllm serve /model --role decode"));
  assert.ok(preview.getByText("vllm serve /model --role decode --headless"), "a follower previews its own headless command");
});
