// Package trust is what the plugin host trusts when it verifies a package:
// the project roots compiled into the server, plus the ones root-signed trust
// updates added and minus the ones they retired; the certificate issuers the
// host operator trusts in server.yaml, with the grants it allows them; the
// revocations of trust updates and revocation lists, each under the anchor
// its list applies to; and the lowest version each package may be installed
// at. It also imports trust updates and revocation lists offline. The rules
// are docs/reference/plugin-protocol.md ("信任更新与吊销列表") and
// docs/adr/0071-installable-extensions.md (decision 5).
package trust

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// RootAnchor is the anchor of the project roots. A host-trusted issuer is
// its own anchor, HostAnchor(keyid).
const RootAnchor = "gwroot"

const hostAnchorPrefix = "host:"

// HostAnchor is the anchor of an issuer the host operator trusts.
func HostAnchor(keyID string) string { return hostAnchorPrefix + keyID }

// HostIssuer is a certificate issuer the host operator trusts in
// server.yaml (plugins.publishers). By default its packages get no
// privileged role, no privileged table, no authority and must run under
// Landlock; the operator allows more explicitly. A host issuer can never
// certify the project publisher, the bare feature namespace or same-origin
// console modules (pkg/extapi refuses those without a root's signature).
type HostIssuer struct {
	Key extapi.PublicKey
	// Roles and Tables are the privileged roles and tables its packages may
	// claim, intersected with what each certificate grants.
	Roles  []string
	Tables []string
	// Authority lets its certificates name an authority package for the
	// publisher's own namespace.
	Authority bool
	// Unsandboxed lets a certificate's min_isolation "none" stand; without
	// it, its packages need Landlock whatever the certificate says.
	Unsandboxed bool
}

// Set is the trust one verification runs against. It is immutable.
type Set struct {
	roots       extapi.KeySet
	issuers     extapi.KeySet
	hosts       map[string]HostIssuer
	revocations map[string][]extapi.Revocation
	floors      map[string]string
}

// Persisted is the trust stored in the database: roots trust updates added
// and retired, revocations by anchor, and floors by package id.
type Persisted struct {
	AddedRoots   []extapi.PublicKey
	RetiredRoots []string
	Revocations  map[string][]extapi.Revocation
	Floors       map[string]string
}

// New builds the trust from the compiled-in roots, the host issuers and
// what is persisted. It refuses two keys under one key id, a host issuer
// under a root key id, and a malformed key.
func New(compiled []extapi.PublicKey, hosts []HostIssuer, p Persisted) (*Set, error) {
	byID := map[string]extapi.PublicKey{}
	for _, k := range append(append([]extapi.PublicKey(nil), compiled...), p.AddedRoots...) {
		if !strings.HasPrefix(k.KeyID, extapi.RootKeyIDPrefix) {
			return nil, fmt.Errorf("root key id %q does not start with %s", k.KeyID, extapi.RootKeyIDPrefix)
		}
		if prev, ok := byID[k.KeyID]; ok && !sameKey(prev, k) {
			return nil, fmt.Errorf("root key id %s names two keys", k.KeyID)
		}
		byID[k.KeyID] = k
	}
	for _, id := range p.RetiredRoots {
		delete(byID, id)
	}
	rootList := make([]extapi.PublicKey, 0, len(byID))
	for _, k := range byID {
		rootList = append(rootList, k)
	}
	roots, err := extapi.NewKeySet(rootList...)
	if err != nil {
		return nil, err
	}
	s := &Set{roots: roots, hosts: map[string]HostIssuer{}, revocations: p.Revocations, floors: p.Floors}
	all := append([]extapi.PublicKey(nil), rootList...)
	for _, h := range hosts {
		if strings.HasPrefix(h.Key.KeyID, extapi.RootKeyIDPrefix) {
			return nil, fmt.Errorf("host-trusted issuer key id %q uses the root prefix %s", h.Key.KeyID, extapi.RootKeyIDPrefix)
		}
		if _, dup := s.hosts[h.Key.KeyID]; dup {
			return nil, fmt.Errorf("host-trusted issuer key id %s is listed twice", h.Key.KeyID)
		}
		s.hosts[h.Key.KeyID] = h
		all = append(all, h.Key)
	}
	if s.issuers, err = extapi.NewKeySet(all...); err != nil {
		return nil, err
	}
	if s.revocations == nil {
		s.revocations = map[string][]extapi.Revocation{}
	}
	if s.floors == nil {
		s.floors = map[string]string{}
	}
	return s, nil
}

func sameKey(a, b extapi.PublicKey) bool { return a.Alg == b.Alg && string(a.Key) == string(b.Key) }

// Store is the persisted trust; *store.Store satisfies it.
type Store interface {
	PluginRoots() (added []extapi.PublicKey, retired []string, err error)
	PluginRevocations() (map[string][]extapi.Revocation, error)
	PluginFloors() (map[string]string, error)
	ApplyPluginTrust(issuer, anchor string, u extapi.TrustUpdate) (applied bool, highest uint64, err error)
}

// Load reads the persisted trust and builds the Set.
func Load(db Store, compiled []extapi.PublicKey, hosts []HostIssuer) (*Set, error) {
	added, retired, err := db.PluginRoots()
	if err != nil {
		return nil, fmt.Errorf("read plugin trust roots: %w", err)
	}
	revs, err := db.PluginRevocations()
	if err != nil {
		return nil, fmt.Errorf("read plugin revocations: %w", err)
	}
	floors, err := db.PluginFloors()
	if err != nil {
		return nil, fmt.Errorf("read plugin floors: %w", err)
	}
	return New(compiled, hosts, Persisted{AddedRoots: added, RetiredRoots: retired, Revocations: revs, Floors: floors})
}

// Roots are the project roots trusted now.
func (s *Set) Roots() extapi.KeySet { return s.roots }

// Issuers are the keys a publisher certificate may be signed by: the roots
// and the host-trusted issuers.
func (s *Set) Issuers() extapi.KeySet { return s.issuers }

// Anchor is the anchor of a certificate whose signatures by issuerKeyIDs
// verified: the project roots when any of them is a root, otherwise the
// first host-trusted issuer.
func (s *Set) Anchor(issuerKeyIDs []string) string {
	ids := slices.Sorted(slices.Values(issuerKeyIDs))
	for _, id := range ids {
		if strings.HasPrefix(id, extapi.RootKeyIDPrefix) {
			return RootAnchor
		}
	}
	if len(ids) == 0 {
		return ""
	}
	return HostAnchor(ids[0])
}

// Grants are what a certificate under anchor may grant its packages: the
// certificate's own grants for the project roots; for a host-trusted
// issuer, only the roles, tables, authority and unsandboxed running the
// operator allowed that issuer.
func (s *Set) Grants(c *extapi.PublisherCert, anchor string) extapi.CertGrants {
	g := c.Grants
	g.IDPrefixes = slices.Clone(g.IDPrefixes)
	g.FeatureNS = slices.Clone(g.FeatureNS)
	if anchor == RootAnchor {
		g.Roles, g.Tables = slices.Clone(g.Roles), slices.Clone(g.Tables)
		return g
	}
	h := s.hosts[strings.TrimPrefix(anchor, hostAnchorPrefix)]
	g.Roles = intersect(g.Roles, h.Roles)
	g.Tables = intersect(g.Tables, h.Tables)
	if !h.Authority {
		g.AuthorityID = ""
	}
	if g.MinIsolation == extapi.IsolationNone && !h.Unsandboxed {
		g.MinIsolation = extapi.IsolationLandlock
	}
	g.UIModule = false
	return g
}

func intersect(a, allowed []string) []string {
	var out []string
	for _, v := range a {
		if slices.Contains(allowed, v) {
			out = append(out, v)
		}
	}
	return out
}

// CertRevocation returns the revocation that covers certificate c under
// anchor, or nil: an entry of the same anchor naming c's publisher alone,
// its serial or its key id.
func (s *Set) CertRevocation(anchor string, c *extapi.PublisherCert) *extapi.Revocation {
	for _, r := range s.revocations[anchor] {
		if r.Publisher != c.Publisher || r.Manifest != "" {
			continue
		}
		if r.CertSerial == 0 && r.KeyID == "" || r.CertSerial == c.Serial || r.KeyID == c.KeyID {
			return &r
		}
	}
	return nil
}

// ManifestRevocation returns the revocation of a package build by its
// manifest digest, under any anchor, or nil. It applies only to a manifest
// naming the revocation's publisher.
func (s *Set) ManifestRevocation(publisher string, d extapi.Digest) *extapi.Revocation {
	anchors := make([]string, 0, len(s.revocations))
	for a := range s.revocations {
		anchors = append(anchors, a)
	}
	sort.Strings(anchors)
	for _, a := range anchors {
		for _, r := range s.revocations[a] {
			if r.Manifest == d && r.Publisher == publisher {
				return &r
			}
		}
	}
	return nil
}

// Floor is the lowest version package id may be installed at, or "".
func (s *Set) Floor(id string) string { return s.floors[id] }

// known reports whether keyID is a root trusted now or a host-trusted
// issuer, and with which key.
func (s *Set) known(keyID string) (extapi.PublicKey, bool) {
	k, ok := s.issuers[keyID]
	return k, ok
}
