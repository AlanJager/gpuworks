package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestConsoleManifestOfACommunityServer(t *testing.T) {
	rig := newTokenRig(t)
	read := rig.mint("read")
	if rec := rig.do("", "GET", "/api/v1/console/manifest", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d %s", rec.Code, rec.Body)
	}
	for _, bearer := range []string{rig.session, read} {
		rec := rig.do(bearer, "GET", "/api/v1/console/manifest", "")
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"edition":"community","features":[],"nav":[],"banners":[]}` {
			t.Fatalf("community manifest = %d %s", rec.Code, rec.Body)
		}
	}
	if rec := rig.do(rig.session, "POST", "/api/v1/console/manifest", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", rec.Code)
	}
}

// bannerFunc is a BannerSource from a function.
type bannerFunc func(context.Context, auth.Claims) []dto.ConsoleBanner

func (f bannerFunc) Banners(ctx context.Context, viewer auth.Claims) []dto.ConsoleBanner {
	return f(ctx, viewer)
}

func TestConsoleManifestCarriesFeaturesNavAndBanners(t *testing.T) {
	src := &switchSource{}
	src.set(map[featuregate.Key]featuregate.Availability{
		"fake.audit": {Access: featuregate.Full, Link: "https://example.invalid/never-shown", Limits: []featuregate.Limit{{Dim: "cards", Max: 8}, {Dim: "nodes", Unlimited: true}}},
		"fake.sso":   {Access: featuregate.Locked, Link: "https://example.invalid/sso"},
		"fake.pool":  {Access: featuregate.ReadOnly, Notice: messages.NewProductError("SOME_NOTICE", "read-only since yesterday", map[string]any{"since": "yesterday"})},
		// fake.infra is concealed and locked: hidden.
	})
	gate := mustGate(t, src,
		featuregate.Feature{Key: "fake.audit"},
		featuregate.Feature{Key: "fake.sso"},
		featuregate.Feature{Key: "fake.pool"},
		featuregate.Feature{Key: "fake.infra", Presentation: featuregate.Conceal},
	)
	ext := fakeNavExtension{fakeExtension{name: "fake", nav: []dto.NavContribution{
		{ID: "fake-audit", Group: "system", LabelKey: "fake:nav.audit", Icon: "audit", Route: "/fake/audit", Feature: "fake.audit"},
		{ID: "fake-sso", Group: "system", LabelKey: "fake:nav.sso", Route: "/fake/sso", Feature: "fake.sso"},
		{ID: "fake-infra", Group: "system", LabelKey: "fake:nav.infra", Route: "/fake/infra", Feature: "fake.infra"},
		{ID: "fake-about", Group: "system", LabelKey: "fake:nav.about", Route: "/fake/about"},
	}}}
	var viewers []string
	banners := bannerFunc(func(_ context.Context, viewer auth.Claims) []dto.ConsoleBanner {
		viewers = append(viewers, viewer.Username)
		return []dto.ConsoleBanner{
			{ID: "b1", Severity: "warning", Message: dto.ProductMessage{Code: "SOME_NOTICE", Params: map[string]any{"days": 3}, Message: "three days left"}, Link: "/settings"},
			{ID: "b2", Severity: "loud", Message: dto.ProductMessage{Code: "OTHER", Message: "other"}},
			{ID: "", Severity: "info", Message: dto.ProductMessage{Code: "NO_ID", Message: "dropped"}},
			{ID: "b3", Severity: "info", Message: dto.ProductMessage{Message: "no code, dropped"}},
			{ID: "b4", Severity: "info", Message: dto.ProductMessage{Code: "ACKABLE", Message: "can be acknowledged"}, AckKey: "b4:stage-30:2027-01-01T00:00:00Z"},
			{ID: "b5", Severity: "info", Message: dto.ProductMessage{Code: "BAD_KEY", Message: "key dropped"}, AckKey: "has space"},
			{ID: "b6", Severity: "info", Message: dto.ProductMessage{Code: "LONG_KEY", Message: "key dropped"}, AckKey: strings.Repeat("k", 129)},
		}
	})
	rig := newTokenRig(t, func(d *Deps) {
		d.Extensions = ExtensionDeps{Extensions: []Extension{ext}, Features: gate, Banners: []BannerSource{banners}, Edition: "partner"}
	})
	rec := rig.do(rig.session, "GET", "/api/v1/console/manifest", "")
	want := `{"edition":"partner",` +
		`"features":[` +
		`{"key":"fake.audit","access":"full","presentation":"promote","limits":[{"dim":"cards","max":8},{"dim":"nodes","max":null}]},` +
		`{"key":"fake.infra","access":"hidden","presentation":"hidden","limits":[]},` +
		`{"key":"fake.pool","access":"read_only","presentation":"promote","message":{"code":"SOME_NOTICE","params":{"since":"yesterday"},"message":"read-only since yesterday"},"limits":[]},` +
		`{"key":"fake.sso","access":"locked","presentation":"promote","link":"https://example.invalid/sso","limits":[]}],` +
		`"nav":[` +
		`{"id":"fake-audit","group":"system","label_key":"fake:nav.audit","icon":"audit","route":"/fake/audit","feature":"fake.audit"},` +
		`{"id":"fake-sso","group":"system","label_key":"fake:nav.sso","route":"/fake/sso","feature":"fake.sso"},` +
		`{"id":"fake-about","group":"system","label_key":"fake:nav.about","route":"/fake/about"}],` +
		`"banners":[` +
		`{"id":"b1","severity":"warning","message":{"code":"SOME_NOTICE","params":{"days":3},"message":"three days left"},"link":"/settings"},` +
		`{"id":"b2","severity":"info","message":{"code":"OTHER","message":"other"}},` +
		`{"id":"b4","severity":"info","message":{"code":"ACKABLE","message":"can be acknowledged"},"ack_key":"b4:stage-30:2027-01-01T00:00:00Z"},` +
		`{"id":"b5","severity":"info","message":{"code":"BAD_KEY","message":"key dropped"}},` +
		`{"id":"b6","severity":"info","message":{"code":"LONG_KEY","message":"key dropped"}}]}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("manifest = %d\n%s\nwant\n%s", rec.Code, rec.Body, want)
	}
	if len(viewers) != 1 || viewers[0] != "admin" {
		t.Fatalf("banner sources saw viewers %v, want [admin]", viewers)
	}
	// Round trip: the wire decodes into the DTO and encodes back the same.
	var m dto.ConsoleManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(m)
	if string(again) != want {
		t.Fatalf("round trip = %s", again)
	}
	// The answer follows the source: once sso is available its entry stays,
	// once infra is, its entry appears.
	src.set(map[featuregate.Key]featuregate.Availability{"fake.infra": {Access: featuregate.Full}})
	rec = rig.do(rig.session, "GET", "/api/v1/console/manifest", "")
	if !strings.Contains(rec.Body.String(), `"id":"fake-infra"`) || strings.Contains(rec.Body.String(), `"id":"fake-audit"`) {
		t.Fatalf("manifest after the source changed: %s", rec.Body)
	}
}
