package cli

import (
	"log/slog"

	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// claimedSections are the server.yaml extensions sections a module of this
// build reads: one per module, under its name.
func (o Options) claimedSections() []string { return app.ModuleNames(o.Extensions) }

// warnUnclaimedSections logs each extensions section of server.yaml that no
// module of this build reads. It is a warning, not a refusal: a data
// directory moved from another build brings its sections along, and the
// server must still start.
func warnUnclaimedSections(log *slog.Logger, file *serverconfig.File, known []string) {
	if file == nil {
		return
	}
	for _, name := range serverconfig.UnknownSections(*file, known) {
		log.Warn("server.yaml has an extensions section no module of this build reads; it is ignored", "section", name)
	}
}
