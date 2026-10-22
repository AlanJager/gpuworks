# Template probe fixtures

`k100-first38-signatures.json` is the minimum non-sensitive projection of
`worktree_1/artifacts/k100-model-support/hygon-live-validation.json` records
`[0:38]`, the acceptance subset fixed by `plan-v4.md`. It keeps only the stable
case ID, historical PASS/FAIL label, whether the failure occurred in a forward
kernel, and the smallest log fragment needed to exercise the shared signature
classifier. It intentionally omits hosts, addresses, mounts, model locations,
and raw artifacts.

The fixture is a regression of classifier selectivity, not a new hardware run.
Its expected accounting is 18 PASS + 20 FAIL, with 13 deterministic startup or
dependency failures caught by default, 14 when forward-kernel failures are
explicitly included, and zero PASS records classified as incompatible.
