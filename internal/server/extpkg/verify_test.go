package extpkg_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/extpkg/archive"
	"gpuworks.ai/gpuworks/internal/server/extpkg/trust"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func codeOf(err error) (string, map[string]any) {
	var pe *messages.ProductError
	if errors.As(err, &pe) {
		return pe.Code(), pe.Params()
	}
	var xe *extapi.Error
	if errors.As(err, &xe) {
		return xe.Code, xe.Params
	}
	return "", nil
}

// world is one server's trust with a root, a host issuer and a publisher.
type world struct {
	root   *extsign.Key
	issuer *extsign.Key
	pub    *extsign.Publisher
	set    *trust.Set
	host   trust.HostIssuer
	p      trust.Persisted
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{root: extsign.NewRoot(t, "1"), issuer: extsign.NewKey(t, "site-issuer")}
	w.host = trust.HostIssuer{Key: w.issuer.Public}
	w.pub = extsign.NewPublisher(t, "acme", nil, w.root)
	w.rebuild(t)
	return w
}

func (w *world) rebuild(t *testing.T) {
	t.Helper()
	set, err := trust.New([]extapi.PublicKey{w.root.Public}, []trust.HostIssuer{w.host}, w.p)
	if err != nil {
		t.Fatal(err)
	}
	w.set = set
}

func (w *world) verifier() *extpkg.Verifier {
	return &extpkg.Verifier{Trust: w.set, Offered: map[string]int{"routes": 1, "table.availability": 1},
		Platform: extpkg.Platform{OS: "linux", Arch: "amd64"}, Isolation: extapi.IsolationLandlock}
}

func contents(t *testing.T, b *extsign.Built) *archive.Contents {
	t.Helper()
	c, err := archive.Read(bytes.NewReader(b.Archive(t)), archive.Discard{}, archive.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestVerifyAcceptsAWellSignedPackage(t *testing.T) {
	w := newWorld(t)
	spec := extsign.Minimal("acme", "acme_backup", "1.0.0")
	spec.Manifest.Features = []extapi.FeatureDecl{{Key: "acme.backup", Presentation: extapi.PresentationPromote, Gate: extapi.GateNone}}
	spec.Manifest.Requires = map[string]int{"table.availability": 1}
	b := extsign.Build(t, w.pub, spec)
	v, err := w.verifier().Verify(contents(t, b), extpkg.Install)
	if err != nil {
		t.Fatal(err)
	}
	if v.Manifest.ID != "acme_backup" || v.Trust != extpkg.TrustProject || v.Anchor != trust.RootAnchor ||
		v.Binary != "bin/linux_amd64/acme_backup" || v.Digest != b.ManifestDigest() || v.Revoked != nil || v.Authority() {
		t.Fatalf("verified = %+v", v)
	}
	h1, err := extpkg.ApprovalHash(v)
	if err != nil || !h1.Valid() {
		t.Fatalf("approval hash = %q %v", h1, err)
	}
	spec.Manifest.Permissions = []string{"read:fleet"}
	spec.Manifest.Uses = map[string]int{"host.fleet": 1}
	v2, err := w.verifier().Verify(contents(t, extsign.Build(t, w.pub, spec)), extpkg.Install)
	if err != nil {
		t.Fatal(err)
	}
	if h2, _ := extpkg.ApprovalHash(v2); h2 == h1 {
		t.Fatal("a new permission did not change the approval hash")
	}
}

func TestVerifyRefusesInOrder(t *testing.T) {
	type tc struct {
		name   string
		build  func(t *testing.T, w *world) *extsign.Built
		edit   func(w *world, v *extpkg.Verifier)
		code   string
		params map[string]any
	}
	minimal := func(t *testing.T, w *world) *extsign.Built {
		return extsign.Build(t, w.pub, extsign.Minimal("acme", "acme_backup", "1.0.0"))
	}
	withSpec := func(edit func(*extsign.Spec)) func(t *testing.T, w *world) *extsign.Built {
		return func(t *testing.T, w *world) *extsign.Built {
			s := extsign.Minimal("acme", "acme_backup", "1.0.0")
			edit(&s)
			return extsign.Build(t, w.pub, s)
		}
	}
	cases := []tc{
		{name: "1 certificate by an unknown issuer", build: func(t *testing.T, w *world) *extsign.Built {
			stranger := extsign.NewRoot(t, "9")
			return extsign.Build(t, extsign.NewPublisher(t, "acme", nil, stranger), extsign.Minimal("acme", "acme_backup", "1.0.0"))
		}, code: extapi.ErrorSignatureUntrusted},
		{name: "1 garbage certificates", build: func(t *testing.T, w *world) *extsign.Built {
			b := minimal(t, w)
			b.PublisherCrt = []byte("[]")
			return b
		}, code: extapi.ErrorWireInvalid},
		{name: "1 host issuer certifying the project publisher", build: func(t *testing.T, w *world) *extsign.Built {
			p := extsign.NewPublisher(t, "gpuworks", nil, w.issuer)
			return extsign.Build(t, p, extsign.Minimal("gpuworks", "gpuworks_sso", "1.0.0"))
		}, code: extapi.ErrorSignatureUntrusted},
		{name: "2 revoked certificate", build: minimal, edit: func(w *world, v *extpkg.Verifier) {
			w.p.Revocations = map[string][]extapi.Revocation{trust.RootAnchor: {{Publisher: "acme", CertSerial: 1}}}
		}, code: messages.ErrorPluginCertRevoked, params: map[string]any{"publisher": "acme", "serial": uint64(1)}},
		{name: "2 revoked key", build: minimal, edit: func(w *world, v *extpkg.Verifier) {
			w.p.Revocations = map[string][]extapi.Revocation{trust.RootAnchor: {{Publisher: "acme", KeyID: "acme-test1"}}}
		}, code: messages.ErrorPluginCertRevoked},
		{name: "3 manifest signed by another key", build: func(t *testing.T, w *world) *extsign.Built {
			b := minimal(t, w)
			uncertified := extsign.NewKey(t, "acme-uncertified")
			b.ManifestSig = extsign.Encode(t, extsign.Seal(t, mustDecode(t, b.Manifest), uncertified))
			return b
		}, code: extapi.ErrorSignatureUntrusted},
		{name: "3 tampered signature", build: func(t *testing.T, w *world) *extsign.Built {
			b := minimal(t, w)
			env, _ := extapi.DecodeEnvelope(b.ManifestSig)
			env.Signatures[0].Sig[0] ^= 1
			b.ManifestSig = extsign.Encode(t, env)
			return b
		}, code: extapi.ErrorSignatureInvalid},
		{name: "4 manifest.json differs from the signed payload", build: func(t *testing.T, w *world) *extsign.Built {
			b := minimal(t, w)
			other := extsign.Build(t, w.pub, extsign.Minimal("acme", "acme_backup", "1.0.1"))
			b.Manifest = other.Manifest
			return b
		}, code: messages.ErrorPluginManifestMismatch},
		{name: "5 manifest schema too new", build: func(t *testing.T, w *world) *extsign.Built {
			payload := []byte(`{"min_core_hint":"v28.1","schema":2}`)
			env, err := extapi.Seal(payload, w.pub.Key.Signer)
			if err != nil {
				t.Fatal(err)
			}
			b := minimal(t, w)
			b.Manifest, b.ManifestSig = payload, extsign.Encode(t, env)
			return b
		}, code: extapi.ErrorManifestTooNew, params: map[string]any{"min_core_hint": "v28.1"}},
		{name: "6 publisher differs from the certificate", build: func(t *testing.T, w *world) *extsign.Built {
			s := extsign.Minimal("beta", "beta_backup", "1.0.0")
			return extsign.Build(t, w.pub, s)
		}, code: messages.ErrorPluginPublisherMismatch, params: map[string]any{"publisher": "beta", "cert_publisher": "acme"}},
		{name: "6 id not covered", build: func(t *testing.T, w *world) *extsign.Built {
			p := extsign.NewPublisher(t, "acme", func(c *extapi.PublisherCert) { c.Grants.IDPrefixes = []string{"acme_tools"} }, w.root)
			return extsign.Build(t, p, extsign.Minimal("acme", "acme_backup", "1.0.0"))
		}, code: messages.ErrorPluginIDNotGranted},
		{name: "7 created before the signing window", build: withSpec(func(s *extsign.Spec) {
			s.Manifest.Created = extapi.NewTime(extsign.Epoch.Add(-time.Hour))
		}), code: messages.ErrorPluginCreatedOutsideWindow},
		{name: "7 created after the signing window", build: withSpec(func(s *extsign.Spec) {
			s.Manifest.Created = extapi.NewTime(extsign.Epoch.AddDate(5, 0, 1))
		}), code: messages.ErrorPluginCreatedOutsideWindow},
		{name: "8 feature outside the namespace", build: withSpec(func(s *extsign.Spec) {
			s.Manifest.Features = []extapi.FeatureDecl{{Key: "quota", Presentation: extapi.PresentationPromote, Gate: extapi.GateAuthority}}
		}), code: messages.ErrorPluginGrantMissing, params: map[string]any{"grant": "feature_ns", "value": "quota"}},
		{name: "8 role not granted", build: withSpec(func(s *extsign.Spec) { s.Manifest.Roles = []string{"key_wrap"} }),
			code: messages.ErrorPluginGrantMissing, params: map[string]any{"grant": "role", "value": "key_wrap"}},
		{name: "8 authority role on a package that is not the authority", build: func(t *testing.T, w *world) *extsign.Built {
			p := extsign.NewPublisher(t, "acme", func(c *extapi.PublisherCert) {
				c.Grants.Roles = []string{extpkg.AuthorityRole}
				c.Grants.AuthorityID = "acme_lic"
			}, w.root)
			s := extsign.Minimal("acme", "acme_backup", "1.0.0")
			s.Manifest.Roles = []string{extpkg.AuthorityRole}
			return extsign.Build(t, p, s)
		}, code: messages.ErrorPluginGrantMissing, params: map[string]any{"grant": "authority", "value": "acme_backup"}},
		{name: "8 privileged table not granted", build: withSpec(func(s *extsign.Spec) {
			s.Manifest.Permissions = []string{"push:fleet_capacity"}
			s.Manifest.Requires = map[string]int{"table.fleet_capacity": 1}
		}),
			code: messages.ErrorPluginGrantMissing, params: map[string]any{"grant": "table", "value": "fleet_capacity"}},
		{name: "8 same-origin console module without the grant", build: withSpec(func(s *extsign.Spec) {
			s.Files["ui/index.js"] = extsign.File{Body: []byte("export {}")}
			s.Manifest.UI = &extapi.UIDecl{Mode: extapi.UIModeModule, Entry: "ui/index.js", SDK: extapi.Range{Min: 1, Max: 1}}
			s.Manifest.Requires = map[string]int{"ui.bundle": 1}
		}), code: messages.ErrorPluginGrantMissing, params: map[string]any{"grant": "ui_module"}},
		{name: "9 unlisted member", build: func(t *testing.T, w *world) *extsign.Built {
			b := minimal(t, w)
			b.Files["bin/linux_amd64/extra"] = extsign.File{Body: []byte("x")}
			return b
		}, code: messages.ErrorPluginFileMismatch, params: map[string]any{"member": "bin/linux_amd64/extra", "reason": extpkg.FileUnlisted}},
		{name: "9 missing member", build: func(t *testing.T, w *world) *extsign.Built {
			b := minimal(t, w)
			delete(b.Files, "locales/en.json")
			return b
		}, code: messages.ErrorPluginFileMismatch, params: map[string]any{"member": "locales/en.json", "reason": extpkg.FileMissing}},
		{name: "9 changed member", build: func(t *testing.T, w *world) *extsign.Built {
			b := minimal(t, w)
			b.Files["locales/en.json"] = extsign.File{Body: []byte(`{"x"}`)}
			return b
		}, code: messages.ErrorPluginFileMismatch, params: map[string]any{"reason": extpkg.FileSize}},
		{name: "9 same size, other bytes", build: func(t *testing.T, w *world) *extsign.Built {
			b := minimal(t, w)
			b.Files["locales/en.json"] = extsign.File{Body: []byte(`[]`)}
			return b
		}, code: messages.ErrorPluginFileMismatch, params: map[string]any{"reason": extpkg.FileSHA256}},
		{name: "9 no executable for this platform", build: withSpec(func(s *extsign.Spec) { delete(s.Files, "bin/linux_amd64/acme_backup") }),
			code: messages.ErrorPluginBinaryMissing, params: map[string]any{"os": "linux", "arch": "amd64", "count": 0}},
		{name: "9 two executables for this platform", build: withSpec(func(s *extsign.Spec) {
			s.Files["bin/linux_amd64/helper"] = extsign.File{Body: []byte("x"), Exec: true}
		}), code: messages.ErrorPluginBinaryMissing, params: map[string]any{"count": 2}},
		{name: "10 required capability missing", build: withSpec(func(s *extsign.Spec) {
			s.Manifest.Requires = map[string]int{"table.availability": 2}
			s.Manifest.MinCoreHint = "v27.3"
		}), code: messages.ErrorPluginIncompatible, params: map[string]any{"capability": "table.availability", "version": 2, "offered": 1, "min_core_hint": "v27.3"}},
		{name: "10 protocol not spoken", build: withSpec(func(s *extsign.Spec) { s.Manifest.Protocol = extapi.Range{Min: 2, Max: 3} }),
			code: extapi.ErrorProtocolUnsupported},
		{name: "11 publisher not allowed", build: minimal, edit: func(w *world, v *extpkg.Verifier) { v.Policy.AllowPublishers = []string{"beta"} },
			code: messages.ErrorPluginPolicyRefused, params: map[string]any{"rule": extpkg.PolicyPublisher, "value": "acme"}},
		{name: "11 package denied", build: minimal, edit: func(w *world, v *extpkg.Verifier) { v.Policy.DenyPackages = []string{"acme_backup"} },
			code: messages.ErrorPluginPolicyRefused, params: map[string]any{"rule": extpkg.PolicyPackage}},
		{name: "11 permission denied", build: withSpec(func(s *extsign.Spec) {
			s.Manifest.Permissions = []string{"read:directory"}
			s.Manifest.Uses = map[string]int{"host.directory": 1}
		}),
			edit: func(w *world, v *extpkg.Verifier) { v.Policy.DenyPermissions = []string{"read:directory"} },
			code: messages.ErrorPluginPolicyRefused, params: map[string]any{"rule": extpkg.PolicyPermission}},
		{name: "11 third party without Landlock", build: minimal, edit: func(w *world, v *extpkg.Verifier) { v.Isolation = extapi.IsolationNone },
			code: messages.ErrorPluginIsolationUnavailable},
		{name: "11 Landlock required by the site", build: minimal, edit: func(w *world, v *extpkg.Verifier) {
			v.Isolation, v.Policy.RequireIsolation, v.Policy.AllowUnsandboxed = extapi.IsolationNone, extapi.IsolationLandlock, []string{"acme_backup"}
		}, code: messages.ErrorPluginIsolationUnavailable},
		{name: "12 below the floor", build: minimal, edit: func(w *world, v *extpkg.Verifier) {
			w.p.Floors = map[string]string{"acme_backup": "1.0.1"}
		}, code: messages.ErrorPluginBelowFloor, params: map[string]any{"id": "acme_backup", "version": "1.0.0", "floor": "1.0.1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			b := c.build(t, w)
			v := w.verifier()
			if c.edit != nil {
				c.edit(w, v)
				w.rebuild(t)
				v.Trust = w.set
			}
			_, err := v.Verify(contents(t, b), extpkg.Install)
			code, params := codeOf(err)
			if code != c.code {
				t.Fatalf("err = %v (%s), want %s", err, code, c.code)
			}
			for k, want := range c.params {
				if params[k] != want {
					t.Fatalf("param %s = %#v, want %#v (all %v)", k, params[k], want, params)
				}
			}
		})
	}
}

func mustDecode(t *testing.T, raw []byte) *extapi.Manifest {
	t.Helper()
	m, err := extapi.DecodeManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestVerifyRecheckReportsRevocationsWithoutRefusing(t *testing.T) {
	w := newWorld(t)
	b := extsign.Build(t, w.pub, extsign.Minimal("acme", "acme_backup", "1.0.0"))
	w.p.Revocations = map[string][]extapi.Revocation{trust.HostAnchor("x"): {{Publisher: "acme", Manifest: b.ManifestDigest(), Replacement: "1.0.2"}}}
	w.p.Floors = map[string]string{"acme_backup": "2.0.0"}
	w.rebuild(t)
	if _, err := w.verifier().Verify(contents(t, b), extpkg.Install); func() string { c, _ := codeOf(err); return c }() != messages.ErrorPluginPackageRevoked {
		t.Fatalf("install of a revoked build = %v", err)
	}
	v, err := w.verifier().Verify(contents(t, b), extpkg.Recheck)
	if err != nil {
		t.Fatalf("recheck refused an installed revoked build below the floor: %v", err)
	}
	if v.Revoked == nil || v.Revoked.Replacement != "1.0.2" {
		t.Fatalf("revocation not reported: %+v", v.Revoked)
	}

	// A revoked certificate is reported, never refused, on recheck.
	w.p.Revocations = map[string][]extapi.Revocation{trust.RootAnchor: {{Publisher: "acme"}}}
	w.rebuild(t)
	v, err = w.verifier().Verify(contents(t, b), extpkg.Recheck)
	if err != nil || v.Revoked == nil {
		t.Fatalf("recheck with a revoked certificate = %+v %v", v, err)
	}
}

func TestVerifyHostIssuedPackages(t *testing.T) {
	w := newWorld(t)
	w.host = trust.HostIssuer{Key: w.issuer.Public, Roles: []string{extpkg.AuthorityRole}, Authority: true}
	w.rebuild(t)
	pub := extsign.NewPublisher(t, "acme", func(c *extapi.PublisherCert) {
		c.Grants.Roles = []string{extpkg.AuthorityRole, "key_wrap"}
		c.Grants.AuthorityID = "acme_lic"
		c.Grants.Tables = []string{"fleet_capacity"}
	}, w.issuer)
	spec := extsign.Minimal("acme", "acme_lic", "1.0.0")
	spec.Manifest.Roles = []string{extpkg.AuthorityRole}
	spec.Manifest.Features = []extapi.FeatureDecl{{Key: "acme.seats", Presentation: extapi.PresentationPromote, Gate: extapi.GateAuthority}}
	v, err := w.verifier().Verify(contents(t, extsign.Build(t, pub, spec)), extpkg.Install)
	if err != nil {
		t.Fatal(err)
	}
	if v.Trust != extpkg.TrustHost || v.Anchor != trust.HostAnchor(w.issuer.ID) || !v.Authority() {
		t.Fatalf("host-issued authority = %+v", v)
	}
	// What the operator did not allow the issuer stays ungranted.
	spec.Manifest.Roles = []string{"key_wrap"}
	if _, err := w.verifier().Verify(contents(t, extsign.Build(t, pub, spec)), extpkg.Install); func() string { c, _ := codeOf(err); return c }() != messages.ErrorPluginGrantMissing {
		t.Fatalf("role the operator did not allow = %v", err)
	}
	spec.Manifest.Roles = nil
	spec.Manifest.Permissions = []string{"push:fleet_capacity"}
	spec.Manifest.Requires = map[string]int{"table.fleet_capacity": 1}
	if _, err := w.verifier().Verify(contents(t, extsign.Build(t, pub, spec)), extpkg.Install); func() string { c, _ := codeOf(err); return c }() != messages.ErrorPluginGrantMissing {
		t.Fatalf("table the operator did not allow = %v", err)
	}
}

func TestVerifyUsesAnyCertificateThatSigned(t *testing.T) {
	// During a key rotation the package is dual-signed and publisher.crt
	// holds both certificates; the old one's window has closed.
	w := newWorld(t)
	old := w.pub
	old.Cert.NotAfter = extapi.NewTime(extsign.Epoch.Add(time.Hour))
	old.Certs = []*extapi.Envelope{extsign.Seal(t, old.Cert, w.root)}
	fresh := extsign.NewKey(t, "acme-test2")
	cert := extsign.CertFor("acme", fresh)
	cert.Serial = 2
	spec := extsign.Minimal("acme", "acme_backup", "1.0.0")
	b := extsign.Build(t, old, spec)
	m := mustDecode(t, b.Manifest)
	b.ManifestSig = extsign.Encode(t, extsign.Seal(t, m, old.Key, fresh))
	both := []extapi.Envelope{*old.Certs[0], *extsign.Seal(t, cert, w.root)}
	b.PublisherCrt = extsign.Encode(t, both)
	v, err := w.verifier().Verify(contents(t, b), extpkg.Install)
	if err != nil {
		t.Fatal(err)
	}
	if v.Cert.KeyID != "acme-test2" {
		t.Fatalf("verified under %s", v.Cert.KeyID)
	}
}

func TestFeatureNamespace(t *testing.T) {
	for _, c := range []struct{ pub, key, ns string }{
		{"acme", "acme.backup", "acme"}, {"acme", "acme.backup.s3", "acme"}, {"acme", "backup", ""},
		{"acme", "acmebank.pay", ""}, {"gpuworks", "quota", ""}, {"gpuworks", "gpuworks.sso", "gpuworks"},
	} {
		if got := extpkg.FeatureNamespace(c.pub, c.key); got != c.ns {
			t.Fatalf("FeatureNamespace(%s, %s) = %q", c.pub, c.key, got)
		}
	}
}
