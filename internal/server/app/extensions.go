package app

import (
	"context"
	"fmt"
	"io/fs"
	"regexp"

	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/webui"
)

// Extensions are what a separately built server adds to this one: compiled
// modules and, optionally, its own console build. The zero value is the
// community server, unchanged. The contract is described in
// docs/dev/extensions.md.
type Extensions struct {
	// Modules are started in this order: their migrations run when the
	// store opens, their Init after the API dependencies are built and
	// before the router, their services with the server's own loops.
	Modules []Module
	// UI is the console build the server serves, laid out like
	// internal/server/webui/dist (index.html at its root); nil serves the
	// embedded community console.
	UI fs.FS
}

// Module is one compiled extension. Its name is its stable id: the
// server.yaml section extensions.<name>, the namespace of its migrations and
// tables, and the label on its log records.
type Module interface {
	// Name follows [a-z][a-z0-9_]{1,31}.
	Name() string
}

// Migrator is optional on a Module: the schema of its own tables. The set's
// Namespace must be the module's name; the store runs it right after the
// core migrations, in New and in the maintenance commands (migrate,
// reset-password, support-bundle).
type Migrator interface {
	Migrations() store.MigrationSet
}

// Initializer is optional on a Module: Init registers what the module adds
// through the Host. It runs once per New, in module order, before the API
// router is built; the Host refuses registrations after it returns. An error
// stops New, which closes the store again.
type Initializer interface {
	Init(ctx context.Context, h *Host) error
}

// Service is optional on a Module: a background loop the server runs with
// its own background loops, after the controller has started. Run returns
// when ctx is done; an error it returns before that is logged and the loop is
// not restarted. A server may be Run more than once (a restart in place), so
// Run must tolerate being called again after it returned.
type Service interface {
	Run(ctx context.Context) error
}

var moduleNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)

// validate refuses a module list the server cannot compose: a nil module, a
// name outside the grammar or used twice, or a migration set under another
// namespace. It runs before the store opens.
func (e Extensions) validate() error {
	seen := map[string]bool{}
	for i, m := range e.Modules {
		if m == nil {
			return fmt.Errorf("extension module %d is nil", i+1)
		}
		name := m.Name()
		switch {
		case !moduleNamePattern.MatchString(name):
			return fmt.Errorf("extension module name %q does not match [a-z][a-z0-9_]{1,31}", name)
		case seen[name]:
			return fmt.Errorf("extension module %q is registered twice", name)
		case name == pluginsModuleName:
			return fmt.Errorf("extension module name %q is the built-in plugin host's", name)
		}
		seen[name] = true
		if mig, ok := m.(Migrator); ok {
			if ns := mig.Migrations().Namespace; ns != name {
				return fmt.Errorf("extension module %q: its migrations use namespace %q; a module's namespace is its name", name, ns)
			}
		}
	}
	return nil
}

// MigrationSets are the migration sets of the modules that have one, in
// module order: what store.OpenWith takes for this build.
func MigrationSets(ext Extensions) []store.MigrationSet {
	var out []store.MigrationSet
	for _, m := range ext.Modules {
		if mig, ok := m.(Migrator); ok {
			out = append(out, mig.Migrations())
		}
	}
	return out
}

// ModuleNames are the modules' names, in order: the server.yaml extensions
// sections this build reads.
func ModuleNames(ext Extensions) []string {
	out := make([]string, 0, len(ext.Modules))
	for _, m := range ext.Modules {
		if m != nil {
			out = append(out, m.Name())
		}
	}
	return out
}

// ui is the console build to serve.
func (e Extensions) ui() fs.FS {
	if e.UI != nil {
		return e.UI
	}
	return webui.FS()
}
