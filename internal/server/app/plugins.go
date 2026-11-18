package app

import (
	"context"
	"errors"
	"io"
	"runtime"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/extpkg/trust"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginbridge"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// PluginRuntime adjusts how the plugin host runs, for tests; the zero
// value runs plugins through this server's own binary, on the system
// clock, with the default supervision timing.
type PluginRuntime struct {
	// Trampoline is the binary started as `<Trampoline> plugin-exec`.
	Trampoline string
	// Clock is what pushed state's timelines and grace follow.
	Clock pushstate.Clock
	// Timing is the supervisor's intervals.
	Timing pluginhost.Timing
	// TickInterval is the least time between two writer ticks of pushed
	// state (one second when zero).
	TickInterval time.Duration
}

// pluginsModuleName is the built-in module's name; no compiled module may
// take it.
const pluginsModuleName = "plugins"

// pluginsModule is the built-in module that runs installed plugins
// (package pluginbridge, docs/dev/extensions.md): its Init loads pushed
// state before the router is built and registers its evaluators as the
// feature source (when installed packages declare features), the node
// insert guard, the scheduling holder and a banner source; its service
// starts the plugins.
type pluginsModule struct {
	cfg    Config
	st     *store.Store
	bridge *pluginbridge.Bridge
}

func (m *pluginsModule) Name() string { return pluginsModuleName }

func (m *pluginsModule) Init(ctx context.Context, h *Host) error {
	roots := m.cfg.PluginRoots
	if roots == nil {
		roots = trust.ProjectRoots()
	}
	pkgs, err := extpkg.NewHost(extpkg.Setup{DataDir: m.cfg.DataDir, Roots: roots, Config: m.cfg.Plugins, Store: m.st})
	if err != nil {
		return err
	}
	rt := m.cfg.PluginRuntime
	bus := h.Bus()
	b, err := pluginbridge.Open(ctx, pluginbridge.Config{
		Packages: pkgs,
		State: pushstate.Config{Store: m.st, Fleet: PushedStateFleet(m.st), Bus: bus, FleetTopic: store.NodeFleetTopic,
			CoreVersion: version.Version, Clock: rt.Clock, TickInterval: rt.TickInterval,
			Effects: pushstate.Effects{
				ReapplyHolds:    h.ReapplySchedulingHolds,
				FeaturesChanged: func() { bus.PublishChange(pluginbridge.FeatureTopic, "", "changed") },
			}},
		Host:             pluginhost.Config{Trampoline: rt.Trampoline, Timing: rt.Timing},
		Core:             extapi.CoreInfo{Version: version.Version, InstallID: h.InstallID(), OS: runtime.GOOS, Arch: runtime.GOARCH, HARole: extapi.HARoleSingle},
		RequireIsolation: m.cfg.Plugins.RequireIsolation,
		Log:              h.Log(),
	})
	if err != nil {
		return err
	}
	hooks := NewPushedStateHooks(b.State())
	if features := b.Features(); len(features) > 0 {
		h.DeclareFeatures(features...)
		if err := h.ProvideFeatures(hooks.Features()); err != nil {
			return err
		}
	}
	h.GuardNodeInsert(hooks.GuardNode)
	if err := h.HoldScheduling(hooks.Holder()); err != nil {
		return err
	}
	h.AddBannerSource(hooks.Banners())
	m.bridge = b
	return nil
}

func (m *pluginsModule) Run(ctx context.Context) error { return m.bridge.Run(ctx) }

// pluginAdmin adapts the bridge to the API's plugin administration.
type pluginAdmin struct{ b *pluginbridge.Bridge }

var _ api.PluginAdmin = pluginAdmin{}

func (p pluginAdmin) List(context.Context) []dto.Plugin {
	views := p.b.List()
	out := make([]dto.Plugin, len(views))
	for i, v := range views {
		out[i] = pluginDTO(v)
	}
	return out
}

func (p pluginAdmin) Install(ctx context.Context, body io.Reader, approve string, enable bool, by string) (dto.PluginInstalled, error) {
	in, err := p.b.Install(ctx, body, pluginbridge.InstallRequest{Approve: extapi.Digest(approve), Enable: enable, By: by})
	if err != nil {
		return dto.PluginInstalled{}, productErrorOf(err)
	}
	return dto.PluginInstalled{Plugin: pluginDTO(in.View), Previous: in.Previous, Unchanged: in.Unchanged}, nil
}

func (p pluginAdmin) Enable(ctx context.Context, id, by string) (dto.Plugin, error) {
	v, err := p.b.Enable(ctx, id, by)
	return pluginDTO(v), productErrorOf(err)
}

func (p pluginAdmin) Disable(ctx context.Context, id, by string) (dto.Plugin, error) {
	v, err := p.b.Disable(ctx, id, by)
	return pluginDTO(v), productErrorOf(err)
}

func (p pluginAdmin) Uninstall(ctx context.Context, id string, purge bool, by string) error {
	return productErrorOf(p.b.Uninstall(ctx, id, purge, by))
}

// productErrorOf hands a wire refusal from pkg/extapi on as the product
// error it is, so the API keeps its code and params.
func productErrorOf(err error) error {
	var xe *extapi.Error
	var pe *messages.ProductError
	if err == nil || errors.As(err, &pe) || !errors.As(err, &xe) {
		return err
	}
	return messages.WrapProductError(xe.Code, xe.Message, xe.Params, err)
}

// pluginPermissions gives each permission the server's risk class, so the
// console shows the class without judging it.
func pluginPermissions(names []string) []dto.PluginPermission {
	out := make([]dto.PluginPermission, len(names))
	for i, name := range names {
		out[i] = dto.PluginPermission{Name: name, Risk: extpkg.PermissionRisk(name)}
	}
	return out
}

func pluginDTO(v pluginbridge.View) dto.Plugin {
	out := dto.Plugin{ID: v.ID, Version: v.Version, Publisher: v.Publisher, Trust: v.Trust, Enabled: v.Enabled, State: v.State,
		Restarts: v.Restarts, Parts: nonNil(v.Parts), Isolation: v.Isolation, Roles: nonNil(v.Roles), Permissions: pluginPermissions(v.Permissions),
		ApproveHash: v.ApproveHash, Actions: nonNil(v.Actions)}
	if v.Code != "" {
		out.Message = &dto.ProductMessage{Code: v.Code, Params: v.Params, Message: v.Message}
	}
	if !v.Since.IsZero() {
		since := v.Since.UTC()
		out.Since = &since
	}
	return out
}
