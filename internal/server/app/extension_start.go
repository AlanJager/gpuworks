package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/holdpass"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
)

// extensionSet is what the modules registered through their Hosts, and the
// services the server runs for them.
type extensionSet struct {
	apis     []api.Extension
	banners  []api.BannerSource
	features []featuregate.Feature
	// featureBy names the module that declared each static feature key.
	featureBy map[featuregate.Key]string
	// dynamic holds the feature keys installed plugin packages declare;
	// the plugins module makes it once every compiled module has declared.
	dynamic  *featuregate.DynamicSet
	source   featuregate.Source
	sourceBy string
	holders  int
	holds    *holdpass.Runner
	services []func(context.Context)
	closed   bool
	// plugins is the plugin administration of the built-in plugins module.
	plugins api.PluginAdmin
}

// holdPassFloor is the least time between two passes of the scheduling
// holds a module asks for (Host.ReapplySchedulingHolds): each is a writer
// transaction over the fleet, so a holder whose answers flap costs one pass
// per floor.
const holdPassFloor = 2 * time.Second

// startExtensions runs every module's Init, applies the scheduling holds
// they registered (and releases holds no module of this build owns), runs
// the passes they ask for afterwards with the modules' services, and hands
// the API what they added. With no modules it leaves the API exactly
// as the community server has it, apart from releasing holds a former build
// left behind. On error it stops what New has started that Run would
// otherwise stop; New closes the store.
func startExtensions(cfg Config, a *App, master string, deps *api.Deps) (err error) {
	defer func() {
		if err != nil {
			a.proxy.Close()
		}
	}()
	set := &extensionSet{holds: holdpass.New(a.store.ApplySchedulingHolds, a.log.With("component", "scheduling_holds"), holdPassFloor)}
	section := serverconfig.File{Extensions: cfg.Sections}
	plugins := &pluginsModule{cfg: cfg, st: a.store}
	for _, m := range append(slices.Clip(cfg.Extensions.Modules), Module(plugins)) {
		name := m.Name()
		if svc, ok := m.(Service); ok {
			set.services = append(set.services, moduleService(a, name, svc))
		}
		starter, ok := m.(Initializer)
		if !ok {
			continue
		}
		h := &Host{name: name, log: a.log.With("module", name), bus: a.bus, dataDir: cfg.DataDir,
			install: a.settings.installID, master: master, section: section, st: a.store, reg: set}
		if h.tables, err = a.store.ExtensionTables(name); err != nil && !errors.Is(err, store.ErrUnknownNamespace) {
			return err
		}
		if err = starter.Init(context.Background(), h); err != nil {
			return fmt.Errorf("extension %s: init: %w", name, err)
		}
	}
	set.closed = true
	set.plugins = pluginAdmin{plugins.bridge}
	if released, err := a.store.ReleaseUnownedHolds(); err != nil {
		return fmt.Errorf("release scheduling holds no extension owns: %w", err)
	} else if len(released) > 0 {
		a.log.Info("released nodes held by an extension this server does not run", "nodes", len(released))
	}
	if set.holders > 0 {
		held, released, err := a.store.ApplySchedulingHolds()
		if err != nil {
			return fmt.Errorf("apply extension scheduling holds: %w", err)
		}
		a.log.Info("applied extension scheduling holds", "held", len(held), "released", len(released))
		set.services = append(set.services, set.holds.Run)
	}
	if deps.Extensions, err = set.apiDeps(); err != nil {
		return err
	}
	a.ext = set
	return nil
}

// apiDeps is what the API gets from the modules: their routes, banners and
// the feature gate over the features the modules declared and those the
// installed plugin packages declare now, and the build's edition for
// the console manifest. Without modules it changes no route.
func (set *extensionSet) apiDeps() (api.ExtensionDeps, error) {
	gate, err := featuregate.NewWithDynamic(set.features, set.source, set.dynamic)
	if err != nil {
		return api.ExtensionDeps{}, err
	}
	return api.ExtensionDeps{Extensions: set.apis, Features: gate, Banners: set.banners, Edition: version.Edition, Plugins: set.plugins}, nil
}

// moduleService runs a module's Service on Run's context and logs an error it
// stops with while the server is still running.
func moduleService(a *App, name string, svc Service) func(context.Context) {
	return func(ctx context.Context) {
		if err := svc.Run(ctx); err != nil && ctx.Err() == nil {
			a.log.Error("extension service stopped", "module", name, "err", err)
		}
	}
}
