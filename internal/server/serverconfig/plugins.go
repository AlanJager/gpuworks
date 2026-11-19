package serverconfig

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Plugins is the plugin host's part of server.yaml, owned by the host's root
// like the rest of the file (ADR 0071, decision 20): the certificate issuers
// the operator trusts besides the project roots, and the site policy, which
// can only narrow what those allow.
type Plugins struct {
	// Publishers are certificate issuers the operator trusts. A package they
	// certify gets no privileged role, table or authority and must run under
	// Landlock unless its entry says otherwise.
	Publishers []PluginPublisher `yaml:"publishers,omitempty"`
	// Policy narrows which packages install.
	Policy PluginPolicy `yaml:"policy,omitempty"`
	// AllowUnsandboxed are third-party package ids allowed to run unconfined
	// on a host without Landlock although their certificate asks for it.
	AllowUnsandboxed []string `yaml:"allow_unsandboxed,omitempty"`
	// RequireIsolation "landlock" refuses any plugin process that would run
	// unconfined; empty or "none" leaves the default.
	RequireIsolation string `yaml:"require_isolation,omitempty"`
}

// PluginPublisher is one certificate issuer the operator trusts.
type PluginPublisher struct {
	// KeyID, Alg and PublicKey (standard base64) are the issuer's key. The
	// key id may not start with the project roots' prefix "gwroot-".
	KeyID     string `yaml:"keyid"`
	Alg       string `yaml:"alg"`
	PublicKey string `yaml:"public_key"`
	// Roles and Tables are the privileged roles and tables the issuer's
	// packages may claim, when their certificates grant them too.
	Roles  []string `yaml:"roles,omitempty"`
	Tables []string `yaml:"tables,omitempty"`
	// Authority lets the issuer's certificates name an authority package
	// for the publisher's own namespace (never the bare one).
	Authority bool `yaml:"authority,omitempty"`
	// Unsandboxed lets its certificates' min_isolation "none" stand.
	Unsandboxed bool `yaml:"unsandboxed,omitempty"`
}

// PluginPolicy narrows which packages install, and says whether an
// upgrade asking for nothing new needs approving again.
type PluginPolicy struct {
	// AllowPublishers, when not empty, are the only publishers whose
	// packages install.
	AllowPublishers []string `yaml:"allow_publishers,omitempty"`
	// DenyPackages, DenyPermissions and DenyRoles refuse any package with
	// that id, permission or claimed role.
	DenyPackages    []string `yaml:"deny_packages,omitempty"`
	DenyPermissions []string `yaml:"deny_permissions,omitempty"`
	DenyRoles       []string `yaml:"deny_roles,omitempty"`
	// AutoApproveUnchanged lets an upgrade whose approval hash equals the
	// approved one go ahead without a new approval. It lets in nothing that
	// verification refuses. Default false: every upgrade is approved.
	AutoApproveUnchanged bool `yaml:"auto_approve_unchanged,omitempty"`
	// MaxDownGrace, a Go duration ("30m", "2h"), is the site's ceiling on
	// how long a plugin's grants keep their access after it went down
	// (ADR 0071 decision 9): a plugin's own grace above it is lowered to it.
	// Empty or "0" leaves the plugins' own; otherwise it is 60 s to 24 h.
	MaxDownGrace string `yaml:"max_down_grace,omitempty"`
}

// Bounds of plugins.policy.max_down_grace: those of the grace a plugin may
// push (pkg/extapi MinDownGraceS and MaxDownGraceS).
const (
	minDownGraceCeiling = extapi.MinDownGraceS * time.Second
	maxDownGraceCeiling = extapi.MaxDownGraceS * time.Second
)

// DownGraceCeiling is plugins.policy.max_down_grace, zero when it is not
// set; Validate refuses a value outside its bounds.
func (p PluginPolicy) DownGraceCeiling() (time.Duration, error) {
	if p.MaxDownGrace == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(p.MaxDownGrace)
	if err != nil || d != 0 && (d < minDownGraceCeiling || d > maxDownGraceCeiling) {
		return 0, fmt.Errorf("plugins.policy.max_down_grace %q is neither 0 nor a duration from %v to %v", p.MaxDownGrace, minDownGraceCeiling, maxDownGraceCeiling)
	}
	return d, nil
}

// Key returns the issuer's key in wire form.
func (p PluginPublisher) Key() (extapi.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(p.PublicKey)
	if err != nil {
		return extapi.PublicKey{}, fmt.Errorf("plugins.publishers %s: public_key is not base64", p.KeyID)
	}
	k := extapi.PublicKey{KeyID: p.KeyID, Alg: p.Alg, Key: raw}
	if _, err := extapi.NewKeySet(k); err != nil {
		return extapi.PublicKey{}, fmt.Errorf("plugins.publishers %s: %w", p.KeyID, err)
	}
	if strings.HasPrefix(p.KeyID, extapi.RootKeyIDPrefix) {
		return extapi.PublicKey{}, fmt.Errorf("plugins.publishers %s: key ids starting with %s belong to the project roots", p.KeyID, extapi.RootKeyIDPrefix)
	}
	return k, nil
}

// Validate checks the section without trusting anything.
func (p Plugins) Validate() error {
	seen := map[string]bool{}
	for _, pub := range p.Publishers {
		if _, err := pub.Key(); err != nil {
			return err
		}
		if seen[pub.KeyID] {
			return fmt.Errorf("plugins.publishers %s is listed twice", pub.KeyID)
		}
		seen[pub.KeyID] = true
	}
	switch p.RequireIsolation {
	case "", extapi.IsolationNone, extapi.IsolationLandlock:
	default:
		return fmt.Errorf("plugins.require_isolation %q is neither %s nor %s", p.RequireIsolation, extapi.IsolationLandlock, extapi.IsolationNone)
	}
	for _, id := range append(append([]string(nil), p.AllowUnsandboxed...), p.Policy.DenyPackages...) {
		if !extapi.ValidPackageID(id) {
			return fmt.Errorf("plugins: %q is not a package id", id)
		}
	}
	if _, err := p.Policy.DownGraceCeiling(); err != nil {
		return err
	}
	for _, pub := range p.Policy.AllowPublishers {
		if !extapi.ValidPublisher(pub) {
			return fmt.Errorf("plugins.policy.allow_publishers: %q is not a publisher name", pub)
		}
	}
	return nil
}
