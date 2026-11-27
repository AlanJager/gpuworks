---
name: extension-design-review
description: Review a proposed extension point, pushed table kind, host capability or plugin-facing contract in gpuworks against ADR 0084 (extension levels and criteria) and ADR 0071. Use when an ADR or change adds or opens something to extension ("make X pluggable", "add a table kind", "new host capability", "plugin route access", "can X be extended"), or when reviewing such a merge request. Produces a verdict per criterion with file:line evidence. Not for checks a test or budget already enforces.
---

# Extension design review

The rules live in the repository, not in this skill. Read them before judging and cite them in the verdict:

- `docs/adr/0084-extension-levels-and-table-kind-registry.md`: the levels L0–L3 and the seven criteria
- `docs/adr/0071-installable-extensions.md`: tiers, hot paths in core, table classes and merge rules, down grace, local admin floor, contract tests
- `docs/dev/extensions.md`: the module contract, hot hook purity, what open-source code does not do
- `docs/reference/plugin-protocol.md` and `pkg/extapi`: the frozen wire formats and capabilities
- `CONTEXT.md`: the terms; a new term goes there before docs or UI text use it
- `scripts/arch-constraints.txt` and `scripts/arch-constraints.md`: the budgets that hold seams

When a file disagrees with this skill, the file wins. Report the disagreement too.

## Steps

1. **Name the thing and its level now.** Say what can be extended, and at which level it is today (L0–L3 as ADR 0084 decision 1 defines them). Back the level with evidence: where it is defined, how many places name it outside the definition (`rg` for its ids or names), and which budget holds the seam, if any. Without a single seam it is L0, whatever the proposal says.

2. **Answer the seven criteria** of ADR 0084 decision 3 in order, one row each, with evidence. Stop at the first criterion that keeps it at L1, and say so. Common failures:
   - opening something that is not yet one definition (criterion 1);
   - a hot-path decision opened as a call to a plugin instead of a table (criterion 3; ADR 0071 decision 3 allows only the login password check and the first KMS unwrap);
   - configuration that runs in a GPU container or grants host devices, accepted from an API, node or unsigned source (criterion 4);
   - a support claim without real-machine evidence (criterion 5; see `docs/dev/add-accelerator-vendor.md` for the vendor case);
   - a core mechanism that changes behaviour of a server with no extension installed (criterion 6);
   - a contract opened with no second user (criterion 7).

3. **Check the table kind**, when the proposal adds or changes one. Its descriptor (ADR 0084 decision 5) must state:
   - the class (restriction, grant, display, opaque);
   - whether it is privileged, and the row cap;
   - strict decoding to one named core type, with typed persistence (ADR 0032);
   - a merge rule that only tightens (ADR 0071 decision 8);
   - the snapshot it compiles to and the hook that executes it;
   - for a grant, down grace and the degraded answer; any timeline steps;
   - what the uninstall preview reports.

   Rows reference core ids, never names.

4. **Check the hot path.** Any hook it adds must be pure: no Store, no `Tables()`, no I/O, no contended lock; O(1) on one atomic snapshot; zero allocations on the allow path, proven with `testing.AllocsPerRun`; pre-built `*messages.ProductError` refusals; no reapply or rebuild requested from inside a hook (`docs/dev/extensions.md`, "纯函数规则").

5. **Check the contract.**
   - New wire fields: a new schema or capability version, never a changed meaning (ADR 0071 decision 7). Display-only data goes under `display`.
   - Golden files in `pkg/extapi` cover the new formats.
   - A capability once shipped is never removed.
   - The manifest's `Requires` decides compatibility at install time.

6. **Check parity and floors.**
   - Compiled modules and plugin packages express the concept as the same table kind (ADR 0084 decision 6), with no second, module-only path.
   - The local admin floor is untouched (ADR 0071 decision 11).
   - Running work is never interrupted (decision 13).
   - Removing or corrupting a package never loosens a restriction.

7. **Check the boundary.**
   - Core code names no feature and branches on no edition (budgets `edition_branches` and `feature_key_literals_in_core` stay 0).
   - Docs, tests and samples describe the mechanism in its own terms, with the fictional publisher `acme`.

8. **Check the paperwork.**
   - The ADR has an "扩展等级" section.
   - The row is in `docs/adr/README.md`.
   - Terms are in `CONTEXT.md`.
   - `docs/dev/extensions.md` or `docs/reference/plugin-protocol.md` describe what is implemented, and only that.
   - Contract tests are planned in `internal/testenv/extmodule` (compiled modules) or against a fake plugin process.

## Output

A table, one row per step and criterion:

| Item | Verdict | Evidence |
|---|---|---|
| Level now | L1 | `internal/server/kvbackend/kvbackend.go:12`; budget `kv_backend_id_literals 0` |
| Criterion 3 hot path | FAIL | renders at deployment evaluation, `deploy/...:NN`, yet proposes a plugin call |

Verdicts are PASS, FAIL, N/A or UNKNOWN. UNKNOWN means you could not find the evidence; it is not a pass. After the table, give the target level you would accept and the smallest change that gets there.

Do not repeat what a test or budget already enforces. Name the make target instead (`make check-architecture`, `make test`), and say whether you ran it.
