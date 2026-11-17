package cli

import (
	"log/slog"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// openMaintenanceStore opens the database for migrate, reset-password and
// extra commands: the core migrations, then the build's extension
// migrations, exactly as the server runs them when it starts.
func (o Options) openMaintenanceStore(dbPath string, policy securefile.Policy) (*store.Store, error) {
	return store.OpenWith(dbPath, bus.New(), store.OpenOptions{
		Policies:   []securefile.Policy{policy},
		Extensions: app.MigrationSets(o.Extensions),
	})
}

// logNamespaceVersions reports, after migrate, the schema version of each of
// the build's extension migration sets beside the core version.
func logNamespaceVersions(log *slog.Logger, st *store.Store, ext app.Extensions) error {
	for _, set := range app.MigrationSets(ext) {
		v, err := st.NamespaceVersion(set.Namespace)
		if err != nil {
			return err
		}
		log.Info("extension schema migrated", "namespace", set.Namespace, "version", v)
	}
	return nil
}
