// Package extsign makes signed plugin test material: project roots
// ("gwroot-test-*"), host-trusted issuers, publisher certificates, trust
// updates, revocation lists and signed .gwpkg packages, for the tests of
// every part of the plugin host. Keys are generated per test run; no private
// key is ever checked in, and nothing here is a real root.
//
// The package layout and the verification order it exercises are described
// in docs/reference/plugin-protocol.md and docs/adr/0071-installable-extensions.md.
package extsign

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Epoch is the reference time of the material: certificates are valid for
// signing from Epoch for five years, and manifests are created at Epoch plus
// a day unless a test says otherwise.
var Epoch = time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)

// Key is one Ed25519 signing key under a key id.
type Key struct {
	ID     string
	Public extapi.PublicKey
	Signer extapi.KeySigner
}

// NewKey generates an Ed25519 key under id.
func NewKey(t testing.TB, id string) *Key {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := extapi.PublicKeyOf(id, pub)
	if err != nil {
		t.Fatal(err)
	}
	return &Key{ID: id, Public: pk, Signer: extapi.KeySigner{KeyID: id, Signer: priv}}
}

// NewRoot generates a project root; name is appended to "gwroot-test-".
func NewRoot(t testing.TB, name string) *Key {
	t.Helper()
	return NewKey(t, extapi.RootKeyIDPrefix+"test-"+name)
}

// Seal signs v (marshalled to canonical JSON) with every key.
func Seal(t testing.TB, v any, keys ...*Key) *extapi.Envelope {
	t.Helper()
	payload, err := extapi.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	signers := make([]extapi.KeySigner, len(keys))
	for i, k := range keys {
		signers[i] = k.Signer
	}
	env, err := extapi.Seal(payload, signers...)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// Encode returns the canonical bytes of v.
func Encode(t testing.TB, v any) []byte {
	t.Helper()
	raw, err := extapi.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Publisher is a publisher's signing key and its certificates.
type Publisher struct {
	Name  string
	Key   *Key
	Cert  extapi.PublisherCert
	Certs []*extapi.Envelope
}

// CertFor returns a format-1 certificate template for publisher name over
// key: serial 1, signing window [Epoch, Epoch+5y), every id starting with
// "<name>_" (or, for the project publisher, ids without "_"), the
// publisher's own feature namespace, Landlock required.
func CertFor(name string, key *Key) extapi.PublisherCert {
	grants := extapi.CertGrants{IDPrefixes: []string{name + "_"}, FeatureNS: []string{name}, MinIsolation: extapi.IsolationLandlock}
	if name == extapi.ProjectPublisher {
		grants.IDPrefixes = []string{"gpuworks_"}
		grants.FeatureNS = []string{"", name}
	}
	return extapi.PublisherCert{
		Format: extapi.CertFormat, Serial: 1, Publisher: name,
		KeyID: key.ID, Alg: key.Public.Alg, PublicKey: key.Public.Key,
		NotBefore: extapi.NewTime(Epoch), NotAfter: extapi.NewTime(Epoch.AddDate(5, 0, 0)),
		Grants: grants,
	}
}

// NewPublisher generates a key for publisher name ("<name>-test1") and
// certifies it with CertFor, edited by edit when it is not nil, signed by
// issuers.
func NewPublisher(t testing.TB, name string, edit func(*extapi.PublisherCert), issuers ...*Key) *Publisher {
	t.Helper()
	key := NewKey(t, name+"-test1")
	cert := CertFor(name, key)
	if edit != nil {
		edit(&cert)
	}
	return &Publisher{Name: name, Key: key, Cert: cert, Certs: []*extapi.Envelope{Seal(t, cert, issuers...)}}
}

// CertBundle is the bytes of publisher.crt.
func (p *Publisher) CertBundle(t testing.TB) []byte {
	t.Helper()
	list := make([]extapi.Envelope, len(p.Certs))
	for i, e := range p.Certs {
		list[i] = *e
	}
	return Encode(t, list)
}

// File is one member of a package: its bytes and whether it is executable.
type File struct {
	Body []byte
	Exec bool
}

// Spec describes a package to build. Manifest's Files is filled from Files;
// every other manifest field is the test's.
type Spec struct {
	Manifest extapi.Manifest
	Files    map[string]File
}

// Minimal returns a spec for a package id of publisher pub at version with
// one server executable for linux/amd64 and darwin/arm64, a resident
// runtime and nothing else.
func Minimal(pub, id, version string) Spec {
	return Spec{
		Manifest: extapi.Manifest{
			Schema: extapi.ManifestSchema, ID: id, Publisher: pub, Version: version,
			Created:  extapi.NewTime(Epoch.Add(24 * time.Hour)),
			Protocol: extapi.Range{Min: 1, Max: 1},
			Runtime:  &extapi.RuntimeDecl{Lifecycle: extapi.LifecycleResident},
		},
		Files: map[string]File{
			"bin/linux_amd64/" + id:  {Body: []byte("#!fake " + id + " " + version + " linux"), Exec: true},
			"bin/linux_arm64/" + id:  {Body: []byte("#!fake " + id + " " + version + " linux arm"), Exec: true},
			"bin/darwin_arm64/" + id: {Body: []byte("#!fake " + id + " " + version + " darwin"), Exec: true},
			"locales/en.json":        {Body: []byte(`{}`)},
		},
	}
}

// Built is a package's parts before they are archived, so a test can
// tamper with any of them.
type Built struct {
	Manifest     []byte
	ManifestSig  []byte
	PublisherCrt []byte
	Files        map[string]File
	// Extra are raw tar entries appended after the members (links, special
	// files, duplicate or unsafe names).
	Extra []Entry
}

// Entry is a raw tar entry.
type Entry struct {
	Header tar.Header
	Body   []byte
}

// Build signs spec with pub's key and returns its parts.
func Build(t testing.TB, pub *Publisher, spec Spec) *Built {
	t.Helper()
	m := spec.Manifest
	m.Files = ListFiles(spec.Files)
	raw := Encode(t, m)
	return &Built{
		Manifest:     raw,
		ManifestSig:  Encode(t, Seal(t, m, pub.Key)),
		PublisherCrt: pub.CertBundle(t),
		Files:        spec.Files,
	}
}

// ListFiles returns the manifest file list of files, sorted by path.
func ListFiles(files map[string]File) []extapi.FileEntry {
	out := make([]extapi.FileEntry, 0, len(files))
	for path, f := range files {
		sum := sha256.Sum256(f.Body)
		out = append(out, extapi.FileEntry{Path: path, Size: int64(len(f.Body)), SHA256: hex.EncodeToString(sum[:]), Exec: f.Exec})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Archive returns the .gwpkg bytes: a gzip-compressed tar holding the
// signature members first, then the files sorted by path, then Extra.
func (b *Built) Archive(t testing.TB) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write := func(h tar.Header, body []byte) {
		if h.Typeflag == tar.TypeReg || h.Typeflag == 0 {
			h.Size = int64(len(body))
		}
		if h.ModTime.IsZero() && h.Typeflag != tar.TypeXGlobalHeader {
			h.ModTime = Epoch
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if len(body) > 0 {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, m := range []struct {
		name string
		body []byte
	}{{"manifest.json", b.Manifest}, {"manifest.sig", b.ManifestSig}, {"publisher.crt", b.PublisherCrt}} {
		if m.body != nil {
			write(tar.Header{Name: m.name, Mode: 0o644, Typeflag: tar.TypeReg}, m.body)
		}
	}
	paths := make([]string, 0, len(b.Files))
	for p := range b.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		mode := int64(0o644)
		if b.Files[p].Exec {
			mode = 0o755
		}
		write(tar.Header{Name: p, Mode: mode, Typeflag: tar.TypeReg}, b.Files[p].Body)
	}
	for _, e := range b.Extra {
		write(e.Header, e.Body)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Package builds and archives spec signed by pub.
func Package(t testing.TB, pub *Publisher, spec Spec) []byte {
	t.Helper()
	return Build(t, pub, spec).Archive(t)
}

// TrustUpdate seals a trust update with serial and the given changes,
// created at Epoch plus serial days, signed by roots.
func TrustUpdate(t testing.TB, serial uint64, edit func(*extapi.TrustUpdate), roots ...*Key) []byte {
	t.Helper()
	u := extapi.TrustUpdate{Format: extapi.TrustUpdateFormat, Serial: serial, Created: extapi.NewTime(Epoch.AddDate(0, 0, int(serial)))}
	if edit != nil {
		edit(&u)
	}
	return Encode(t, Seal(t, u, roots...))
}

// CRL seals a revocation list by publisher (empty for a root list) with
// serial and revoked, signed by keys.
func CRL(t testing.TB, serial uint64, publisher string, revoked []extapi.Revocation, keys ...*Key) []byte {
	t.Helper()
	c := extapi.CRL{Format: extapi.CRLFormat, Serial: serial, Created: extapi.NewTime(Epoch.AddDate(0, 0, int(serial))), Publisher: publisher, Revoked: revoked}
	return Encode(t, Seal(t, c, keys...))
}

// ManifestDigest is the digest a revocation by manifest names.
func (b *Built) ManifestDigest() extapi.Digest { return extapi.DigestOf(b.Manifest) }
