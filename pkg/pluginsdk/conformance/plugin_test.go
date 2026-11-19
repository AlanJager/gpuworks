package conformance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
)

// The test binary is also the plugin under test: started with envPlugin
// set, TestMain runs runTestPlugin instead of the tests. envManifest names
// the manifest it answers for, envMode its comma-separated faults.
const (
	envPlugin   = "CONFORMANCE_TEST_PLUGIN"
	envManifest = "CONFORMANCE_TEST_MANIFEST"
	envMode     = "CONFORMANCE_TEST_MODE"
)

// The faults, each breaking one rule:
const (
	// modeSkipPrincipal serves routes without checking the principal.
	modeSkipPrincipal = "skip-principal-check"
	// modeIgnoreAction checks the principal's MAC but not its route
	// action.
	modeIgnoreAction = "ignore-route-action"
	// modeActionInDeclaredSet checks the principal's MAC and accepts any
	// route action the manifest declares, not only this route's.
	modeActionInDeclaredSet = "action-in-declared-set"
	// modeHangHealth never answers health.
	modeHangHealth = "hang-health"
	// modeLinger keeps running for lingerFor after shutdown.
	modeLinger = "linger-after-shutdown"
	// modeAlwaysPush pushes its table past the SDK, even when the server
	// holds it.
	modeAlwaysPush = "always-push"
	// modeUndeclaredCapability negotiates host.nodes, which the manifest
	// does not declare.
	modeUndeclaredCapability = "undeclared-capability"
	// modeUndeclaredCall reads the server's meta without negotiating
	// host.clock.
	modeUndeclaredCall = "undeclared-call"
)

// lingerFor outlasts the shutdown timeout the tests run with.
const lingerFor = 3 * time.Second

// runTestPlugin is a small plugin: it requires table.banners, pushes one
// banner at start, and serves the manifest's routes behind
// RequirePrincipal; envMode adds faults.
func runTestPlugin() {
	modes := strings.Split(os.Getenv(envMode), ",")
	has := func(m string) bool { return slices.Contains(modes, m) }
	raw, err := os.ReadFile(os.Getenv(envManifest))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	m, err := extapi.DecodeManifest(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	version := m.Version
	if has(modeWrongVersion) {
		version = "9.9.9"
	}
	cfg := pluginsdk.Config{ID: m.ID, Version: version, ManifestSHA256: extapi.DigestOf(raw),
		Requires: map[string]extapi.Range{extapi.TableCapability(extapi.KindBanners): {Min: 1, Max: 1}},
		Handler:  routes(m.Routes, has)}
	if has(modeUndeclaredCapability) {
		cfg.Uses = map[string]extapi.Range{extapi.CapHostNodes: {Min: 1, Max: 1}}
	}
	if has(modeHangHealth) {
		cfg.Health = func(ctx context.Context) extapi.Health {
			<-ctx.Done()
			return extapi.Health{Status: extapi.HealthOK}
		}
	}
	cfg.Start = func(ctx context.Context, p *pluginsdk.Plugin) error {
		if has(modeUndeclaredCall) {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://gwx-host"+extapi.PathHostMeta, nil)
			if resp, err := p.HostClient().Do(req); err == nil {
				resp.Body.Close()
			}
		}
		if has(modeNoPush) || (has(modePushOnlyFresh) && len(p.Request().Held) > 0) {
			return nil
		}
		table := &extapi.BannersV1{Banners: []extapi.Banner{{ID: "maintenance", Audience: "admin",
			Steps: []extapi.BannerStep{{Severity: "info", Notice: &extapi.Notice{Code: "ACME_NOTICE"}}}}}}
		if has(modeAlwaysPush) {
			return pushPastTheSDK(ctx, p, table)
		}
		_, err := p.PushState(ctx, extapi.KindBanners, table)
		return err
	}
	if has(modeLinger) {
		_ = pluginsdk.Run(context.Background(), cfg)
		time.Sleep(lingerFor)
		os.Exit(0)
	}
	pluginsdk.Main(cfg)
}

// routes serves each route, behind RequirePrincipal unless a fault says
// otherwise.
func routes(decls []extapi.RouteDecl, has func(string) bool) http.Handler {
	mux := http.NewServeMux()
	for _, r := range decls {
		pattern := r.Method + " " + extapi.PathRoutePrefix + r.Pattern
		if r.Pattern == "/" {
			pattern += "{$}"
		}
		ok := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		})
		switch {
		case has(modeRefuseAll):
			mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(extapi.HeaderProductError, "1")
				w.WriteHeader(http.StatusForbidden)
			})
		case has(modeSkipPrincipal):
			mux.Handle(pattern, ok)
		case has(modeIgnoreAction):
			mux.Handle(pattern, macOnly(ok, nil))
		case has(modeActionInDeclaredSet):
			mux.Handle(pattern, macOnly(ok, decls))
		default:
			mux.Handle(pattern, pluginsdk.RequirePrincipal(r.Action, ok))
		}
	}
	return mux
}

// macOnly verifies the principal but not which route it was signed for;
// with decls, it also requires the action to be one of theirs.
func macOnly(h http.Handler, decls []extapi.RouteDecl) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := pluginsdk.PluginFrom(r.Context()).Request().PrincipalKey
		p, _, err := extapi.VerifyPrincipal(key, r.Header.Get(extapi.HeaderPrincipal),
			r.Header.Get(extapi.HeaderPrincipalMac), r.Header.Get(extapi.HeaderFeatureAccess), time.Now())
		if err == nil && decls != nil && !slices.ContainsFunc(decls, func(d extapi.RouteDecl) bool { return d.Action == p.RouteAction }) {
			err = errors.New("the action is not declared")
		}
		if err != nil {
			w.Header().Set(extapi.HeaderProductError, "1")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// pushPastTheSDK pushes table above the held generation without the SDK's
// digest check.
func pushPastTheSDK(ctx context.Context, p *pluginsdk.Plugin, table *extapi.BannersV1) error {
	var gen uint64
	for _, h := range p.Request().Held {
		if h.Kind == extapi.KindBanners {
			gen = h.Generation
		}
	}
	body, err := json.Marshal(table)
	if err != nil {
		return err
	}
	envelope, err := extapi.Marshal(extapi.StatePush{Kind: extapi.KindBanners, Schema: 1, Generation: gen + 1, Body: body})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://gwx-host"+extapi.PathHostState+extapi.KindBanners, bytes.NewReader(envelope))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", extapi.ContentTypeJSON)
	resp, err := p.HostClient().Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}
