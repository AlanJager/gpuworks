# Adding a KV offload backend

A KV offload backend decides what keeps an instance's KV cache beyond the GPU: the engine's own memory and disk tiers, or a component outside the engine (LMCache or Mooncake Store, for example). This page is for contributors adding a backend. It describes the backend model, where each part lives and what adding a backend changes. The design trade-offs are in [ADR 0069](../adr/0069-kv-offload-backends.md).

## Backend model

A backend is one descriptor row in `internal/server/kvbackend`, unique per (engine, id) and compiled into the binary:

| Field | Meaning |
|---|---|
| `ID`, `Engine` | The backend id and its engine; the id is the value of the deployment parameter `kv_offload_backend` |
| `Label` | The product name, shown as is on the deploy form's card and the instance panel |
| `Default` | The engine's own offload; an empty `kv_offload_backend` resolves to it, at most one per engine |
| `External` | A component outside the engine keeps the KV cache; the requested-feature snapshot records `external_kv_cache` |
| `Vendors` | Accelerator vendor allow-list; empty means every vendor |
| `Replaces` | Native parameter keys that cannot be set while the backend is selected |
| `PDRoles` | PD roles that keep the backend; other roles launch without offload |
| `Topology` | The node-count cap and whether PP and DP are allowed |

A row holds only the data decisions are made on. There is no `init()` registration and no mutable package state. Today the only row is `vllm/native`, so the deploy form and the API behave exactly as with native offload alone.

## Where each part lives

- **Selection**: `kv_offload_backend` in the parameter table is a selector key (`Selector`) with no flag, and it is not a platform parameter. It is published only when the engine has at least one non-default row, with options generated from the registry; otherwise the key is dropped from requests.
- **Rendering**: `internal/server/deploy/kv_render_vllm.go` gives each backend's connector object by id, and `composeVLLMKVTransfer` in `kv_compose.go` composes the PD part and the offload part into the one `--kv-transfer-config`.
- **Validation**: `kv_offload.go` returns these codes (text in `ui/src/i18n/locales/{en,zh}/common.json`):

  | Code | When |
  |---|---|
  | `KV_OFFLOAD_BACKEND_UNKNOWN` | The engine has no selectable backend with that id; checked on evaluate, edit and launch |
  | `KV_OFFLOAD_BACKEND_CONFLICTS_NATIVE_TIER` | A native key in `Replaces` has an effective value; values the platform writes by default do not count |
  | `KV_OFFLOAD_BACKEND_CONFLICTS_ARG` | An argument layer of a non-PD external backend states a flag that would replace its configuration (`KVConflictFlags`, matched across spellings) |
  | `KV_OFFLOAD_BACKEND_UNSUPPORTED_ON_TARGET` | The target vendor is not in `Vendors` |
  | `KV_OFFLOAD_BACKEND_TOPOLOGY_UNSUPPORTED` | The node count, PP or DP is outside `Topology` |

  The last two are checked in `ValidateTargetFeatures`, which both validating modes reach; collecting capabilities alone does not check them.
- **PD**: `PDRoleLaunchConfig` clears the selection and the native fields when the role is not in `PDRoles`.
- **Reverse parsing**: the engine hook `RecognizeKVBackend` reads a backend's non-PD rendering back into the selection. `ReverseArgs` and `CanUnsetTemplateFlag` share it, so saving as a template round-trips.
- **Frontend**: the engine profile's `kv_backends` lists the selectable backends (`ProfileForTarget` filters them by vendor). The deploy form shows one card per entry, and when one is selected `projectDraft` unsets the keys in its `replaces`. `ui/src` names no backend id.

## Adding a backend

1. Add a row in `kvbackend.Builtin()`, filling only the fields it really uses. A new field arrives together with the first row that uses it.
2. Add a renderer for the id in `vllmOffloadRenderers()` (`kv_render_vllm.go`). If its connector is offload-only, also add it to the offload connector list in `internal/engines/features.go`, or it is classified as PD.
3. Add the id to the `kv_backend_id_literals` pattern in `scripts/check_architecture.sh`.
4. Tests: the table invariants in `kvbackend`; `TestKVBackendSetRendersEverySelectableRow` in `deploy` checks the renderer and `Replaces`. The generic rules are already covered by the test row `vllm/testext` (`kv_backends_helpers_test.go`), so a new backend adds tests for its own rendering and a real-hardware L4 case.
5. Update the cards' execution status in the browser E2E ledger `test/e2e/coverage/deployment-wizard-create.json`.

## Known limits

- In a mixed-vendor cluster the scheduler does not filter nodes by backend vendor; an unsupported vendor is refused at target validation after placement.
- An older server drops `kv_offload_backend`: a deployment that selected an external backend runs without offload there.

<!-- translation-source-sha256: efe360e5ce84831d3f88e7e598cb5398deb154cea5cadde6835eec362c940149 -->
