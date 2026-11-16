# extapi

`gpuworks.ai/gpuworks/pkg/extapi` holds the wire formats between a GPUWorks
server and the plugins it runs. It is its own Go module and uses only the
standard library, so a plugin can depend on it without depending on the
server.

What it defines:

| Object | Go type | Version |
|---|---|---|
| Package manifest (`manifest.json`) | `Manifest` | `schema: 1` |
| Signature envelope (`manifest.sig`, certificates, trust updates) | `Envelope` | protocol major 1 |
| Publisher certificate (`publisher.crt` holds one to four envelopes) | `PublisherCert` | `format: 1` |
| Trust update | `TrustUpdate` | `format: 1` |
| Revocation list | `CRL` | `format: 1` |
| gwx/1 handshake (`POST /gwx/v1/initialize`) | `HandshakeRequest`, `HandshakeResponse` | protocol major 1 |
| gwx/1 health and error bodies | `Health`, `WireError` | protocol major 1 |
| State push and its acknowledgement | `StatePush`, `StateAck` | protocol major 1 |
| Pushed tables | `AvailabilityV1`, `FleetCapacityV1`, `NodeHoldsV1`, `BannersV1`, `AttestationsV1` | `table.<kind>@1` |

## Building against it

The server's `go.mod` requires `gpuworks.ai/gpuworks/pkg/extapi v0.0.0` and
points it at this directory with a `replace`. Go applies `replace` only in
the main module, so a module that depends on `gpuworks.ai/gpuworks` (a
separately built server, a fork) resolves `pkg/extapi` only if it either
uses a `go.work` that includes the server checkout, or carries the same
replace itself:

```
replace gpuworks.ai/gpuworks/pkg/extapi => <server checkout>/pkg/extapi
```

Before the module is published for plugins, tag it `pkg/extapi/vX.Y.Z` and
require that version in the server's `go.mod` instead of `v0.0.0`.

## Compatibility promise

Sites run servers that are never upgraded next to plugins built years later,
and the reverse. So:

1. **Protocol major 1 is frozen.** The envelope, handshake, error and push
   envelope shapes never change. A major 2 needs a design decision record,
   and a server that speaks it keeps speaking major 1 for at least three
   years.
2. **Capability versions are never removed.** Every version a server has
   offered within major 1 (`table.availability@1`, `routes@1`, ...) stays
   offered. New behaviour arrives as a new capability or a new version of
   one, never as a change to a released version.
3. **Rows are strict.** A reader decodes an object only at a version it
   knows, and refuses unknown fields there. A field that changes what the
   server enforces is therefore always a new version of its object (a new
   Go type beside the old one); an older server refuses it instead of
   silently dropping a restriction. Display-only data goes in `display`
   objects, the only part an older reader may drop.
4. **Numbers are integers** within ±(2^53−1), times are RFC 3339 UTC with
   whole seconds, and byte strings are standard padded base64.

## Canonical JSON and signatures

Signed objects carry their payload bytes exactly as signed, and those bytes
must be canonical JSON: no whitespace, object keys sorted by their UTF-8
bytes, no duplicate keys, integers only, and minimal string escaping (see the
package documentation for the exact rules). One document therefore has one
digest, `sha256:<hex>`.

An envelope holds one or more `{keyid, sig}` signatures. The algorithm
belongs to the trusted key, never to the signature: Ed25519 (`ed25519`) or
ECDSA P-256 with SHA-256 and DER signatures (`ecdsa-p256-sha256`, minimal
DER with a low `s` only, so a signature has one encoding). A key id
names one key forever: a project root's starts with `gwroot-` (the stem `gwroot` is not a valid publisher name), a certified
publisher key's with `<publisher>-`. Signatures by unknown key ids are skipped, so a signer
can rotate by signing with the old and the new key; a signature by a trusted
key that does not verify fails the whole envelope.

## Errors

Every decoding, validation and verification failure is an `*extapi.Error`
with a stable code (`PLUGIN_WIRE_INVALID`,
`PLUGIN_STATE_REJECTED`, `PLUGIN_SCHEMA_TOO_NEW`, ...) and parameters; the
message is an English fallback. The server's console carries English and
Chinese text for every code.

## Golden files

`testdata/golden/<kind>/v1/` is the frozen wire corpus. Every file under
`valid/` decodes, validates and re-encodes to the same bytes; every file
under `invalid/<CODE>/` fails with exactly that code, at the field its name
gives. `testdata/golden/SHA256SUMS` may only grow: a released golden is never
edited or removed. A new version of an object gets its own `v2/` directory.

The reference page `docs/reference/plugin-protocol.md` (and its English
translation under `docs-en/`) embeds goldens as its examples;
`TestReferencePageExamplesAreGoldenFiles` keeps each one byte-for-byte
equal to its file and requires an example of every wire object.

## Tests

```bash
cd pkg/extapi && CGO_ENABLED=0 go test -race ./...
```

From the repository root, `make test-extapi` runs vet and the tests of this
module; `make test` includes it.
