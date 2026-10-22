package expose

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// The connector forwards to the server's own listening address. The connector
// service is not the server and cannot see what it bound, so the server
// records it on every start, and the CLI reads it when it commits a request.

// RecordServerOrigin is called by the server once its listeners exist: it
// writes the address it really bound, a wildcard turned into loopback.
func RecordServerOrigin(dataDir, listenAddr string) error {
	origin, err := OriginFromListenAddr(listenAddr)
	if err != nil {
		return err
	}
	store := NewStore(dataDir)
	if err := store.Ensure(); err != nil {
		return err
	}
	return writeFileAtomic(store.ServerOriginPath(), []byte(origin+"\n"), 0o664)
}

// ResolveOrigin decides what the connector forwards to: an explicit --origin,
// then what the server recorded, then the first console address in the
// server.yaml of the installation that owns this data dir. It says which rule
// matched.
func ResolveOrigin(store *Store, explicit string) (string, string, error) {
	if explicit != "" {
		if !strings.HasPrefix(explicit, "http://") && !strings.HasPrefix(explicit, "https://") {
			return "", "", fmt.Errorf("--origin %q must be an http:// or https:// URL", explicit)
		}
		return strings.TrimRight(explicit, "/"), "--origin", nil
	}
	if raw, err := os.ReadFile(store.ServerOriginPath()); err == nil {
		if origin := strings.TrimSpace(string(raw)); origin != "" {
			return origin, "recorded by the server", nil
		}
	}
	for _, path := range serverYAMLCandidates() {
		f, err := serverconfig.Load(path)
		if err != nil || len(f.Listen.Console) == 0 || filepath.Clean(f.DataDir) != filepath.Clean(filepath.Dir(store.Root())) {
			continue
		}
		if origin, err := OriginFromListenAddr(f.Listen.Console[0]); err == nil {
			return origin, path, nil
		}
	}
	return "", "", errors.New("cannot tell which address the server listens on: start the server once, or pass --origin http://HOST:PORT")
}

// serverYAMLCandidates are the server.yaml files ResolveDataDir looks at.
func serverYAMLCandidates() []string {
	var paths []string
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, serverconfig.Path(filepath.Dir(exe)))
	}
	return append(paths, serverconfig.Path(selfinstall.DefaultDir))
}
