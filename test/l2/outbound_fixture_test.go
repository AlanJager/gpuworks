//go:build multiproc

package l2

import (
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// loopbackOutboundConfig explicitly trusts only the local HTTP source and
// collector fixtures used by these process tests. Other process tests keep
// the server's default outbound policy.
func loopbackOutboundConfig(t *testing.T) string {
	t.Helper()
	return serverFixtureConfig(t, serverconfig.File{OutboundAllowPrivateHosts: []string{"127.0.0.1"}})
}

func serverFixtureConfig(t *testing.T, cfg serverconfig.File) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), serverconfig.FileName)
	if err := serverconfig.Write(path, cfg, 0o600); err != nil {
		t.Fatalf("write server fixture configuration: %v", err)
	}
	return path
}
