package expose

import (
	"os"
	"path/filepath"
	"strings"
)

// connectorEnv builds the environment for the connector process.
//
// Two things are deliberately taken away from it. HOME and XDG_CONFIG_HOME
// point at a per-run directory, because a `~/.cloudflared/config.yaml` left by
// an earlier manual run changes what a quick tunnel does — the plan calls this
// out as a documented Cloudflare behaviour, and silently using someone else's
// config is not something an operator could debug. TUNNEL_* and CF_* are
// dropped for the same reason: they would redirect the connector at a tunnel
// the saved configuration never named.
//
// Everything else — including the server's proxy settings — is inherited: they
// describe how this host reaches the internet, which is exactly the question
// the connector faces.
func connectorEnv(isolationDir string, extra ...string) []string {
	out := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		switch {
		case key == "HOME", key == "XDG_CONFIG_HOME":
			continue
		case strings.HasPrefix(key, "TUNNEL_"), strings.HasPrefix(key, "CF_"):
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "HOME="+isolationDir, "XDG_CONFIG_HOME="+filepath.Join(isolationDir, "config"))
	out = append(out, extra...)
	return out
}

// isolationDir is the per-entry-point HOME the connector runs with. It is kept
// under the state directory so an operator can see that it is ours, and it is
// recreated on every start so nothing carries over between attempts.
func (s *Store) isolationDir() string {
	return filepath.Join(s.root, "run", "home")
}

// prepareIsolation clears and recreates the isolation directory.
func (s *Store) prepareIsolation() (string, error) {
	dir := s.isolationDir()
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o700); err != nil {
		return "", err
	}
	return dir, nil
}
