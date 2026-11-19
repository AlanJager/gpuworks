package extpkg

import (
	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/extpkg/trust"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// HostConfig turns server.yaml's plugins section into the issuers trust
// adds and the site policy. The section was validated when it was loaded;
// a key that still does not decode is an error here too.
func HostConfig(p serverconfig.Plugins) ([]trust.HostIssuer, Policy, error) {
	issuers := make([]trust.HostIssuer, 0, len(p.Publishers))
	for _, pub := range p.Publishers {
		key, err := pub.Key()
		if err != nil {
			return nil, Policy{}, err
		}
		issuers = append(issuers, trust.HostIssuer{Key: key, Roles: pub.Roles, Tables: pub.Tables, Authority: pub.Authority, Unsandboxed: pub.Unsandboxed})
	}
	policy := Policy{
		AllowPublishers: p.Policy.AllowPublishers, DenyPackages: p.Policy.DenyPackages,
		DenyPermissions: p.Policy.DenyPermissions, DenyRoles: p.Policy.DenyRoles,
		RequireIsolation: p.RequireIsolation, AllowUnsandboxed: p.AllowUnsandboxed,
		AutoApproveUnchanged: p.Policy.AutoApproveUnchanged,
	}
	return issuers, policy, nil
}

// Store is what the package host keeps in the server's database: trust,
// floors and version markers. *store.Store satisfies it.
type Store interface {
	trust.Store
	Markers
}

// Setup is everything NewHost needs.
type Setup struct {
	// DataDir is the server's data directory.
	DataDir string
	// Roots are the project roots of the build (trust.ProjectRoots, or a
	// fork's own).
	Roots []extapi.PublicKey
	// Config is server.yaml's plugins section.
	Config serverconfig.Plugins
	Store  Store
}

// NewHost loads the trust and returns a Host verifying against this
// server's capabilities, platform and isolation.
func NewHost(s Setup) (*Host, error) {
	issuers, policy, err := HostConfig(s.Config)
	if err != nil {
		return nil, err
	}
	set, err := trust.Load(s.Store, s.Roots, issuers)
	if err != nil {
		return nil, err
	}
	return &Host{
		Dir:      layout.New(s.DataDir),
		Verifier: &Verifier{Trust: set, Offered: Capabilities(), Policy: policy, Platform: HostPlatform(), Isolation: HostIsolation()},
		Markers:  s.Store,
		Hasher:   layout.NewHasher(),
	}, nil
}
