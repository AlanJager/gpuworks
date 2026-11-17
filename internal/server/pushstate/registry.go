// Package pushstate is the core's pushed state: the tables plugins push
// through the host API (docs/reference/plugin-protocol.md, "推送与确认"),
// validated, persisted and enforced by the core (ADR 0071 decisions 8 to
// 10).
//
// A push is checked against the pusher's identity (its package, its
// certificate's grants, its approved permissions), queued, and committed by
// the writer tick together with every other pending change in one
// transaction, at most once a second. Only after the commit does the core
// swap one snapshot pointer, so the hooks it feeds (the feature gate's
// source, the node insert guard, the scheduling holder and the console
// banners) each answer with one atomic load and map lookups, never waiting
// on a plugin, a lock or the database.
//
// What was pushed is read back at boot before any plugin runs, so it is
// enforced across a core restart with every plugin dead. Timelines follow
// extclock, a clock that never runs backwards. A plugin that is down keeps
// its restrictions in force indefinitely; its grants narrow after its grace.
// The package knows nothing of processes, packages on disk or HTTP: its
// callers tell it who is installed, who is pushing and who is up.
package pushstate

import (
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Package is one installed package as pushed state needs to know it.
type Package struct {
	// ID is the package id; Publisher its publisher.
	ID        string
	Publisher string
	// Grants are the grants of the certificate the package verified under,
	// with the authority the host operator configured for a host-trusted
	// publisher already applied.
	Grants extapi.CertGrants
	// Features are the feature keys the package declares.
	Features []extapi.FeatureDecl
	// Verified is false for a package that is installed but no longer
	// verifies (a broken or missing file, revoked by digest): its
	// restrictions and banners stay enforced and only its grants are
	// dropped, so damaging a package never loosens what it restricted.
	Verified bool
}

// Pusher is the verified identity of the process a push came from, bound to
// its connection by the supervisor; nothing in a push names it.
type Pusher struct {
	ID        string
	Publisher string
	// Grants are the grants of the package's certificate (see Package).
	Grants extapi.CertGrants
	// Permissions are the permissions the administrator approved
	// ("push:node_holds", ...).
	Permissions []string
	// Codes are the product codes the package's manifest lists; every
	// notice it pushes must use one of them or a core feature code.
	Codes []string
	// Generation is the spawn generation of the process: a push from a
	// process the supervisor has since replaced is refused as stale.
	Generation uint64
}

// coreNoticeCodes are core codes a pushed notice may use without listing
// them: the feature gate's own refusals, whose texts the console has.
var coreNoticeCodes = []string{
	"FEATURE_UNAVAILABLE", "FEATURE_READ_ONLY", "FEATURE_PROVIDER_ABSENT", "FEATURE_LIMIT_REACHED",
}

func (p *Pusher) mayUseCode(code string) bool {
	return slices.Contains(p.Codes, code) || slices.Contains(coreNoticeCodes, code)
}

func (p *Pusher) hasPermission(perm string) bool { return slices.Contains(p.Permissions, perm) }

// decl is who declared a feature key and who answers for it.
type decl struct {
	pkg          string
	gate         string
	presentation string
	// authority is the package id the declaring publisher's certificate
	// names for gate:authority keys; "" when it names none.
	authority string
	bare      bool
}

// registry is the installed packages and the feature keys they declare.
type registry struct {
	pkgs  map[string]Package
	decls map[featuregate.Key]decl
}

// newRegistry indexes pkgs. A declaration outside its publisher's feature
// namespaces, or of a key another package (by lower id) already declared,
// is left out: the verifier refuses such packages, and this keeps a bad one
// from answering for another's key.
func newRegistry(pkgs []Package) *registry {
	r := &registry{pkgs: make(map[string]Package, len(pkgs)), decls: map[featuregate.Key]decl{}}
	sorted := slices.Clone(pkgs)
	slices.SortFunc(sorted, func(a, b Package) int { return strings.Compare(a.ID, b.ID) })
	for _, p := range sorted {
		if _, dup := r.pkgs[p.ID]; dup {
			continue
		}
		r.pkgs[p.ID] = p
		for _, f := range p.Features {
			k := featuregate.Key(f.Key)
			ns, ok := namespaceOf(p.Publisher, f.Key)
			if !ok || !slices.Contains(p.Grants.FeatureNS, ns) {
				continue
			}
			if _, dup := r.decls[k]; dup {
				continue
			}
			r.decls[k] = decl{pkg: p.ID, gate: f.Gate, presentation: f.Presentation, authority: p.Grants.AuthorityID, bare: ns == ""}
		}
	}
	return r
}

// namespaceOf is the feature namespace a key declared by publisher lies in:
// the project publisher's keys are bare (""), a third party's start with
// its name and a dot.
func namespaceOf(publisher, key string) (string, bool) {
	if publisher == extapi.ProjectPublisher {
		return "", true
	}
	if strings.HasPrefix(key, publisher+".") {
		return publisher, true
	}
	return "", false
}

// answers reports whether package id answers for key k: as the authority
// the declaring publisher's certificate names for a gate:authority key, or
// as the declarer of a gate:self key. A bare key is answered only by a
// package of the project publisher.
func (r *registry) answers(id, publisher string, k featuregate.Key) bool {
	d, ok := r.decls[k]
	if !ok {
		return false
	}
	if d.bare && publisher != extapi.ProjectPublisher {
		return false
	}
	switch d.gate {
	case extapi.GateAuthority:
		return d.authority != "" && d.authority == id
	case extapi.GateSelf:
		return d.pkg == id
	}
	return false
}

// installed reports whether id is installed, and whether it verifies.
func (r *registry) installed(id string) (installed, verified bool) {
	p, ok := r.pkgs[id]
	return ok, ok && p.Verified
}

// Features are the feature keys the installed packages declare and pushed
// state answers for, sorted by key, as the feature gate declares them: a
// key outside its publisher's granted namespaces, invalid, or declared
// again by another package is left out, exactly as the answers are.
func (s *State) Features() []featuregate.Feature {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]featuregate.Feature, 0, len(s.reg.decls))
	for k, d := range s.reg.decls {
		if !k.Valid() {
			continue
		}
		f := featuregate.Feature{Key: k, Presentation: featuregate.Promote}
		if d.presentation == extapi.PresentationConceal {
			f.Presentation = featuregate.Conceal
		}
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b featuregate.Feature) int { return strings.Compare(string(a.Key), string(b.Key)) })
	return out
}
