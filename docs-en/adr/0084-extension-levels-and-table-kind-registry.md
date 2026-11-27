# ADR 0084: Extension Levels and Criteria; Pushed Table Kinds Described by a Registry, and Compiled Modules Push the Same Tables as Plugin Packages

Date: 2026-11-27 · Status: Proposed (adds to [ADR 0071](0071-installable-extensions.md) decisions 3 and 8; does not change the frozen `gwx/1` wire format)

## Background

ADR 0071 settled that decision points live in the core and plugins push typed tables. ADRs 0069, 0081 and 0082 then each decided whether one kind of thing can be extended: KV offload backends are compiled in, plugin routes are authorized by pushed roles, and vendors are only registered at compile time. Each ADR answered "should this be opened" case by case, with no shared terms, so the same question keeps coming back:

- "Make it a plugin" sometimes means a compile-time registry, sometimes a signed data catalog, sometimes a server child process. The meanings differ, and so does the cost, by an order of magnitude.
- Some argue for opening everything by default. Opening a kind of thing freezes an add-only contract in `pkg/extapi` (ADR 0071 decision 7); frozen before there is a second user, it can never change. Some things (device grants, configuration that runs in a GPU container) add a security boundary as soon as they are opened.
- A thing can only be opened once it has a single definition. Vendor knowledge still lives in 39 files outside the definitions (`vendor_name_files_outside_definitions`); opening it now would turn scattered branches into a public contract.

Nor can the pushed table framework add a kind by "adding a row": `intake.go`, `rows.go` and `combine.go` in `internal/server/pushstate` switch on `extapi.Kind*` kind by kind, so each new kind touches several places. The planned tables (resource ranges, role bindings from ADR 0081, count limits, KV offload backend descriptions) all go through here.

Compiled modules (T1) and plugin packages (T2) also express the same thing in two ways: T1 calls dedicated methods on `app.Host` (`HoldScheduling`, `ProvideFeatures`, `AddBannerSource`, ...), while T2 pushes tables. Every concept is designed twice, and moving from T1 to T2 means a rewrite.

## Decision

1. **Extension levels.** Every extensible thing is at one of four levels, and ADRs and reviews use these names:

   | Level | Meaning | Who can add a new one |
   |---|---|---|
   | L0 scattered | Knowledge spread across many branches | A core change, easy to miss a place |
   | L1 one seam | One definition (a descriptor row or an interface), registered explicitly at compile time; its name or id appears nowhere outside the seam | A core change, or a compiled module |
   | L2 open data | The core implements the behaviour; which ones exist and their parameters come from signed data: a catalog in the release channel, or a typed table a plugin pushes | A trusted publisher, without a release |
   | L3 open code | Third-party code runs out of process (a server child process or a node component) and hands results over a protocol; hot paths still only look up tables | A trusted publisher |

2. **L1 is the default for every capability; L2 and L3 are opened by the criteria.** A new capability is a single seam from its first implementation, and a budget in `scripts/arch-constraints.txt` holds names outside the seam (for example `kv_backend_id_literals 0`). Until L1 holds, nothing is opened to L2 or L3; it converges first.

3. **Criteria.** To open a thing to L2 or L3, the ADR answers each of these:
   1. Is it L1 already? If not, converge first.
   2. Does the variation come from outside: different sites or publishers need different answers, or it changes faster than releases? If not, stay at L1.
   3. Is the decision on a hot path (per request, insert, report, claim or placement)? If so, it can only be opened as a table (L2, or a table pushed from L3), executed by the core.
   4. Does it run code or grant devices in a GPU container or on the host? If so, accept only content signed by a trusted publisher, with the core checking the output (device numbers, a closed set of template variables); if that cannot be done, stay at L1.
   5. Does claiming support need real-machine verification? If so, the extension must carry its evidence; if it cannot, stay at L1.
   6. Is the new core mechanism useful to a server with no extension installed, or at least behaviour-neutral for it? If not, do not add the mechanism.
   7. Are we willing to carry the contract cost: add-only schema, compatibility with old plugins, docs and golden files? Without a second user, stay at L1.

   The ADR has a section "扩展等级" (extension level): the level now, the target level, and the answer to each criterion.

4. **Extensions change answers, not where decisions are made.** Decision points are added only in the core (ADR 0071 decision 3); the answers of several extensions only tighten, by the merge rule the table kind declares (ADR 0071 decision 8).

5. **Pushed table kinds are described by a registry.** Each kind has one descriptor in `pushstate`; the framework reads descriptors and no longer branches on kinds:
   - the kind name, the class (restriction, grant, display, opaque; ADR 0071 decision 8), whether it is privileged, and the row cap;
   - strict decoding and validation (wire schema to the core's canonical form, "typed persistence" in ADR 0071 decision 8);
   - the merge rule (union, minimum, exclusive by key, everyone allows), implemented by the framework; a descriptor only picks one;
   - the function that compiles the snapshot, and the hook that executes it;
   - the next step time when there is a timeline, and the degradation rule on disconnect (grant class).

   The five existing kinds (`availability`, `fleet_capacity`, `node_holds`, `banners`, `attestations`) move to the registry first, with wire format, persistence and behaviour byte-for-byte unchanged, as the existing golden files and contract tests show. After that, a new kind is one descriptor and its executing hook, and no code in `pushstate` outside the descriptor files names a kind. The budget to be added, `pushstate_kind_refs_outside_descriptors` (registered in `scripts/arch-constraints.txt` with implementation step 1), counts references to `extapi.Kind*` constants in `internal/server/pushstate`, excluding the kind descriptor files and tests, with a baseline of 0. It counts more than `switch`: forms such as `sortedParts(extapi.KindNodeHolds, …)` count too.

6. **Compiled modules push the same tables; decision-function methods are frozen.** `app.Host` gains an in-process push entry: a compiled module pushes the same table kinds as a plugin package, with the same merging and validation; only its identity comes from the module name instead of an fd. From then on, new extension concepts are designed only as table kinds, and no new dedicated `Host` method is added for T1. Existing methods fall into two groups:
   - **Those expressible as data** (registered once, answered by a set of values, such as declaring features or the static content of banners): they become thin wrappers that push tables, with unchanged behaviour, as their contract tests show.
   - **Decision functions** (`SchedulingHolder.Hold`, `CardSchedulingHolder.HoldCard`, `NodeEnrollmentGuard`, `featuregate.Source.Availability`, `BannerSource.Banners` computed per viewer): they are arbitrary Go functions called each time and cannot be written losslessly as a table, so **no rewrite is promised**. These methods are frozen: existing behaviour and contract tests stay, no method of the same kind is added, and the docs say that new concepts go through tables; when what a decision function does can be expressed by a table, deprecating it is decided separately. This matches why the trade-offs of ADR 0081 rejected T1 decision functions.

   When a concept moves from a compiled module to a plugin package, the table content does not change.

7. **This ADR opens nothing new.** It only gives the terms, the criteria and the registry; table kinds such as resource ranges, action authorization for core routes, count limits, external identity and KV offload backend descriptions are each decided by their own ADR under criterion 3.

## Extension level of this ADR

Not applicable: this ADR is a mechanism and adds nothing extensible. The registry itself is L1, and the `pushstate_kind_refs_outside_descriptors` budget holds its seam.

## Trade-offs

- **Open everything by default vs L1 by default, opened by the criteria**: opening by default saves the discussion each time, but each opened thing freezes a contract and may add a security boundary, and most things have no second user. The cost of L1 by default is one convergence, and opening later needs no rework.
- **A registry vs keeping branches per kind**: branches are still readable with five kinds; the planned tables would double them, and every kind follows the same rules for merging, degradation and persistence, so the branches only repeat each other.
- **T1 pushes tables vs keeping dedicated methods**: dedicated methods have more direct types, but every concept gets designed twice, and T1 bypasses the merge rules and disconnect degradation (the trade-offs of ADR 0081 rejected T1 decision functions for that reason). Pushing tables gives both paths the same semantics.

## Implementation status

Not implemented. Order:

1. The registry and the migration of the five existing kinds, with the `pushstate_kind_refs_outside_descriptors` budget; refactoring only, golden files unchanged.
2. The in-process push entry of `app.Host`, with contract tests (`internal/testenv/extmodule`).
3. Existing methods expressible as data become thin wrappers one by one, one commit each; decision-function methods are marked frozen in the docs.
4. A line in `CLAUDE.md` under "文档与计划": an ADR that adds an extensible capability must have an "扩展等级" section.

<!-- translation-source-sha256: ecf6e0f641304146454339b4cac02c402df051e3a3b2a1f3e33e6b9e2ced10d1 # ADR 0084: Extension Levels and Criteria; Pushed Table Kinds Described by a Registry, and Compiled Modules Push the Same Tables as Plugin Packages -->
<!-- translation-source-sha256: 7ed5284792c5426a692a11983dc57b9f5565fce3ee0e7f26b95e030e1b75cfbe ## Background -->
<!-- translation-source-sha256: 15ea9c439ba805251621605eb4f64b9d943aa52ec64bbe314b5ce9e5795390d7 ## Decision -->
<!-- translation-source-sha256: 281313ddafa375dc6ee90cce339fa01b05733cdb2acc19e165a6b3a9c99f0928 ## Extension level of this ADR -->
<!-- translation-source-sha256: d656f3ce2a2d900dfd722247fbd08840e4066a0c1d85993a49e1fdc3dbde3d48 ## Trade-offs -->
<!-- translation-source-sha256: fbf3dbb0eea7a2c85c058a9974a32b00384a174afd546453ee4ffe46914d7c38 ## Implementation status -->
