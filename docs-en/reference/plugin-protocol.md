# Plugin Protocol and Wire Formats (gwx/1)

The wire formats of plugin package manifests, publisher certificates, trust updates, revocation lists, signature envelopes, the `gwx/1` handshake, health and error messages, and the tables plugins push are defined in a separate Go module, `pkg/extapi` (module path `gpuworks.ai/gpuworks/pkg/extapi`). It uses only the standard library, so a plugin depends on it and not on the server.

These formats are frozen, and this page describes each of them. The server's plugin host (installing and verifying packages, starting plugin processes, enforcing pushed tables) is **not built yet**: the merge, grace, timeline and revocation semantics this page mentions are conventions of the formats that the future plugin host will enforce. The design and its trade-offs are in [ADR 0071](../adr/0071-installable-extensions.md); the contract for modules compiled into the server is in [Extension Points](../dev/extensions.md).

The golden corpus `pkg/extapi/testdata/golden/` is the authority on the formats. Every JSON example on this page is byte-for-byte identical to a file in the corpus, named in a comment above the example in the page source, and a test in `pkg/extapi` (`TestReferencePageExamplesAreGoldenFiles`) compares each one. That is why the examples are canonical JSON (one line, sorted keys).

## General rules

- **Strict decoding.** A reader decodes an object only at a version it knows and refuses any field that version does not define (`PLUGIN_WIRE_UNKNOWN_FIELD`). Keys are case-sensitive; duplicate keys, data after the JSON value, values of the wrong type, invalid UTF-8 and an escaped UTF-16 surrogate that is not half of a pair (a lone `\ud800`) are refused (`PLUGIN_WIRE_MALFORMED`).
- **`display` is the only lenient part.** Manifests, certificates, revocation entries, availability rows and banners may carry a `display` object: any JSON object of at most 8 KiB encoded, kept as received so a relay loses nothing. It is for display only, never changes what the core enforces, and an older reader may drop it.
- **Numbers, times and bytes.** Every number is an integer of magnitude at most 2^53−1; every time is an RFC 3339 UTC timestamp with whole seconds (`2026-01-02T03:04:05Z`), and any other spelling of the same instant is refused; every byte string is standard padded base64.
- **Sorted sets.** Lists that are sets (roles, permissions, codes, files, root key ids, namespaces and so on) must be sorted by bytes without repeats, so a set has exactly one encoding.
- **Codes and parameters.** A `code` is an UPPER_SNAKE code of [ADR 0067](../adr/0067-runtime-outcomes-as-codes.md); `params` has at most 16 entries, with lower_snake keys and values that are strings of at most 512 bytes, integers or booleans.

### Versions and compatibility

| Object | Where its version comes from | Current version | When a reader meets a newer one |
|---|---|---|---|
| Package manifest | `schema` | 1 | `PLUGIN_MANIFEST_TOO_NEW`, with the manifest's `min_core_hint` among its parameters |
| Publisher certificate, trust update, revocation list | `format` | 1 | `PLUGIN_SCHEMA_TOO_NEW` |
| Envelope, handshake, health, error, push and acknowledgement | Protocol major | 1 | `PLUGIN_PROTOCOL_UNSUPPORTED` at the handshake |
| Rows of a pushed table | The push's `schema`, the negotiated capability version `table.<kind>@<n>` | 1 | `PLUGIN_SCHEMA_TOO_NEW` |

- Protocol major 1 is frozen: the shapes of the envelope, handshake, error and push envelope never change.
- Capabilities (`routes`, `table.availability` and so on) carry their own versions; a capability version released within major 1 is never removed.
- A field that changes what the core enforces can only arrive as a new version of its object (in Go, a new type beside the old one), so an older reader refuses it instead of silently dropping a restriction.
- A released golden file is never edited or removed (see [Golden files](#golden-files)).

## Canonical JSON and digests

Signed payloads and pushed-state digests use one byte form, so a signature or a digest names exactly one document. Canonical JSON is UTF-8 JSON with:

- no insignificant whitespace;
- object members sorted by the UTF-8 bytes of their keys, with no duplicate keys;
- integers only, in decimal, without a plus sign, leading zeros, a fraction or an exponent, and zero without a minus sign;
- strings escaped only where JSON requires it: `\"` and `\\`; the controls U+0008, U+0009, U+000A, U+000C and U+000D as `\b`, `\t`, `\n`, `\f` and `\r`; every other control below U+0020 as `\u00xx` in lowercase hex; every other character as its UTF-8 bytes.

A digest is written `sha256:<lowercase hex>`, the SHA-256 of the canonical bytes.

## Signature envelope

Signed documents travel as envelopes: the payload bytes and the signatures over them sit in one file, so a reader never pairs a document with a signature of another version of it. `manifest.sig`, each certificate in `publisher.crt`, and trust updates are envelopes. The shape is the same as the server's release channel envelope.

<!-- golden: envelope/v1/valid/two_signatures.json -->
```json
{"payload":"eyJjcmVhdGVkIjoiMjAyNi0xMS0xN1QwMDowMDowMFoiLCJmaWxlcyI6W3siZXhlYyI6dHJ1ZSwicGF0aCI6ImJpbi9saW51eF9hbWQ2NC9hY21lLWJhY2t1cCIsInNoYTI1NiI6IjUxYTFmMDVhZjg1ZTM0MmUzYzg0OWI0N2QzODcwODY0NzYyODJkNWY1MGRjMjQwYzE5MjE2ZDZlZGZiMWViNWEiLCJzaXplIjo0MDk2fV0sImlkIjoiYWNtZV9iYWNrdXAiLCJwcm90b2NvbCI6eyJtYXgiOjEsIm1pbiI6MX0sInB1Ymxpc2hlciI6ImFjbWUiLCJzY2hlbWEiOjEsInZlcnNpb24iOiIxLjAuMCJ9","signatures":[{"keyid":"acme-2026a","sig":"EREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREQ=="},{"keyid":"acme-hsm-1","sig":"MEQiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIg=="}]}
```

| Field | Rules |
|---|---|
| `payload` | The signed bytes exactly as signed (base64); must be canonical JSON, at most 4 MiB. An encoded envelope is at most 6 MiB and `publisher.crt` at most 24 MiB; anything longer is refused before parsing (`PLUGIN_WIRE_MALFORMED`, rule `range`) |
| `signatures` | 1–8 `{keyid, sig}` entries; no `keyid` repeats within an envelope; each `sig` is 1–512 bytes |

The signatures in the example are filler bytes that only show the shape.

The algorithm belongs to the trusted key, never to the signature: a reader verifies with the algorithm of the key it trusts under that key id, so a signature cannot choose a weaker one.

| `alg` | Public key | Signature |
|---|---|---|
| `ed25519` | A 32-byte Ed25519 public key | A 64-byte pure Ed25519 signature over the payload (RFC 8032) |
| `ecdsa-p256-sha256` | A P-256 point in uncompressed SEC 1 form (65 bytes, `0x04‖X‖Y`) | An ASN.1 DER signature over the SHA-256 of the payload, the form hardware keys and cloud key services return; it must be minimal DER with nothing after it and `s` ≤ n/2 (low-S), or it does not verify, so a signature has one encoding; `Seal` replaces a high `s` a signer returns with n−s |

A key id matches `^[a-z0-9][a-z0-9._-]{1,63}$` and names one key forever; it is never reused. A project root's key id starts with `gwroot-` (`gwroot-1`, `gwroot-2`), and a certified key's with `<publisher>-` (never with `gwroot-`), and `gwroot` is not a valid publisher name, so a root and a publisher key, or two publishers' keys, never share an id. Verification rules:

- Signatures by key ids the reader does not know are skipped, so a signer can sign with the old and the new key before readers switch to trusting the new one.
- When a signature by a trusted key does not verify, the whole envelope fails (`PLUGIN_SIGNATURE_INVALID`): a key id names one key, so a failure is tampering or corruption, never a rotation.
- At least one trusted signature must verify, or the result is `PLUGIN_SIGNATURE_UNTRUSTED`; a payload that is not canonical JSON gives `PLUGIN_WIRE_NOT_CANONICAL`.

## Package manifest

`manifest.json` is the package's manifest, stored as canonical JSON; its bytes are the payload of `manifest.sig`. `publisher.crt` is a JSON array of 1–4 certificate envelopes, so a publisher rotating keys can ship the old and the new certificate together.

<!-- golden: manifest/v1/valid/minimal.json -->
```json
{"created":"2026-11-17T00:00:00Z","files":[{"exec":true,"path":"bin/linux_amd64/acme-backup","sha256":"51a1f05af85e342e3c849b47d387086476282d5f50dc240c19216d6edfb1eb5a","size":4096}],"id":"acme_backup","protocol":{"max":1,"min":1},"publisher":"acme","schema":1,"version":"1.0.0"}
```

An example that uses every field is `manifest/v1/valid/full.json` in the corpus.

| Field | Rules |
|---|---|
| `schema` | 1 |
| `id` | The package id, `^[a-z][a-z0-9_]{1,31}$`; also the route segment `/api/v1/x/<id>/` and a directory name. It must belong to `publisher`: a third party's ids start with `<publisher>_`, and the project publisher's have no `_` or start with `gpuworks_`, so a project id never equals a third party's; which prefixes it may use is a grant of its certificate |
| `publisher` | `^[a-z][a-z0-9]{1,15}$` (no underscore, so `<publisher>_` splits a package id unambiguously; `gwroot` is reserved for the root keys); must equal the subject of the signing certificate |
| `version` | A semantic version without a leading `v` |
| `min_core_hint` | Optional, at most 32 bytes; names the server release needed, for messages only. Compatibility is decided by `requires` |
| `created` | Must lie inside the signing certificate's signing window |
| `protocol` | The range of `gwx` majors the package speaks, `{min, max}` |
| `requires` | Capabilities the package cannot run without → the lowest version it needs; a server that lacks one judges the package incompatible and names the capability |
| `uses` | Optional capabilities → the highest version the package can use, negotiated down at run time; a capability cannot also appear in `requires` |
| `roles` | Privileged roles the package claims, sorted; a claim counts only if the certificate grants it |
| `permissions` | What the administrator approves, `<verb>:<object>`, sorted; the only verbs are `events`, `http`, `metrics`, `node`, `notify`, `push`, `read` and `svc` |
| `features` | Declared feature keys `{key, presentation, gate}`: `presentation` is `promote` (shown as locked) or `conceal` (hidden); `gate` is `none` (always full), `self` (this package answers) or `authority` (only the namespace authority answers) |
| `routes` | The package's HTTP routes; see the table below |
| `ui` | The console bundle: `mode` is `module` (a same-origin ES module, which needs the certificate grant) or `iframe`; `entry` is a member under `ui/`; `sdk` is the range of console SDK majors; `nav` are menu entries |
| `node` | The node part: `app` is the application description under `node/`, `images` are image archives for offline sites under `node/images/` |
| `runtime` | The server part: `lifecycle` is `resident` (started with the server) or `on_demand` (started on use, stopped when idle, owns no tables); `mem_mib`, `cpu_milli` and `max_concurrency` (≤1024) take defaults at 0; `devices` are device nodes under `/dev`, sorted |
| `storage` | The data directory: `kind` is `sqlite` or `files`; `ha` is `shipped` (replicated with the server database) or `leader_local` (a rebuildable cache); `state_version` is the data format version this build writes (≥1); `cold_copy_safe` means the directory may be copied while the plugin is stopped |
| `host_setup` | Host steps from a closed vocabulary, never a script; today only `usb_device` (`vendor_id` and `product_id`, four lowercase hex digits each) |
| `codes` | Every code the package can emit, sorted; each has en and zh text in the package's locales |
| `files` | Every member except `manifest.json`, `manifest.sig` and `publisher.crt`, sorted by path: `{path, size, sha256, exec}`; paths are relative, every segment starts with a letter, digit or underscore (no `.`, `..` or hidden files), at most 512 bytes |

When a part of the manifest is present, `requires` must list its capability, so a server without it refuses the package by capability instead of mishandling it:

| Part | Must `require` |
|---|---|
| Non-empty `routes` | `routes` |
| `ui` | `ui.bundle` |
| `node` | `node.app` |
| Non-empty `host_setup` | `host.setup` |

Routes are served at `/api/v1/x/<id><pattern>`:

| Field | Rules |
|---|---|
| `method` | `GET`, `POST`, `PUT`, `PATCH` or `DELETE`; `method` plus `pattern` is unique |
| `pattern` | Starts with `/`, chi syntax, parameters written `{name}` |
| `action` | `<package id>.<verb>`, the action name an authorizer grants |
| `access` | `admin` (administrators, by session or platform token), `admin_session` (administrator sessions only), `member` (any signed-in user or platform token), `public` (no authentication) |
| `feature` / `ungated` | Exactly one is set; `feature` must be a key this manifest declares |
| `write_when_read_only` | Lets this non-GET write through while the feature is read-only (a write that only removes or relaxes); needs `feature` |
| `max_body` | Request body limit; 0 means 1 MiB, at most 1 GiB; above 1 MiB the package needs the permission `http:uploads` |
| `timeout_s` | 0 means 60 seconds, at most 600 |
| `stream` / `websocket` | A streamed response / a WebSocket upgrade (GET only, not together with `stream`) |
| `ai_readable` | The assistant and MCP may read it; GET only |
| `cookies` / `rate` | `public` routes only: may set cookies (a single sign-on callback) / a token bucket per client address `{per_minute, burst}`, each 1–100000 |

## Publisher certificate

A certificate is a publisher's signing key and its grants. It travels as the payload of an envelope signed by a project root key (or, for a publisher the host operator trusts, as the operator configures). The issuer is the key id whose signature verified; the payload has no issuer field. A certificate for the project publisher, or one granting the bare namespace or `ui_module`, needs a project root's signature to verify; signed only by issuers the host operator configures, it fails with `PLUGIN_SIGNATURE_UNTRUSTED`, whose `field` parameter names the grant.

<!-- golden: publisher_cert/v1/valid/authority.json -->
```json
{"alg":"ed25519","format":1,"grants":{"authority_id":"acme_quota","feature_ns":["acme"],"id_prefixes":["acme_"],"min_isolation":"landlock","roles":["availability_authority","clock_authority"],"tables":["fleet_capacity"]},"keyid":"acme-2026a","not_after":"2027-02-01T00:00:00Z","not_before":"2026-11-17T00:00:00Z","public_key":"1+5QRW1qr6d5EuAxQt77BRh28RH6+gBNDvAKPKdkHV4=","publisher":"acme","serial":17}
```

| Field | Rules |
|---|---|
| `format` | 1 |
| `serial` | Unique among all certificates of the publisher, whichever root issued them, so `publisher` + `serial` names one certificate; ≥1 |
| `publisher` | The subject; manifests signed with this key must name it |
| `keyid` / `alg` / `public_key` | The publisher's signing key, with the rules of the [signature envelope](#signature-envelope); `keyid` starts with `<publisher>-` and not with `gwroot-` |
| `not_before` / `not_after` | The signing window; `not_after` must be later than `not_before`. A package whose `created` lies in the window stays valid after `not_after`, so installed packages never expire |
| `grants.id_prefixes` | Required: the package ids the key may sign, sorted, each with the package id grammar. An entry ending in `_` covers every id that starts with it; any other entry covers exactly that id. Every entry belongs to the subject, by the rule for a manifest's `id` (a third party's start with `<publisher>_`; the project publisher's have no `_` or start with `gpuworks_`), so a certificate never covers another publisher's ids (`acme_` does not cover `acmebank_pay`, and a project certificate cannot list `acme_`) |
| `grants.feature_ns` | Namespaces its packages may declare feature keys in, sorted: only the publisher's own name, plus `""` (the bare namespace) for the project publisher |
| `grants.authority_id` | The one package id that may answer `gate: authority` keys of `feature_ns`; needs `feature_ns` and must be covered by `id_prefixes` |
| `grants.roles` / `grants.tables` | Privileged roles its packages may claim / privileged tables they may push, sorted |
| `grants.ui_module` | Its packages may ship same-origin console modules |
| `grants.min_isolation` | Required: `landlock` (runs only where Landlock confines it) or `none` |

The project publisher is named `gpuworks`; its own packages use bare feature keys and ids without a publisher prefix:

<!-- golden: publisher_cert/v1/valid/project.json -->
```json
{"alg":"ed25519","format":1,"grants":{"authority_id":"quota","feature_ns":["","gpuworks"],"id_prefixes":["quota","sso"],"min_isolation":"landlock","roles":["availability_authority","clock_authority"],"tables":["fleet_capacity"]},"keyid":"gpuworks-2026a","not_after":"2027-02-01T00:00:00Z","not_before":"2026-11-17T00:00:00Z","public_key":"0RZF/KSeB5I5emmRFX7vHLklBOUA2A8ot0mLncySqxs=","publisher":"gpuworks","serial":1}
```

## Trust updates and revocation lists

A trust update is signed by at least one project root key the reader trusts now, and can be imported offline. A reader keeps the highest `serial` it has applied and refuses a lower or equal one, so an old update cannot be replayed.

<!-- golden: trust_update/v1/valid/rotate.json -->
```json
{"add_roots":[{"alg":"ed25519","keyid":"gwroot-3","public_key":"eC0hm7/hzMkrq452w7FbsvdumRZzFEroSPiJYIFGCqs="},{"alg":"ecdsa-p256-sha256","keyid":"gwroot-4","public_key":"BMX+c4VTtfKGKA3l0EjO3d6IXwcBTVbSabaijGatYQCn+OSfRFHDO2UzUlWZu07cpYIEmdreHZH0eYnGPrSojuY="}],"created":"2026-12-01T00:00:00Z","floors":[{"id":"acme_backup","min_version":"1.1.0"},{"id":"acme_quota","min_version":"2.0.3"}],"format":1,"retire_roots":["gwroot-1"],"revoke":[{"keyid":"acme-2025a","publisher":"acme"},{"cert_serial":9,"publisher":"acme"}],"serial":3}
```

| Field | Rules |
|---|---|
| `format` / `serial` / `created` | 1 / an increasing serial / when it was issued |
| `add_roots` | Root public keys to trust from now on, sorted by key id, each key id starting with `gwroot-`; none may also be in `retire_roots` |
| `retire_roots` | Root key ids to stop trusting, sorted, each starting with `gwroot-` |
| `revoke` | Revocation entries, see below; at most 4096 |
| `floors` | Raise the lowest installable version of packages, `{id, min_version}`, sorted by id; a floor only rises |

At least one of the four lists is non-empty. Every revocation names its `publisher`, which scopes it, and at most one of `cert_serial`, `keyid` and `manifest` narrows it; an entry does not repeat within a list:

| Selector | Revokes |
|---|---|
| `publisher` | Every certificate of the publisher |
| `publisher` + `cert_serial` | One certificate |
| `publisher` + `keyid` | One of the publisher's keys, in every certificate that certifies it; the key id starts with `<publisher>-` |
| `publisher` + `manifest` | One package build of the publisher, by manifest digest, applied only to a package whose manifest names that publisher; only this kind of entry may carry `replacement` (the version to install instead) |

Revoking by manifest digest rather than by package file hash means re-compressing the tar does not escape a revocation.

Revocation lists are cumulative: a reader keeps the highest `serial` per issuer. A list issued under a project root has no `publisher`; a publisher may issue one about its own certificates, keys and builds, naming itself, and then every entry must name that same publisher, so one publisher's list never revokes another's keys or builds. `revoked` must not be empty.

<!-- golden: crl/v1/valid/root.json -->
```json
{"created":"2026-12-01T00:00:00Z","format":1,"revoked":[{"cert_serial":9,"publisher":"acme"},{"keyid":"acme-2025a","publisher":"acme"}],"serial":12}
```

## gwx/1 transport and handshake

Under ADR 0071, a plugin is a child process of the server and runs plaintext HTTP/2 over each of two inherited socketpairs: on one the plugin serves the server, on the other it calls the host API. These constants are frozen with major 1 in `pkg/extapi`:

| Name | Value | Purpose |
|---|---|---|
| Environment variable `GWX_PROTOCOL` | The major | The protocol major in the plugin's environment |
| Environment variables `GWX_SERVE_FD` / `GWX_HOST_FD` | Descriptor numbers | Where the plugin serves / calls the host API |
| `POST /gwx/v1/initialize` | Handshake | The server's first call to the plugin |
| `/gwx/v1/shutdown` | Stop | Asks the plugin to exit |
| `GET /gwx/v1/health` | Health | The server's periodic probe |
| `PUT /host/v1/state/{kind}` | Push | The plugin pushes the whole table of one kind |
| Request header `Gwx-Deadline-Ms` | Unix milliseconds | The call's deadline |
| Response header `Gwx-Product-Error` | — | Marks a plugin route's response body as an error object |

The handshake request:

<!-- golden: handshake_request/v1/valid/first_start.json -->
```json
{"clock":{"extclock":"2026-12-01T00:00:00Z","wall":"2026-12-01T00:00:00Z"},"core":{"arch":"amd64","ha_role":"leader","install_id":"inst_7f3a9c2e41b8","offer":{"host.events":{"max":1,"min":1},"host.fleet":{"max":1,"min":1},"routes":{"max":1,"min":1},"table.attestations":{"max":1,"min":1},"table.availability":{"max":1,"min":1},"table.banners":{"max":1,"min":1},"table.fleet_capacity":{"max":1,"min":1},"table.node_holds":{"max":1,"min":1}},"os":"linux","version":"v26.12.0"},"plugin":{"id":"acme_quota","permissions_granted":["http:routes","push:banners","read:fleet"],"version":"2.0.4"},"principal_key":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=","protocol":{"max":1,"min":1}}
```

| Field | Rules |
|---|---|
| `protocol` | The range of majors the server speaks; without 1 the result is `PLUGIN_PROTOCOL_UNSUPPORTED` |
| `core` | `version`, `install_id`, `os`, `arch`, `ha_role` (`leader` or `single`; plugins run only where the server leads), and `offer`: every capability the server offers → its version range, never empty |
| `plugin` | What the administrator approved: `id`, `version`, `permissions_granted`, `roles_granted`, `authority_namespaces` (namespaces this package is the authority for, `""` being the bare namespace); every list is sorted |
| `held` | Each table the server holds for this package, `{kind, schema, generation, digest}`, with unique kinds; the plugin may skip a push whose digest matches |
| `clock` | The `wall` clock and `extclock` (the clock timelines follow, which never runs backwards) |
| `principal_key` | A fresh 32-byte key per spawn, which authenticates the principal headers on proxied routes |

The plugin's answer:

<!-- golden: handshake_response/v1/valid/initialized.json -->
```json
{"id":"acme_quota","manifest_sha256":"sha256:69a8fc5cf9fa0174e3dfe74b1c9c1f5f4f64710afa362419883f19c6b42ee422","protocol":1,"services":["backup","support"],"use":{"host.fleet":1,"table.attestations":1,"table.availability":1,"table.banners":1,"table.fleet_capacity":1},"version":"2.0.4"}
```

`protocol` is the major the plugin chose from the server's range (only 1 today); `manifest_sha256` is the digest of the manifest the plugin was built with, by which the server refuses a binary that does not belong to its package; `use` maps each capability the plugin will use to the version it chose, which must lie within the server's `offer`; `services` are the optional services the plugin serves, sorted.

## Health and errors

The answer to a health probe: `status` is `ok` or `degraded`; `degraded` must carry a `code` that says why, and `params` may only appear with a `code`.

<!-- golden: health/v1/valid/degraded.json -->
```json
{"code":"ACME_QUOTA_STORE_LOCKED","params":{"path":"data/quota.db","retries":3},"status":"degraded"}
```

Every `gwx/1` error, in both directions, and every product error a plugin route returns, is the same object:

<!-- golden: error/v1/valid/busy.json -->
```json
{"code":"PLUGIN_BUSY","message":"the plugin is busy","reason":"busy","retry_after_ms":500}
```

| Field | Rules |
|---|---|
| `code` / `params` | The ADR 0067 code and parameters |
| `message` | The required English fallback, at most 2048 bytes |
| `reason` | A handling class independent of the code, closed within major 1: `invalid_argument`, `permission_denied`, `not_found`, `failed_precondition`, `conflict`, `busy`, `unavailable`, `unsupported`, `cancelled`, `deadline_exceeded`, `internal` |
| `retry_after_ms` | A suggested retry delay, 0–86400000, only with `busy` and `unavailable` |

## Pushes and acknowledgements

A plugin pushes the whole table of one kind to the server. The owner is the package bound to the connection; no field of the push names it. A whole push is at most 16 MiB encoded.

<!-- golden: state_push/v1/valid/empty_table.json -->
```json
{"body":{},"generation":1,"kind":"node_holds","schema":1}
```

| Field | Rules |
|---|---|
| `kind` | The table kind, the same as the path segment |
| `schema` | The version of the rows in `body`, the `table.<kind>@<schema>` the plugin negotiated |
| `generation` | Strictly increasing per owner and kind, ≥1 |
| `base` | Only for kinds that define a patch form; no kind of major 1 does, so it must be absent |
| `body` | A JSON object, decoded strictly at `kind` and `schema` |

The server's acknowledgement says either that it applied the push (`applied_at` and `digest`) or that a newer push of the same owner and kind replaced it before it was committed (`superseded_by`, greater than `generation`); exactly one of the two:

<!-- golden: state_ack/v1/valid/applied.json -->
```json
{"applied_at":"2026-12-01T00:00:00Z","digest":"sha256:b4267ce93cbba4e415504feb895f662dcced5d7aa406f27e8447c8fd5f0d48c8","generation":42,"kind":"availability"}
```

<!-- golden: state_ack/v1/valid/superseded.json -->
```json
{"generation":42,"kind":"availability","superseded_by":43}
```

Each kind belongs to a class, which decides how the server combines the rows of several owners:

| Class | Kinds | Combination |
|---|---|---|
| Restrict | `fleet_capacity`, `node_holds` | Any approved owner may push; rows combine by intersection and do not lapse while the owner is down |
| Grant | `availability` | Only from the namespace authority (or, for `gate: self` keys, the declaring package); tightens once the owner has been down longer than the grace |
| Display | `banners` | Shown, never enforced |
| Opaque | `attestations` | Stored and relayed, never interpreted |

## Pushed tables

A row's `notice` is a code with parameters, and the code must be listed in the pushing package's manifest `codes`. Timelines are made of steps: only the first step may omit `at` (it holds from the push), later `at` values strictly increase, and a timeline has 1–64 steps; the server follows the steps on its own clock, and they take effect even while the plugin is down.

### `table.availability@1`

The availability timeline of each feature key the pusher is the authority for (or declared with `gate: self`).

<!-- golden: availability/v1/valid/authority.json -->
```json
{"down_grace_s":3600,"rows":[{"down_access":"read_only","feature":"acme.backup","steps":[{"access":"full","limits":[{"dim":"concurrent_jobs","max":4},{"dim":"gpus","unlimited":true}]},{"access":"full","at":"2027-01-01T00:00:00Z","link":"/x/acme_quota/renew","notice":{"code":"ACME_QUOTA_GRACE","params":{"days":30}}},{"access":"read_only","at":"2027-02-01T00:00:00Z","link":"https://acme.example/renew","notice":{"code":"FEATURE_EXPIRED"}}]},{"display":{"plan":"pro"},"down_access":"locked","feature":"acme.reports","steps":[{"access":"locked","notice":{"code":"ACME_NOT_PURCHASED"}}]}]}
```

- `down_grace_s`: how many seconds the rows stay as pushed after the owner goes down; 0 takes the server's default, otherwise 60–86400, and a site may only lower it.
- `rows`: at most 4096, with unique `feature`; `down_access` is `read_only` or `locked`, the ceiling the row tightens to after the grace.
- Steps: `access` is `full`, `read_only`, `locked` or `hidden`; an optional `notice`; `link` is an https URL or a console path (at most 2048 bytes); at most 16 `limits`, sorted by `dim`, each setting exactly one of `max` and `unlimited`.

### `table.fleet_capacity@1`

Fleet caps: the server refuses enrollments over a cap and holds over-cap nodes from scheduling. The pusher sends caps only; the server picks the nodes.

<!-- golden: fleet_capacity/v1/valid/authority.json -->
```json
{"hold":{"code":"ACME_OVER_CAPACITY"},"refuse_card":{"code":"ACME_CARD_LIMIT","params":{"max":64}},"refuse_node":{"code":"ACME_NODE_LIMIT","params":{"max":8}},"select":"enrollment_order","steps":[{"max_cards":64,"max_nodes_with_cards":8},{"at":"2027-02-01T00:00:00Z","max_cards":16,"max_nodes_with_cards":2}]}
```

`select` is only `enrollment_order` today (keep nodes in the order they joined and hold the rest once a cap is full); `refuse_node` and `refuse_card` explain a refused enrollment, and `hold` is the reason shown on a held node; an absent `max_nodes_with_cards` or `max_cards` in a step means no cap.

### `table.node_holds@1`

Nodes the pusher holds from scheduling (maintenance, quarantine), each with its reason. A hold never stops running work; rows naming a node the server does not have are kept and match nothing.

<!-- golden: node_holds/v1/valid/maintenance.json -->
```json
{"nodes":{"12":{"code":"ACME_MAINTENANCE","params":{"ticket":"OPS-118"}},"9":{"code":"ACME_QUARANTINE"}}}
```

The keys of `nodes` are the server's node ids in decimal (never reused), at most 10000.

### `table.banners@1`

Banners above every console page, at most 16.

<!-- golden: banners/v1/valid/authority.json -->
```json
{"banners":[{"audience":"admin","id":"grace","steps":[{"link":"/x/acme_quota","notice":{"code":"ACME_QUOTA_GRACE","params":{"days":30}},"severity":"warning"},{"at":"2027-02-01T00:00:00Z","hidden":true}]},{"audience":"all","display":{"icon":"key"},"id":"usb_key_missing","steps":[{"at":"2026-12-01T00:00:00Z","notice":{"code":"ACME_USB_KEY_MISSING"},"severity":"danger"}]}]}
```

`id` is the owner's own and unique; `audience` is `admin` or `all`; each step is either `hidden: true` (with no other field) or carries a `severity` (`info`, `warning`, `danger`) and a `notice`, plus an optional `link`.

### `table.attestations@1`

Opaque statements the server stores and relays to the same publisher's other packages, never interpreting them.

<!-- golden: attestations/v1/valid/authority.json -->
```json
{"attestations":[{"blob":"c3RhdGVtZW50LTE=","feature":"acme.backup","format":"acme.grant-statement.v1","name":"grant_backup"},{"blob":"c3RhdGVtZW50LTI=","feature":"acme.reports","format":"acme.grant-statement.v1","name":"grant_reports"},{"blob":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8gISIjJCUmJygpKissLS4vMDEyMzQ1Njc4OTo7PD0+P0BBQkNERUZHSElKS0xNTk9QUVJTVFVWV1hZWltcXV5fYGFiY2RlZmdoaWprbG1ub3BxcnN0dXZ3eHl6e3x9fn+AgYKDhIWGh4iJiouMjY6PkJGSk5SVlpeYmZqbnJ2en6ChoqOkpaanqKmqq6ytrq+wsbKztLW2t7i5uru8vb6/wMHCw8TFxsfIycrLzM3Oz9DR0tPU1dbX2Nna29zd3t/g4eLj5OXm5+jp6uvs7e7v8PHy8/T19vf4+fr7/P3+/w==","format":"acme.binding.v1","name":"install_binding"}]}
```

At most 1024; `name` is unique; `feature` is optional; `format` tells a reader how to verify `blob`; `blob` is non-empty and at most 64 KiB.

## Error codes

Every decoding, validation and verification failure is an error with a stable code and parameters; the English message is only a fallback, and the console carries en and zh text for every code.

| Code | Meaning | Parameters |
|---|---|---|
| `PLUGIN_WIRE_MALFORMED` | Not one JSON value of the expected shape | `object`, `field`, `rule` |
| `PLUGIN_WIRE_UNKNOWN_FIELD` | A field this version does not define | `object`, `field` |
| `PLUGIN_WIRE_INVALID` | A field breaks a rule of its object | `object`, `field`, `rule` |
| `PLUGIN_WIRE_NOT_CANONICAL` | A signed payload is not canonical JSON | `object` |
| `PLUGIN_SCHEMA_TOO_NEW` | The object's version is newer than this build knows | `object`, `version`, `known` |
| `PLUGIN_MANIFEST_TOO_NEW` | The manifest schema is newer than this build knows | `version`, `known`, `min_core_hint` |
| `PLUGIN_PROTOCOL_UNSUPPORTED` | The peer's protocol range lacks this build's major | `min`, `max`, `supported` |
| `PLUGIN_SIGNATURE_UNTRUSTED` | No signature by a trusted key | `object` |
| `PLUGIN_SIGNATURE_INVALID` | A signature by a trusted key does not verify | `object`, `keyid` |
| `PLUGIN_STATE_REJECTED` | A pushed table row breaks a rule of its kind | `kind`, `field`, `rule`, `row` |
| `PLUGIN_STATE_TOO_LARGE` | A pushed table exceeds a size limit of its kind | `kind`, `field`, `max` |
| `PLUGIN_STATE_TOO_NEW` | A persisted version marker is newer than this build knows | `marker`, `version`, `known`, `needs`, `count` |

Values of `rule`: `required` (missing or empty), `grammar` (does not match its grammar), `range` (a number or length out of bounds), `enum` (not in the closed set), `duplicate`, `order` (a list that must be sorted is not), `exclusive` (sets fields that exclude each other), `relation` (contradicts another field of the object), `syntax`, `type`, `number` (not an integer, or outside the safe range) and `trailing`. `field` is a dotted path with list indexes, for example `routes.0.feature`.

`PLUGIN_STATE_TOO_NEW` comes from `extapi.CheckMarkers`: it compares persisted version markers (name, version, and the server version that wrote it) with the highest versions this build reads, and `needs` is the newest server version among the writers of those markers, the version to run instead.

## Golden files

`pkg/extapi/testdata/golden/<kind>/v1/` is the frozen wire corpus:

- every file under `valid/` decodes, validates and re-encodes to the same bytes;
- every file under `invalid/<CODE>/` fails with exactly that code; a name `<field>--<rule>[--note].json` also gives the failing field and rule, and `_root` means the object itself;
- `testdata/golden/SHA256SUMS` may only grow: a released golden file is never edited or removed, which `make check-architecture` enforces against its baseline; a new version of an object gets its own `v2/` directory.

From the repository root, `make test-extapi` runs the format check, vet and tests of this module; `make test` includes it.

<!-- translation-source-sha256: df08575c47ce6c70957015b9b3a66d815473685b2686cc385a53f9b2bf443dfc -->
