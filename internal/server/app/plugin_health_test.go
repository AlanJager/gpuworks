package app

import (
	"context"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The plugin health banners reach administrators only, converted once per
// list: a new list is converted again, and an empty one answers nothing.
func TestHealthBannersAreForAdministratorsAndConvertedOncePerList(t *testing.T) {
	ctx := context.Background()
	var cur atomic.Pointer[[]pushstate.Banner]
	set := func(list []pushstate.Banner) { cur.Store(&list) }
	src := &healthBanners{list: func() []pushstate.Banner { return *cur.Load() }}
	set([]pushstate.Banner{{ID: "plugins.crashloop.acme_backup", Severity: extapi.SeverityDanger,
		Notice: messages.NewProductError(messages.ErrorPluginCrashloop, "plugin acme_backup keeps crashing",
			map[string]any{"id": "acme_backup", "crashes": 5, "window_s": 600, "retry_s": 300})}})

	if got := src.Banners(ctx, auth.Claims{Role: "user"}); got != nil {
		t.Fatalf("a member saw %v", got)
	}
	first := src.Banners(ctx, auth.Claims{Role: adminRole})
	if len(first) != 1 || first[0].ID != "plugins.crashloop.acme_backup" || first[0].Severity != extapi.SeverityDanger ||
		first[0].Message.Code != messages.ErrorPluginCrashloop || first[0].Message.Params["crashes"] != 5 {
		t.Fatalf("admin banners = %+v", first)
	}
	if again := src.Banners(ctx, auth.Claims{Role: adminRole}); &again[0] != &first[0] {
		t.Fatal("the same list was converted again")
	}
	set([]pushstate.Banner{{ID: "plugins.revoked.acme_backup", Severity: extapi.SeverityDanger,
		Notice: messages.NewProductError(messages.ErrorPluginRevoked, "plugin acme_backup is revoked", map[string]any{"id": "acme_backup"})}})
	if got := src.Banners(ctx, auth.Claims{Role: adminRole}); len(got) != 1 || got[0].Message.Code != messages.ErrorPluginRevoked {
		t.Fatalf("after a change = %+v", got)
	}
	set(nil)
	if got := src.Banners(ctx, auth.Claims{Role: adminRole}); got != nil {
		t.Fatalf("with no banner = %+v", got)
	}
}
