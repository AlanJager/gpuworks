// Package extpkg is the plugin host's package handling: it verifies signed
// plugin packages (.gwpkg) fail-closed against the server's trust, stages
// them into the data directory and swaps them into place, lists and removes
// them, and checks the persisted version markers a downgraded server must
// not start past. Packages are read by extpkg/archive, trust lives in
// extpkg/trust, the data directory in extpkg/layout and the marker checks in
// extpkg/compat. The formats are docs/reference/plugin-protocol.md; the
// design is docs/adr/0071-installable-extensions.md.
package extpkg

import (
	"bytes"
	"fmt"
	"runtime"
	"slices"
	"sort"

	"gpuworks.ai/gpuworks/internal/server/extpkg/archive"
	"gpuworks.ai/gpuworks/internal/server/extpkg/trust"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Platform is an operating system and architecture, as Go names them.
type Platform struct{ OS, Arch string }

// HostPlatform is the platform this server runs on.
func HostPlatform() Platform { return Platform{OS: runtime.GOOS, Arch: runtime.GOARCH} }

// Mode says why a package is verified.
type Mode int

const (
	// Install verifies a package about to be installed, upgraded to or
	// rolled back to: a revoked build or a version below the floor is
	// refused.
	Install Mode = iota
	// Recheck verifies an installed package at boot and before each start.
	// A revocation never stops an installed package by itself (the result
	// carries it for the caller to act on), and floors apply to installs
	// only.
	Recheck
)

// Trust classes of a verified package.
const (
	TrustProject = "project" // certified by a project root
	TrustHost    = "host"    // certified by an issuer the host operator trusts
)

// Verifier verifies packages against one view of the server's trust,
// capabilities and site policy.
type Verifier struct {
	Trust *trust.Set
	// Offered are the capabilities this server offers, name to highest
	// version (Capabilities()).
	Offered map[string]int
	// Policy is the site's narrowing policy (server.yaml plugins.policy).
	Policy Policy
	// Platform is the platform whose server executable must exist.
	Platform Platform
	// Isolation is the strongest isolation this host can apply to a plugin
	// process: extapi.IsolationLandlock or extapi.IsolationNone.
	Isolation string
}

// Verified is a package that passed verification.
type Verified struct {
	Manifest *extapi.Manifest
	// ManifestBytes are manifest.json exactly as signed; Digest is theirs.
	ManifestBytes []byte
	Digest        extapi.Digest
	// Cert is the certificate the package is signed under; Anchor and Trust
	// say who certified it; Grants are what that certificate may grant
	// under its anchor.
	Cert   *extapi.PublisherCert
	Anchor string
	Trust  string
	Grants extapi.CertGrants
	// Binary is the server part's executable for the verifier's platform,
	// or "" when the package has no server part.
	Binary string
	// Revoked is the revocation of this build or of its certificate, when
	// one applies; only Recheck returns a revoked package.
	Revoked *extapi.Revocation
}

// Authority reports whether the package is its namespace's authority: the
// certificate names it and it claims the authority role.
func (v *Verified) Authority() bool {
	return v.Grants.AuthorityID == v.Manifest.ID && slices.Contains(v.Manifest.Roles, AuthorityRole)
}

func refuse(code, fallback string, params map[string]any) error {
	return messages.NewProductError(code, fallback, params)
}

// Verify runs the verification order of docs/reference/plugin-protocol.md
// on the contents of a package and returns the first failure.
func (v *Verifier) Verify(c *archive.Contents, mode Mode) (*Verified, error) {
	// 1-3: the certificates against the roots and host issuers, their
	// revocations, and the manifest signature by a certified key.
	signers, err := v.openSigners(c)
	if err != nil {
		return nil, err
	}
	if mode == Install {
		signers = signers.withoutRevoked()
	}
	sig, err := extapi.DecodeEnvelope(c.Signature)
	if err != nil {
		return nil, err
	}
	keys := extapi.KeySet{}
	for _, s := range signers.valid {
		keys[s.cert.KeyID] = s.cert.Key()
	}
	verified, err := sig.Verify("manifest", keys)
	if err != nil {
		if r := signers.revocationFor(sig); r != nil {
			return nil, refuse(messages.ErrorPluginCertRevoked, "the certificate that signed the package is revoked",
				map[string]any{"publisher": r.cert.Publisher, "serial": r.cert.Serial, "keyid": r.cert.KeyID})
		}
		return nil, err
	}
	// 4: the signed payload is manifest.json byte for byte.
	if !bytes.Equal(verified.Payload, c.Manifest) {
		return nil, refuse(messages.ErrorPluginManifestMismatch, "manifest.json is not the signed manifest", map[string]any{})
	}
	// 5: strict decode at a schema this server knows.
	m, err := extapi.DecodeManifest(c.Manifest)
	if err != nil {
		return nil, err
	}
	out := &Verified{Manifest: m, ManifestBytes: c.Manifest, Digest: extapi.DigestOf(c.Manifest)}
	// 6-8 against each certificate whose key signed, the first that passes.
	var firstErr error
	for _, id := range verified.KeyIDs {
		s := signers.byKey(id)
		if err := v.checkCert(m, s); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out.Cert, out.Anchor, out.Grants = s.cert, s.anchor, v.Trust.Grants(s.cert, s.anchor)
		out.Revoked = s.revoked
		break
	}
	if out.Cert == nil {
		return nil, firstErr
	}
	out.Trust = TrustHost
	if out.Anchor == trust.RootAnchor {
		out.Trust = TrustProject
	}
	if r := v.Trust.ManifestRevocation(m.Publisher, out.Digest); r != nil {
		if mode == Install {
			return nil, refuse(messages.ErrorPluginPackageRevoked, fmt.Sprintf("%s %s is revoked", m.ID, m.Version),
				map[string]any{"id": m.ID, "version": m.Version, "replacement": r.Replacement})
		}
		out.Revoked = r
	}
	if err := checkGrants(m, out.Grants); err != nil {
		return nil, err
	}
	// 9: files.
	if out.Binary, err = checkFiles(m, c.Members, v.Platform); err != nil {
		return nil, err
	}
	// 10: capabilities, then this server's own policy.
	if err := checkCapabilities(m, v.Offered); err != nil {
		return nil, err
	}
	if err := checkHostSetupPlatform(m, v.Platform); err != nil {
		return nil, err
	}
	if err := checkServerPolicy(m); err != nil {
		return nil, err
	}
	// 11: site policy.
	if err := v.Policy.check(m, out, v.Isolation); err != nil {
		return nil, err
	}
	// 12: the anti-rollback floor.
	if mode == Install {
		if err := checkFloor(m, v.Trust.Floor(m.ID)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type signer struct {
	cert    *extapi.PublisherCert
	anchor  string
	revoked *extapi.Revocation
}

type signers struct {
	valid   []signer // opened and usable to verify the manifest
	revoked []signer // opened but revoked
}

func (s signers) byKey(id string) signer {
	for _, x := range s.valid {
		if x.cert.KeyID == id {
			return x
		}
	}
	return signer{}
}

// revocationFor returns a revoked certificate whose key signed env.
func (s signers) revocationFor(env *extapi.Envelope) *signer {
	for _, r := range s.revoked {
		for _, sig := range env.Signatures {
			if sig.KeyID == r.cert.KeyID && r.cert.Key().Verify(env.Payload, sig.Sig) {
				return &r
			}
		}
	}
	return nil
}

// openSigners opens every certificate in publisher.crt and marks the
// revoked ones; Verify leaves those out of the signing keys on install.
func (v *Verifier) openSigners(c *archive.Contents) (signers, error) {
	bundle, err := extapi.DecodeCertBundle(c.Certificates)
	if err != nil {
		return signers{}, err
	}
	var out signers
	var firstErr error
	for i := range bundle {
		cert, issuers, err := extapi.OpenPublisherCert(&bundle[i], v.Trust.Issuers())
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		s := signer{cert: cert, anchor: v.Trust.Anchor(issuers)}
		if r := v.Trust.CertRevocation(s.anchor, cert); r != nil {
			s.revoked = r
			out.revoked = append(out.revoked, s)
		}
		out.valid = append(out.valid, s)
	}
	if len(out.valid) == 0 {
		return signers{}, firstErr
	}
	return out, nil
}

// withoutRevoked drops revoked certificates from the signing keys.
func (s signers) withoutRevoked() signers {
	var keep []signer
	for _, x := range s.valid {
		if x.revoked == nil {
			keep = append(keep, x)
		}
	}
	s.valid = keep
	return s
}

// checkCert is steps 6 and 7 against one certificate.
func (v *Verifier) checkCert(m *extapi.Manifest, s signer) error {
	c := s.cert
	if m.Publisher != c.Publisher {
		return refuse(messages.ErrorPluginPublisherMismatch, fmt.Sprintf("manifest publisher %s, certificate %s", m.Publisher, c.Publisher),
			map[string]any{"publisher": m.Publisher, "cert_publisher": c.Publisher})
	}
	if !c.CoversID(m.ID) {
		return refuse(messages.ErrorPluginIDNotGranted, fmt.Sprintf("certificate of %s does not cover %s", c.Publisher, m.ID),
			map[string]any{"id": m.ID, "publisher": c.Publisher})
	}
	if m.Created.Before(c.NotBefore.Time) || m.Created.After(c.NotAfter.Time) {
		return refuse(messages.ErrorPluginCreatedOutsideWindow, "package created outside its certificate's signing window",
			map[string]any{"created": m.Created.String(), "not_before": c.NotBefore.String(), "not_after": c.NotAfter.String()})
	}
	return nil
}

func checkCapabilities(m *extapi.Manifest, offered map[string]int) error {
	if !m.Protocol.Contains(extapi.ProtocolMajor) {
		return &extapi.Error{Code: extapi.ErrorProtocolUnsupported,
			Params:  map[string]any{"min": m.Protocol.Min, "max": m.Protocol.Max, "supported": extapi.ProtocolMajor},
			Message: fmt.Sprintf("package speaks gwx/%d..%d, this server speaks gwx/%d", m.Protocol.Min, m.Protocol.Max, extapi.ProtocolMajor)}
	}
	names := make([]string, 0, len(m.Requires))
	for name := range m.Requires {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if offered[name] < m.Requires[name] {
			return refuse(messages.ErrorPluginIncompatible, fmt.Sprintf("package requires %s@%d, this server offers %d", name, m.Requires[name], offered[name]),
				map[string]any{"id": m.ID, "capability": name, "version": m.Requires[name], "offered": offered[name], "min_core_hint": m.MinCoreHint})
		}
	}
	return nil
}

// checkHostSetupPlatform refuses host setup steps on a platform that
// cannot apply them: only Linux has udev and Landlock device rules.
func checkHostSetupPlatform(m *extapi.Manifest, p Platform) error {
	if len(m.HostSetup) == 0 || p.OS == "linux" {
		return nil
	}
	return refuse(messages.ErrorPluginHostSetupUnsupported,
		fmt.Sprintf("%s declares host setup steps, which %s cannot apply", m.ID, p.OS), map[string]any{"id": m.ID})
}

func checkFloor(m *extapi.Manifest, floor string) error {
	if floor == "" {
		return nil
	}
	c, err := extapi.CompareVersions(m.Version, floor)
	if err != nil || c < 0 {
		return refuse(messages.ErrorPluginBelowFloor, fmt.Sprintf("%s %s is below the floor %s", m.ID, m.Version, floor),
			map[string]any{"id": m.ID, "version": m.Version, "floor": floor})
	}
	return nil
}

// FeatureOwners names who already holds a feature key on this server. The
// plugin host implements it from the modules compiled in and the packages
// installed now.
type FeatureOwners interface {
	// FeatureOwner is who, other than package id, provides key: a compiled
	// module's name or another installed package's id; "" when no one does.
	FeatureOwner(id, key string) string
}

// checkFeatureOwners refuses a package that declares a feature key someone
// else provides (ErrorPluginFeatureTaken), so installing a package never
// moves a key from its owner. A nil owners checks nothing.
func checkFeatureOwners(m *extapi.Manifest, owners FeatureOwners) error {
	if owners == nil {
		return nil
	}
	for _, f := range m.Features {
		if owner := owners.FeatureOwner(m.ID, f.Key); owner != "" {
			return refuse(messages.ErrorPluginFeatureTaken, fmt.Sprintf("feature %s of %s is already provided by %s", f.Key, m.ID, owner),
				map[string]any{"id": m.ID, "feature": f.Key, "owner": owner})
		}
	}
	return nil
}
