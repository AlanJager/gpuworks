// Package extapi defines the wire formats shared by the GPUWorks server and
// the plugins it runs: the package manifest, publisher certificates, trust
// updates and revocation lists, the signature envelope, the gwx/1 handshake,
// error and push envelopes, the rows of the pushed tables, and the
// principal a plugin route request carries.
//
// Plugins and the server both import this package, and neither may change
// what it accepts once released, because a site may run a server that is
// never upgraded next to plugins built years later, or the reverse. The
// rules that keep that working:
//
//   - Protocol major 1 is frozen: the envelope, handshake, error and push
//     envelope shapes never change. Growth happens through capabilities.
//   - Every wire object carries a version (schema, format or the negotiated
//     capability version). A decoder accepts only versions it knows and
//     refuses unknown fields at them: a field the reader would ignore could
//     carry a restriction the reader would then silently drop.
//   - Any field that changes what the server enforces therefore needs a new
//     version of its object, defined as a new Go type beside the old one. A
//     version, once released, is never removed.
//   - Display-only data that an older reader may drop goes in a Display
//     object, the only part of a wire object decoded leniently.
//
// Every number on the wire is an integer of magnitude at most 2^53-1, every
// time is an RFC 3339 UTC timestamp with whole seconds ("2026-01-02T03:04:05Z"),
// and every byte string is standard padded base64.
//
// # Canonical JSON
//
// Signed objects and state digests use one byte form, so that a signature or
// a digest names exactly one document. Canonical JSON is UTF-8 JSON with:
//
//   - no insignificant whitespace;
//   - object members sorted by key, comparing the keys' UTF-8 bytes;
//   - no duplicate keys;
//   - integers only, written in decimal without a sign for zero, a plus
//     sign, leading zeros, a fraction or an exponent;
//   - strings escaped only where JSON requires it: quotation mark and reverse
//     solidus as \" and \\, the controls U+0008, U+0009, U+000A, U+000C and
//     U+000D as \b, \t, \n, \f and \r, every other control below U+0020 as
//     \u00xx with lowercase hex; every other character as its UTF-8 bytes.
//
// A signed payload must already be canonical: the signature covers the
// payload bytes exactly as carried, and a reader refuses a payload that is
// not in canonical form, so one document has one digest.
//
// # Errors
//
// Every decoding, validation and verification failure is an *Error with a
// stable UPPER_SNAKE code and parameters (ADR 0067); the English message is
// a fallback only. Programming errors on the signing side (an unsupported
// key type) and malformed version strings are plain errors.
package extapi
