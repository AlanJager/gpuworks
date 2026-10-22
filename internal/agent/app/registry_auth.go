package app

import (
	"fmt"
	"log/slog"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/imagepull"
	"gpuworks.ai/gpuworks/internal/agent/registryprobe"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// registryLogins is the node's logins: registries and model sites. The type
// exists to keep the secrets out of logs: whatever prints one (a struct dump,
// %v, slog) gets the hosts and users, never a secret.
type registryLogins []proto.SourceAuth

func (l registryLogins) String() string {
	parts := make([]string, len(l))
	for i, a := range l {
		parts[i] = a.Username + "@" + a.Host
	}
	return "[" + strings.Join(parts, " ") + "] (secrets redacted)"
}

func (l registryLogins) GoString() string { return "registryLogins" + l.String() }

func (l registryLogins) LogValue() slog.Value { return slog.StringValue(l.String()) }

func (l registryLogins) Format(f fmt.State, _ rune) { _, _ = fmt.Fprint(f, l.String()) }

// setRegistryAuth replaces the registry logins. Every state sync carries the
// whole set, so a login the server dropped stops being used at once. They live
// in memory only.
func (n *NodeRuntime) setRegistryAuth(auth []proto.SourceAuth) {
	n.authMu.Lock()
	n.registryAuth = append(registryLogins(nil), auth...)
	n.authMu.Unlock()
}

// currentRegistryAuth is a copy for one pull or download to carry.
func (n *NodeRuntime) currentRegistryAuth() []proto.SourceAuth {
	n.authMu.RLock()
	defer n.authMu.RUnlock()
	return append([]proto.SourceAuth(nil), n.registryAuth...)
}

// newPuller builds the node's one image puller. registries is the prober its
// pulls share; nil probes the real registries, except for an injected runtime
// (a test), which probes none (registryprobe.Offline). The same prober is
// the memory of failed addresses model downloads share (n.registries).
func (n *NodeRuntime) newPuller(log *slog.Logger, injected bool, registries *registryprobe.Prober) *imagepull.Puller {
	switch {
	case registries == nil && injected:
		registries = registryprobe.Offline()
	case registries == nil:
		registries = registryprobe.New()
	}
	n.registries = registries
	return imagepull.New(imagepull.Config{
		Prober:      registries,
		Log:         log,
		Network:     n.network,
		Auth:        n.currentRegistryAuth,
		DaemonProxy: n.daemonProxy,
	})
}

// modelLogins are the node's logins as model requests apply them
// (modelrepo.AuthorizationFor).
func (n *NodeRuntime) modelLogins() []modelrepo.Login {
	auths := n.currentRegistryAuth()
	out := make([]modelrepo.Login, 0, len(auths))
	for _, auth := range auths {
		out = append(out, modelrepo.Login{Host: auth.Host, Username: auth.Username, Secret: auth.Secret})
	}
	return out
}
