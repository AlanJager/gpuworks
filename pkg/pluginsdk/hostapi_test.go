package pluginsdk_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
	"gpuworks.ai/gpuworks/pkg/pluginsdk/plugintest"
)

// startAuthority runs a plugin that uses the clock, own-state and key calls
// and returns it once initialized; grant adjusts the initialize request.
func startAuthority(t *testing.T, grant func(*extapi.HandshakeRequest)) (*plugintest.Harness, *pluginsdk.Plugin) {
	t.Helper()
	cfg := baseConfig()
	cfg.Uses = map[string]extapi.Range{
		extapi.CapHostClock: {Min: 1, Max: 1}, extapi.CapHostStateRead: {Min: 1, Max: 1}, extapi.CapHostDeriveKey: {Min: 1, Max: 1},
	}
	started := make(chan *pluginsdk.Plugin, 1)
	cfg.Start = func(_ context.Context, p *pluginsdk.Plugin) error { started <- p; return nil }
	h := plugintest.Start(t, cfg)
	req := h.Request()
	if grant != nil {
		grant(req)
	}
	if _, err := h.Initialize(req); err != nil {
		t.Fatal(err)
	}
	return h, <-started
}

func grantAll(r *extapi.HandshakeRequest) {
	r.Plugin.PermissionsGranted = []string{"crypto:derive_key"}
	r.Plugin.RolesGranted = []string{"clock_authority"}
}

func TestClockCallsThroughTheSDK(t *testing.T) {
	ctx := context.Background()
	h, p := startAuthority(t, grantAll)
	m, err := p.Meta(ctx)
	if err != nil || m.BootID == "" || m.Version == "" {
		t.Fatalf("meta = %+v, %v", m, err)
	}
	to := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	c, err := p.CorrectClock(ctx, to, "acme correction code 7F3A-22C1")
	if err != nil || !c.To.Equal(to) {
		t.Fatalf("correction = %+v, %v", c, err)
	}
	if got := h.Corrections(); len(got) != 1 || got[0].Reason != "acme correction code 7F3A-22C1" {
		t.Fatalf("corrections = %+v", got)
	}
	_, err = p.CorrectClock(ctx, time.Now().Add(-400*24*time.Hour), "too far")
	if e := wireErr(t, err); e.Wire.Code != "PLUGIN_CLOCK_CORRECTION_TOO_LARGE" || e.Status != 400 {
		t.Fatalf("too far = %+v", e)
	}
	if _, err := p.CorrectClock(ctx, to, ""); extapi.CodeOf(err) != extapi.ErrorWireInvalid {
		t.Fatalf("an empty reason was sent: %v", err)
	}
	for range 2 {
		if err := p.Ready(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if h.Readies() != 2 {
		t.Fatalf("readies = %d", h.Readies())
	}

	_, plain := startAuthority(t, nil)
	_, err = plain.CorrectClock(ctx, to, "no role")
	if e := wireErr(t, err); e.Wire.Code != "PLUGIN_ROLE_REQUIRED" || e.Wire.Params["role"] != "clock_authority" || e.Status != 403 {
		t.Fatalf("without the role = %+v", e)
	}
}

func TestDeriveKeyThroughTheSDK(t *testing.T) {
	ctx := context.Background()
	_, p := startAuthority(t, grantAll)
	a, err := p.DeriveKey(ctx, "usb.challenge", 32)
	if err != nil || len(a) != 32 {
		t.Fatalf("key = %x, %v", a, err)
	}
	again, _ := p.DeriveKey(ctx, "usb.challenge", 32)
	other, _ := p.DeriveKey(ctx, "backup.seal", 32)
	if !bytes.Equal(a, again) || bytes.Equal(a, other) {
		t.Fatal("keys are not deterministic per label")
	}
	if _, err := p.DeriveKey(ctx, "Bad Label", 32); extapi.CodeOf(err) != extapi.ErrorWireInvalid {
		t.Fatalf("an invalid label was sent: %v", err)
	}
	_, plain := startAuthority(t, nil)
	_, err = plain.DeriveKey(ctx, "usb.challenge", 32)
	if e := wireErr(t, err); e.Wire.Code != "PLUGIN_HOST_NOT_PERMITTED" || e.Wire.Params["permission"] != "crypto:derive_key" {
		t.Fatalf("without the permission = %+v", e)
	}
}

// A restarted plugin reads back what it pushed; a kind it never pushed is
// not held.
func TestPushedStateReadsBackTheOwnTable(t *testing.T) {
	ctx := context.Background()
	cfg := baseConfig()
	cfg.Uses = map[string]extapi.Range{extapi.CapHostStateRead: {Min: 1, Max: 1}}
	started := make(chan *pluginsdk.Plugin, 1)
	cfg.Start = func(_ context.Context, p *pluginsdk.Plugin) error { started <- p; return nil }
	h := plugintest.Start(t, cfg)
	if _, err := h.Initialize(h.Request()); err != nil {
		t.Fatal(err)
	}
	p := <-started
	r, err := p.PushState(ctx, extapi.KindBanners, banners("hello"))
	if err != nil {
		t.Fatal(err)
	}
	held, err := p.PushedState(ctx, extapi.KindBanners)
	if err != nil || !held.Held() || held.Digest != r.Digest || held.Generation != r.Generation {
		t.Fatalf("held = %+v, %v", held, err)
	}
	table, err := held.Table()
	if err != nil || table.(*extapi.BannersV1).Banners[0].ID != "hello" {
		t.Fatalf("table = %+v, %v", table, err)
	}
	if none, err := p.PushedState(ctx, extapi.KindNodeHolds); err != nil || none.Held() {
		t.Fatalf("not pushed = %+v, %v", none, err)
	}
}

// A call whose capability the handshake did not settle on fails in the
// plugin, without a round trip.
func TestHostCallsNeedTheirCapability(t *testing.T) {
	ctx := context.Background()
	cfg := baseConfig()
	started := make(chan *pluginsdk.Plugin, 1)
	cfg.Start = func(_ context.Context, p *pluginsdk.Plugin) error { started <- p; return nil }
	h := plugintest.Start(t, cfg)
	if _, err := h.Initialize(h.Request()); err != nil {
		t.Fatal(err)
	}
	p := <-started
	for name, err := range map[string]error{
		"meta":    func() error { _, err := p.Meta(ctx); return err }(),
		"correct": func() error { _, err := p.CorrectClock(ctx, time.Now(), "x"); return err }(),
		"state":   func() error { _, err := p.PushedState(ctx, extapi.KindBanners); return err }(),
		"key":     func() error { _, err := p.DeriveKey(ctx, "usb", 16); return err }(),
	} {
		if !errors.Is(err, pluginsdk.ErrNotNegotiated) {
			t.Errorf("%s = %v, want ErrNotNegotiated", name, err)
		}
	}
	if err := p.Ready(ctx); err != nil {
		t.Fatalf("ready is part of the base protocol: %v", err)
	}
}
