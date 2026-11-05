# Local deepmerge security patch

This is upstream deepmerge 4.3.1's self-contained `dist/cjs.js` and type
declarations, with its MIT license retained. The npm override replaces the
existing transitive dependency; it adds no external dependency.
The root `file:` dependency anchors the local path for npm; the `$deepmerge`
override makes the schema library use that same copy.

Upstream source: https://github.com/TehShrike/deepmerge/tree/v4.3.1
Advisory: https://github.com/advisories/GHSA-gcqx-w48m-w2h4

As of 2026-11-05, upstream has no patched release. This fork rejects the
`__proto__` key in both the target and source copy loops, regardless of the
target's prototype or value. Assignments also use `Object.defineProperty`
so the prototype setter cannot run. Other merge options and behavior stay
compatible with 4.3.1. The local name/version distinguish this patched fork
from the vulnerable registry release; changing metadata alone is not a fix.

`json-schema-library@9` embeds an unpatched copy in its CommonJS entry.
The production Vite build uses its ESM entry, which imports this replacement.
`tests/deepmergeSecurity.test.ts` verifies the installed dependency and the
production bundler's resolution and YAML schema behavior. Do not switch the
editor to the bundled CommonJS entry without rechecking this protection.

Remove this override and directory when a compatible upstream release fixes
the advisory, and keep the regression tests.
