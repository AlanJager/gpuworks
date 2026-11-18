package app

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginbridge"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// pluginRoutes adapts the plugin host's route table to the API's dynamic
// extension: it converts each table once, the first time a request sees
// it, signs forwarded calls through the bridge and decodes the plugins'
// product errors.
type pluginRoutes struct {
	b    *pluginbridge.Bridge
	mu   sync.Mutex
	view atomic.Pointer[routeView]
}

// routeView is one route table as the API sees it.
type routeView struct {
	table *pluginbridge.RouteTable
	byKey map[routeKey]*api.DynamicRoute
	list  []api.DynamicRoute
}

type routeKey struct{ plugin, method, pattern string }

func newPluginRoutes(b *pluginbridge.Bridge) *pluginRoutes { return &pluginRoutes{b: b} }

// Dynamic is the API's view of the plugin routes.
func (p *pluginRoutes) Dynamic() *api.DynamicExtension {
	return &api.DynamicExtension{Match: p.match, Routes: p.routes, Forward: p.forward, DecodeError: decodePluginError}
}

// current is the view of the bridge's current table, built once per
// table.
func (p *pluginRoutes) current() *routeView {
	t := p.b.Routes()
	if v := p.view.Load(); v != nil && v.table == t {
		return v
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if v := p.view.Load(); v != nil && v.table == t {
		return v
	}
	v := &routeView{table: t, byKey: map[routeKey]*api.DynamicRoute{}}
	for _, r := range t.List() {
		v.list = append(v.list, dynamicRoute(r))
	}
	for i := range v.list {
		r := &v.list[i]
		v.byKey[routeKey{r.Plugin, r.Method, r.Pattern}] = r
	}
	p.view.Store(v)
	return v
}

func (p *pluginRoutes) match(method, path string) (*api.DynamicRoute, api.DynamicMatch) {
	v := p.current()
	r, _, res := v.table.Match(method, path)
	switch res {
	case pluginbridge.MatchFound:
		return v.byKey[routeKey{r.Plugin, r.Decl.Method, r.Decl.Pattern}], api.DynamicFound
	case pluginbridge.MatchMethodNotAllowed:
		return nil, api.DynamicMethodNotAllowed
	}
	return nil, api.DynamicMiss
}

func (p *pluginRoutes) routes() []api.DynamicRoute { return slices.Clone(p.current().list) }

func (p *pluginRoutes) forward(ctx context.Context, route *api.DynamicRoute, req *http.Request, c api.DynamicCaller) (*http.Response, error) {
	r, _ := route.Ref.(*pluginbridge.Route)
	if r == nil {
		return nil, messages.NewProductError(messages.ErrorPluginNotInstalled, "no plugin "+route.Plugin+" is installed", map[string]any{"id": route.Plugin})
	}
	req.URL.Path = extapi.PathRoutePrefix + req.URL.Path
	if req.URL.RawPath != "" {
		req.URL.RawPath = extapi.PathRoutePrefix + req.URL.RawPath
	}
	return p.b.Forward(ctx, r, req, pluginbridge.Caller{Kind: principalKind(c.Kind), UserID: uint64(c.UserID), Username: c.Username,
		Role: c.Role, LocalAdmin: c.LocalAdmin, APIKeyID: uint64(c.APIKeyID), RequestID: c.RequestID, Access: c.Access})
}

func principalKind(kind string) string {
	switch kind {
	case "session":
		return extapi.PrincipalSession
	case "token":
		return extapi.PrincipalToken
	}
	return extapi.PrincipalPublic
}

// dynamicRoute is r as the API serves it.
func dynamicRoute(r pluginbridge.Route) api.DynamicRoute {
	d := r.Decl
	out := api.DynamicRoute{Plugin: r.Plugin, Version: r.Version, Method: d.Method, Pattern: d.Pattern, Action: d.Action,
		Access: dynamicAccess(d.Access), Feature: featuregate.Key(d.Feature), WriteWhenReadOnly: d.WriteWhenReadOnly,
		AIReadable: d.AIReadable, Stream: d.Stream, HTML: d.HTML, Cookies: d.Cookies, MaxBody: r.MaxBody, Timeout: r.Timeout, Ref: &r}
	if d.Rate != nil {
		out.RatePerMinute, out.RateBurst = d.Rate.PerMinute, d.Rate.Burst
	}
	return out
}

func dynamicAccess(access string) api.DynamicAccess {
	switch access {
	case extapi.RouteAdminSession:
		return api.DynamicAdminSession
	case extapi.RouteMember:
		return api.DynamicMember
	case extapi.RoutePublic:
		return api.DynamicPublic
	}
	return api.DynamicAdmin
}

// decodePluginError is the product error a plugin's error body carries,
// when it is a valid gwx/1 error.
func decodePluginError(raw []byte) (*messages.ProductError, error) {
	we, err := extapi.DecodeWireError(raw)
	if err != nil {
		return nil, err
	}
	return messages.NewProductError(we.Code, we.Message, map[string]any(we.Params)), nil
}

// compiledNames are the names of the compiled modules and of the
// extensions they mount: no package may take one as its id.
func compiledNames(modules []Module, apis []api.Extension) []string {
	var out []string
	for _, m := range modules {
		out = append(out, m.Name())
	}
	for _, e := range apis {
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return slices.Compact(out)
}
