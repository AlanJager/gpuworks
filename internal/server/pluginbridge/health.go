package pluginbridge

import (
	"io"
	"reflect"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// CheckID is the id of the plugins diagnostics check.
const CheckID = "plugins"

// Check is one result of a diagnostics check: a proto.Check* status and a
// code with its params.
type Check struct {
	ID, Status, Code string
	Params           map[string]any
	Message          string
}

// health is what an enabled package's view says about it: whether it
// cannot run (failing), needs a server it is not on (incompatible), and
// the banner it raises.
type health struct {
	failing, incompatible bool
	banner                *pushstate.Banner
}

// healthOf judges one view. Only enabled packages count: one an
// administrator disabled asks nothing of anyone.
func healthOf(v View) health {
	if !v.Enabled {
		return health{}
	}
	switch v.State {
	case string(pluginhost.StateCrashloop):
		return health{failing: true, banner: healthBanner("crashloop", v.ID, extapi.SeverityDanger,
			messages.NewProductError(messages.ErrorPluginCrashloop, "plugin "+v.ID+" keeps crashing", v.Params))}
	case StateRevoked:
		return health{failing: true, banner: healthBanner("revoked", v.ID, extapi.SeverityDanger,
			messages.NewProductError(messages.ErrorPluginRevoked, "plugin "+v.ID+" is revoked", map[string]any{"id": v.ID}))}
	case StateBroken:
		if v.Code == messages.ErrorPluginIncompatible {
			return health{incompatible: true, banner: healthBanner("incompatible", v.ID, extapi.SeverityWarning,
				messages.NewProductError(messages.ErrorPluginIncompatible, v.Message, v.Params))}
		}
		return health{failing: true}
	case string(pluginhost.StateFailed):
		return health{failing: true}
	}
	return health{}
}

func healthBanner(kind, id, severity string, notice *messages.ProductError) *pushstate.Banner {
	return &pushstate.Banner{ID: "plugins." + kind + "." + id, Severity: severity, Notice: notice}
}

// refreshHealth judges every installed package again and publishes the
// banners when they changed. It runs on every status change of a plugin
// and every change of the installed packages; health serializes it, so
// a later judgement is never replaced by an earlier one.
func (b *Bridge) refreshHealth() {
	b.health.Lock()
	defer b.health.Unlock()
	list := bannersOf(b.List())
	if cur := b.banners.Load(); cur != nil && sameHealthBanners(*cur, list) {
		return
	}
	b.banners.Store(&list)
	if b.cfg.HealthChanged != nil {
		b.cfg.HealthChanged()
	}
}

// bannersOf is the health banners of views, danger first, then by id.
func bannersOf(views []View) []pushstate.Banner {
	var list []pushstate.Banner
	for _, v := range views {
		if h := healthOf(v); h.banner != nil {
			list = append(list, *h.banner)
		}
	}
	slices.SortStableFunc(list, func(x, y pushstate.Banner) int {
		if rank(x.Severity) != rank(y.Severity) {
			return rank(x.Severity) - rank(y.Severity)
		}
		return strings.Compare(x.ID, y.ID)
	})
	return list
}

func rank(severity string) int {
	if severity == extapi.SeverityDanger {
		return 0
	}
	return 1
}

func sameHealthBanners(a, b []pushstate.Banner) bool {
	return slices.EqualFunc(a, b, func(x, y pushstate.Banner) bool {
		return x.ID == y.ID && x.Severity == y.Severity && x.Notice.Code() == y.Notice.Code() &&
			reflect.DeepEqual(x.Notice.Params(), y.Notice.Params())
	})
}

// HealthBanners are the administrator banners of the installed plugins'
// health: a crashlooping (PLUGIN_CRASHLOOP), revoked (PLUGIN_REVOKED) or
// incompatible (PLUGIN_INCOMPATIBLE) enabled package, danger first, then by
// id. The list is held in memory, never persisted, and never modified
// after it is returned, so a caller may cache what it derives from it by
// its first element's address.
func (b *Bridge) HealthBanners() []pushstate.Banner {
	if p := b.banners.Load(); p != nil {
		return *p
	}
	return nil
}

// Check is the plugins diagnostics check: fail while any enabled package
// is crashlooping, failed, broken or revoked (params: their ids), warn
// while any is incompatible with this server (params: their ids), pass
// otherwise.
func (b *Bridge) Check() Check { return checkOf(b.List()) }

func checkOf(views []View) Check {
	var failing, incompatible []string
	for _, v := range views {
		h := healthOf(v)
		switch {
		case h.failing:
			failing = append(failing, v.ID)
		case h.incompatible:
			incompatible = append(incompatible, v.ID)
		}
	}
	switch {
	case len(failing) > 0:
		ids := strings.Join(failing, ", ")
		return Check{ID: CheckID, Status: proto.CheckFail, Code: messages.CheckPluginsFailing,
			Params: map[string]any{"ids": ids}, Message: "enabled plugins cannot run: " + ids}
	case len(incompatible) > 0:
		ids := strings.Join(incompatible, ", ")
		return Check{ID: CheckID, Status: proto.CheckWarn, Code: messages.CheckPluginsIncompatible,
			Params: map[string]any{"ids": ids}, Message: "enabled plugins need capabilities this server does not offer: " + ids}
	}
	return Check{ID: CheckID, Status: proto.CheckPass, Code: messages.CheckPluginsOK, Params: map[string]any{},
		Message: "no enabled plugin is crashlooping, failed, broken, revoked or incompatible"}
}

// WriteMetrics writes the gpuworks_plugin_* metrics of every installed
// package (pluginhost.Host.WriteMetrics) in the Prometheus text format.
func (b *Bridge) WriteMetrics(w io.Writer) error {
	views := b.List()
	rows := make([]pluginhost.MetricsRow, len(views))
	for i, v := range views {
		rows[i] = pluginhost.MetricsRow{ID: v.ID, State: v.State}
	}
	return b.host.WriteMetrics(w, rows)
}

// countApplied counts the entries of a push pushed state applied
// (pushstate.Effects.Applied).
func (b *Bridge) countApplied(owner, kind string, entries int) {
	if b.host != nil {
		b.host.CountPushRows(owner, kind, entries)
	}
}

// forgetHealth drops what the bridge keeps in memory for an uninstalled
// package: its counters. Its banners went with its entry.
func (b *Bridge) forgetHealth(id string) {
	b.mu.Lock()
	_, installed := b.entries[id]
	b.mu.Unlock()
	if !installed {
		b.host.ForgetMetrics(id)
	}
}
