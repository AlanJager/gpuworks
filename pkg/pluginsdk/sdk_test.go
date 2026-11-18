package pluginsdk_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
	"gpuworks.ai/gpuworks/pkg/pluginsdk/plugintest"
)

const manifestDigest = extapi.Digest("sha256:69a8fc5cf9fa0174e3dfe74b1c9c1f5f4f64710afa362419883f19c6b42ee422")

func baseConfig() pluginsdk.Config {
	return pluginsdk.Config{
		ID: "acme_backup", Version: "1.2.0", ManifestSHA256: manifestDigest,
		Requires: map[string]extapi.Range{extapi.TableCapability(extapi.KindBanners): {Min: 1, Max: 3}},
		Uses: map[string]extapi.Range{
			extapi.TableCapability(extapi.KindNodeHolds): {Min: 1, Max: 1},
			"host.events": {Min: 1, Max: 2},
		},
		Services: []string{"backup"},
	}
}

func banners(id string) *extapi.BannersV1 {
	return &extapi.BannersV1{Banners: []extapi.Banner{{ID: id, Audience: "admin",
		Steps: []extapi.BannerStep{{Severity: "info", Notice: &extapi.Notice{Code: "ACME_NOTICE"}}}}}}
}

func wireErr(t *testing.T, err error) *pluginsdk.Error {
	t.Helper()
	var e *pluginsdk.Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want a *pluginsdk.Error", err)
	}
	return e
}

// The handshake settles every capability on the newest version both sides
// speak, answers with the package's identity, and only then starts the
// plugin, whose push the host acknowledges with the body's digest.
func TestHandshakeNegotiatesAndStartPushes(t *testing.T) {
	cfg := baseConfig()
	pushed := make(chan pluginsdk.PushResult, 1)
	cfg.Start = func(ctx context.Context, p *pluginsdk.Plugin) error {
		r, err := p.PushState(ctx, extapi.KindBanners, banners("hello"))
		if err != nil {
			return err
		}
		pushed <- r
		return nil
	}
	h := plugintest.Start(t, cfg)
	resp, err := h.Initialize(h.Request())
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != cfg.ID || resp.Version != cfg.Version || resp.ManifestSHA256 != manifestDigest || resp.Protocol != 1 {
		t.Fatalf("response = %+v", resp)
	}
	want := map[string]int{"table.banners": 1, "table.node_holds": 1}
	if len(resp.Use) != len(want) || resp.Use["table.banners"] != 1 || resp.Use["table.node_holds"] != 1 {
		t.Fatalf("use = %v, want %v (host.events is not offered)", resp.Use, want)
	}
	push := h.WaitPush(extapi.KindBanners, 5*time.Second)
	if push.Ack == nil || push.Envelope.Generation != 1 || push.Envelope.Schema != 1 {
		t.Fatalf("push = %+v", push)
	}
	r := <-pushed
	body, _ := extapi.Marshal(banners("hello"))
	if r.Digest != extapi.DigestOf(body) || r.Ack.Digest != r.Digest || r.Generation != 1 {
		t.Fatalf("result = %+v, want the canonical body's digest", r)
	}
	if err := h.Shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// Every way the plugin refuses initialize answers a gwx/1 error the server
// can decode, under the status of its reason.
func TestHandshakeRefusals(t *testing.T) {
	cases := map[string]struct {
		cfg    func(*pluginsdk.Config)
		req    func(*extapi.HandshakeRequest)
		code   string
		reason string
		param  string
	}{
		"protocol": {
			req:  func(r *extapi.HandshakeRequest) { r.Protocol = extapi.Range{Min: 2, Max: 3} },
			code: extapi.ErrorProtocolUnsupported, reason: extapi.ReasonUnsupported, param: "supported",
		},
		"capability absent": {
			cfg: func(c *pluginsdk.Config) {
				c.Requires = map[string]extapi.Range{"host.fleet": {Min: 1, Max: 1}}
			},
			code: extapi.ErrorCapabilityMissing, reason: extapi.ReasonUnsupported, param: "capability",
		},
		"capability too old": {
			cfg: func(c *pluginsdk.Config) {
				c.Requires = map[string]extapi.Range{"table.availability": {Min: 2, Max: 2}}
			},
			code: extapi.ErrorCapabilityMissing, reason: extapi.ReasonUnsupported, param: "max",
		},
		"another package": {
			req:  func(r *extapi.HandshakeRequest) { r.Plugin.ID = "acme_other" },
			code: extapi.ErrorWireInvalid, reason: extapi.ReasonFailedPrecondition, param: "field",
		},
		"malformed": {
			req:  func(r *extapi.HandshakeRequest) { r.PrincipalKey = []byte{1} },
			code: extapi.ErrorWireInvalid, reason: extapi.ReasonInvalidArgument, param: "field",
		},
		"plugin's own refusal": {
			cfg: func(c *pluginsdk.Config) {
				c.Init = func(context.Context, *pluginsdk.Plugin) error {
					return pluginsdk.Refuse(extapi.ReasonFailedPrecondition, "ACME_STORE_LOCKED", "the store is locked", extapi.Params{"retries": 3})
				}
			},
			code: "ACME_STORE_LOCKED", reason: extapi.ReasonFailedPrecondition, param: "retries",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := baseConfig()
			if tc.cfg != nil {
				tc.cfg(&cfg)
			}
			h := plugintest.Start(t, cfg)
			req := h.Request()
			if tc.req != nil {
				tc.req(req)
			}
			_, err := h.Initialize(req)
			e := wireErr(t, err)
			if e.Wire.Code != tc.code || e.Wire.Reason != tc.reason || e.Status != extapi.StatusForReason(tc.reason) {
				t.Fatalf("refusal = %d %+v, want %s/%s", e.Status, e.Wire, tc.code, tc.reason)
			}
			if _, ok := e.Wire.Params[tc.param]; !ok {
				t.Fatalf("refusal params %v lack %s", e.Wire.Params, tc.param)
			}
			// A refused plugin is still not initialized: its routes refuse.
			resp, err := h.Do(mustRequest(t, http.MethodGet, "/r/jobs"))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusPreconditionFailed {
				t.Fatalf("route before initialize = %d", resp.StatusCode)
			}
		})
	}
}

func mustRequest(t *testing.T, method, path string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, "http://x"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// An Init that fails without a refusal ends the plugin as a crash; the
// server sees the process exit rather than a refusal it would trust.
func TestInitFailureEndsThePlugin(t *testing.T) {
	cfg := baseConfig()
	cfg.Init = func(context.Context, *pluginsdk.Plugin) error { return errors.New("disk on fire") }
	h := plugintest.Start(t, cfg)
	_, err := h.Initialize(h.Request())
	if e := wireErr(t, err); e.Wire.Reason != extapi.ReasonInternal {
		t.Fatalf("refusal = %+v", e.Wire)
	}
	if err := h.Wait(5 * time.Second); err == nil || !strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("Serve returned %v, want the init failure", err)
	}
}

func TestSecondInitializeIsAConflict(t *testing.T) {
	h := plugintest.Start(t, baseConfig())
	if _, err := h.Initialize(h.Request()); err != nil {
		t.Fatal(err)
	}
	_, err := h.Initialize(h.Request())
	if e := wireErr(t, err); e.Wire.Reason != extapi.ReasonConflict {
		t.Fatalf("second initialize = %+v", e.Wire)
	}
}

// A push the server already holds is skipped; a restarted plugin numbers
// its pushes past the held generation; a refused push still consumes its
// generation so the next one is never stale.
func TestPushGenerationsAndSkips(t *testing.T) {
	cfg := baseConfig()
	ready := make(chan *pluginsdk.Plugin, 1)
	cfg.Start = func(_ context.Context, p *pluginsdk.Plugin) error { ready <- p; return nil }
	h := plugintest.Start(t, cfg)
	body, _ := extapi.Marshal(banners("kept"))
	h.SetHeld(extapi.HeldState{Kind: extapi.KindBanners, Schema: 1, Generation: 41, Digest: extapi.DigestOf(body)})
	if _, err := h.Initialize(h.Request()); err != nil {
		t.Fatal(err)
	}
	p := <-ready
	ctx := context.Background()

	r, err := p.PushState(ctx, extapi.KindBanners, banners("kept"))
	if err != nil || !r.Skipped || r.Generation != 41 {
		t.Fatalf("held push = %+v, %v; want skipped at 41", r, err)
	}
	if n := len(h.Pushes()); n != 0 {
		t.Fatalf("a skipped push reached the host (%d pushes)", n)
	}
	r, err = p.PushState(ctx, extapi.KindBanners, banners("changed"))
	if err != nil || r.Generation != 42 || r.Ack == nil || r.Ack.AppliedAt == nil {
		t.Fatalf("changed push = %+v, %v; want generation 42 applied", r, err)
	}
	h.RefuseNextPush(extapi.KindBanners, extapi.WireError{Code: "PLUGIN_STATE_NOT_OWNER", Message: "not yours",
		Reason: extapi.ReasonPermissionDenied})
	_, err = p.PushState(ctx, extapi.KindBanners, banners("refused"))
	if e := wireErr(t, err); e.Status != http.StatusForbidden || e.Wire.Code != "PLUGIN_STATE_NOT_OWNER" {
		t.Fatalf("refused push = %d %+v", e.Status, e.Wire)
	}
	r, err = p.PushState(ctx, extapi.KindBanners, banners("refused"))
	if err != nil || r.Generation != 44 {
		t.Fatalf("push after a refusal = %+v, %v; want generation 44", r, err)
	}
	r, err = p.PushState(ctx, extapi.KindBanners, banners("refused"))
	if err != nil || !r.Skipped {
		t.Fatalf("repeat of the applied table = %+v, %v; want skipped", r, err)
	}
	if _, err := p.PushState(ctx, extapi.KindAvailability, &extapi.AvailabilityV1{}); !errors.Is(err, pluginsdk.ErrNotNegotiated) {
		t.Fatalf("push of an unnegotiated kind = %v", err)
	}
	if _, err := p.PushState(ctx, extapi.KindBanners, []int{1}); err == nil {
		t.Fatal("a table that is not an object was pushed")
	}
	if health, err := p.HostHealth(ctx); err != nil || health.Status != extapi.HealthOK {
		t.Fatalf("host health = %+v, %v", health, err)
	}
}

// A push whose body breaks its kind's rules is refused by the host with
// the wire code, and reaches the plugin as an *Error.
func TestInvalidTableIsRefused(t *testing.T) {
	cfg := baseConfig()
	ready := make(chan *pluginsdk.Plugin, 1)
	cfg.Start = func(_ context.Context, p *pluginsdk.Plugin) error { ready <- p; return nil }
	h := plugintest.Start(t, cfg)
	if _, err := h.Initialize(h.Request()); err != nil {
		t.Fatal(err)
	}
	p := <-ready
	bad := banners("x")
	bad.Banners[0].Audience = "everyone"
	_, err := p.PushState(context.Background(), extapi.KindBanners, bad)
	if e := wireErr(t, err); e.Wire.Code != extapi.ErrorStateRejected || e.Status != http.StatusBadRequest {
		t.Fatalf("invalid table = %d %+v", e.Status, e.Wire)
	}
}

func TestHealthAnswers(t *testing.T) {
	cfg := baseConfig()
	var degraded atomic.Bool
	cfg.Health = func(context.Context) extapi.Health {
		if degraded.Load() {
			return extapi.Health{Status: extapi.HealthDegraded, Code: "ACME_STORE_LOCKED", Params: extapi.Params{"retries": 2}}
		}
		return extapi.Health{Status: extapi.HealthOK}
	}
	h := plugintest.Start(t, cfg)
	if got, err := h.Health(); err != nil || got.Status != extapi.HealthOK {
		t.Fatalf("health = %+v, %v", got, err)
	}
	degraded.Store(true)
	if got, err := h.Health(); err != nil || got.Code != "ACME_STORE_LOCKED" {
		t.Fatalf("degraded health = %+v, %v", got, err)
	}
	cfg2 := baseConfig()
	cfg2.Health = func(context.Context) extapi.Health { return extapi.Health{Status: extapi.HealthDegraded} }
	h2 := plugintest.Start(t, cfg2)
	if _, err := h2.Health(); err == nil {
		t.Fatal("a degraded answer without a code was sent")
	}
}

// Routes reach the plugin's handler only after initialize, with the
// caller's deadline and the plugin attached.
func TestHandlerGetsDeadlineAndPlugin(t *testing.T) {
	cfg := baseConfig()
	cfg.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d, ok := r.Context().Deadline()
		p := pluginsdk.PluginFrom(r.Context())
		if !ok || time.Until(d) > time.Minute || p == nil || !p.Has("table.banners", 1) {
			http.Error(w, "missing context", http.StatusTeapot)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	h := plugintest.Start(t, cfg)
	if _, err := h.Initialize(h.Request()); err != nil {
		t.Fatal(err)
	}
	req := mustRequest(t, http.MethodPost, "/r/jobs")
	req.Header.Set(extapi.HeaderDeadline, "9999999999999")
	resp, err := h.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("a far deadline: status %d, want the handler to see it", resp.StatusCode)
	}
	req = mustRequest(t, http.MethodPost, "/r/jobs")
	req.Header.Set(extapi.HeaderDeadline, strconv.FormatInt(time.Now().Add(10*time.Second).UnixMilli(), 10))
	resp, err = h.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("route status %d", resp.StatusCode)
	}
}

// Shutdown cancels Start's context and Serve returns nil; a closed server
// end makes Serve return ErrServerGone; a failing Start ends Serve with
// its error.
func TestServeEnds(t *testing.T) {
	t.Run("shutdown", func(t *testing.T) {
		cfg := baseConfig()
		stopped := make(chan struct{})
		cfg.Start = func(ctx context.Context, _ *pluginsdk.Plugin) error {
			<-ctx.Done()
			close(stopped)
			return ctx.Err()
		}
		h := plugintest.Start(t, cfg)
		if _, err := h.Initialize(h.Request()); err != nil {
			t.Fatal(err)
		}
		if err := h.Shutdown(); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Fatal("Start's context was not cancelled")
		}
	})
	t.Run("server gone", func(t *testing.T) {
		h := plugintest.Start(t, baseConfig())
		if _, err := h.Initialize(h.Request()); err != nil {
			t.Fatal(err)
		}
		if err := h.Close(); !errors.Is(err, pluginsdk.ErrServerGone) {
			t.Fatalf("Serve after the server closed = %v", err)
		}
	})
	t.Run("start fails", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Start = func(context.Context, *pluginsdk.Plugin) error { return errors.New("cannot open store") }
		h := plugintest.Start(t, cfg)
		if _, err := h.Initialize(h.Request()); err != nil {
			t.Fatal(err)
		}
		if err := h.Wait(5 * time.Second); err == nil || !strings.Contains(err.Error(), "cannot open store") {
			t.Fatalf("Serve = %v", err)
		}
	})
}

// A config that would produce an invalid handshake answer is refused
// before anything is served.
func TestConfigIsValidated(t *testing.T) {
	for name, mutate := range map[string]func(*pluginsdk.Config){
		"id":       func(c *pluginsdk.Config) { c.ID = "Acme" },
		"digest":   func(c *pluginsdk.Config) { c.ManifestSHA256 = "sha256:x" },
		"both":     func(c *pluginsdk.Config) { c.Uses["table.banners"] = extapi.Range{Min: 1, Max: 1} },
		"range":    func(c *pluginsdk.Config) { c.Requires["table.banners"] = extapi.Range{Min: 2, Max: 1} },
		"services": func(c *pluginsdk.Config) { c.Services = []string{"b", "a"} },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := baseConfig()
			mutate(&cfg)
			h := plugintest.Start(t, cfg)
			if err := h.Wait(5 * time.Second); err == nil || !strings.Contains(err.Error(), "config") {
				t.Fatalf("Serve = %v, want a config error", err)
			}
		})
	}
}

// Run outside the server finds no descriptors and says so.
func TestRunNeedsTheServersDescriptors(t *testing.T) {
	t.Setenv(extapi.EnvProtocol, "")
	if err := pluginsdk.Run(context.Background(), baseConfig()); extapi.CodeOf(err) != extapi.ErrorProtocolUnsupported {
		t.Fatalf("Run without GWX_PROTOCOL = %v", err)
	}
	t.Setenv(extapi.EnvProtocol, "1")
	t.Setenv(extapi.EnvServeFD, "1")
	if err := pluginsdk.Run(context.Background(), baseConfig()); err == nil || !strings.Contains(err.Error(), "inherited") {
		t.Fatalf("Run with stdout as the serve descriptor = %v", err)
	}
}

// The fake host refuses what the server refuses: a push whose envelope
// names another kind than its path, and a generation that is not newer.
func TestFakeHostRefusesWhatTheServerRefuses(t *testing.T) {
	cfg := baseConfig()
	ready := make(chan *pluginsdk.Plugin, 1)
	cfg.Start = func(_ context.Context, p *pluginsdk.Plugin) error { ready <- p; return nil }
	h := plugintest.Start(t, cfg)
	h.SetHeld(extapi.HeldState{Kind: extapi.KindBanners, Schema: 1, Generation: 5, Digest: extapi.DigestOf([]byte("{}"))})
	if _, err := h.Initialize(h.Request()); err != nil {
		t.Fatal(err)
	}
	p := <-ready
	put := func(path, body string) int {
		req, _ := http.NewRequest(http.MethodPut, "http://gwx-host:80"+path, strings.NewReader(body))
		resp, err := p.HostClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if s := put(extapi.PathHostState+"node_holds", `{"body":{},"generation":9,"kind":"banners","schema":1}`); s != http.StatusBadRequest {
		t.Fatalf("kind mismatch = %d", s)
	}
	if s := put(extapi.PathHostState+"banners", `{"body":{},"generation":5,"kind":"banners","schema":1}`); s != http.StatusPreconditionFailed {
		t.Fatalf("stale generation = %d", s)
	}
	if s := put("/host/v1/nothing", `{}`); s != http.StatusNotFound {
		t.Fatalf("unknown endpoint = %d", s)
	}
	pushes := h.Pushes()
	if len(pushes) != 2 || pushes[0].Refusal == nil || pushes[1].Refusal == nil {
		t.Fatalf("pushes = %+v", pushes)
	}
	if r := pushes[1].Refusal; r.Code != "PLUGIN_STATE_STALE" || r.Reason != extapi.ReasonFailedPrecondition || r.Params["held"] != int64(5) {
		t.Fatalf("stale refusal = %+v", r)
	}
}
