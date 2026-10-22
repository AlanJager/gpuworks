package app

import (
	"errors"
	"log/slog"
	"os"

	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// ensurePlatformSSHKey generates the platform SSH key on first start. A
// failure is logged, not fatal: the server still runs, and SSH logins fall
// back to whatever password or key the operator supplies.
func ensurePlatformSSHKey(st *store.Store, log *slog.Logger) {
	if _, _, err := st.PlatformSSHKey(); err == nil {
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		log.Error("read platform ssh key", "err", err)
		return
	}
	hostname, _ := os.Hostname()
	key, err := provision.GeneratePlatformKey(provision.PlatformKeyComment(hostname))
	if err != nil {
		log.Error("generate platform ssh key", "err", err)
		return
	}
	if _, err := st.SetPlatformSSHKey(key.PrivatePEM, key.PublicKey, key.Fingerprint); err != nil {
		log.Error("save platform ssh key", "err", err)
		return
	}
	log.Info("generated platform ssh key", "fingerprint", key.Fingerprint)
}
