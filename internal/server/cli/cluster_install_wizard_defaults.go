package cli

import (
	"context"
	"os"
	"strings"
)

// Only suggest a private-key file that OpenSSH would consider for this host.
// Agent-only configurations stay automatic; discovery never reads key contents.
func clusterWizardDefaultKey(ctx context.Context, ssh clusterInstallSSH) string {
	args := append([]string{"-G"}, clusterInstallSSHArgs(clusterInstallNode{SSH: ssh})...)
	raw, err := clusterInstallSSHCommand(ctx, args).Output()
	if err != nil {
		return ""
	}
	var candidates []string
	for _, line := range strings.Split(string(raw), "\n") {
		field, value, ok := strings.Cut(line, " ")
		if !ok || field != "identityfile" {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "none" {
			return ""
		}
		candidates = append(candidates, value)
	}
	for _, candidate := range candidates {
		// Unsupported OpenSSH tokens and named-user home expansion remain with
		// OpenSSH. Never interpret them with a shell or guess a different path.
		if strings.ContainsAny(candidate, "%$") || (strings.HasPrefix(candidate, "~") && !strings.HasPrefix(candidate, "~/")) || strings.HasSuffix(candidate, ".pub") {
			continue
		}
		path, err := clusterWizardLocalPath(candidate)
		if err == nil && clusterWizardReadableKey(path) {
			return path
		}
	}
	return ""
}

func clusterWizardReadableKey(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	return file.Close() == nil
}
