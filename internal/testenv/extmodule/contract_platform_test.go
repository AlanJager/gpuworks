package extmodule

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// fakeLimiter is a platform limit a test switches; it answers from one
// atomic load, as a real one must.
type fakeLimiter struct {
	cur atomic.Pointer[api.PlatformLimit]
}

func newFakeLimiter(access featuregate.Access) *fakeLimiter {
	l := &fakeLimiter{}
	l.set(api.PlatformLimit{Access: access})
	return l
}

func (l *fakeLimiter) set(limit api.PlatformLimit)      { l.cur.Store(&limit) }
func (l *fakeLimiter) PlatformLimit() api.PlatformLimit { return *l.cur.Load() }

type limiterModule struct{ name string }

func (m *limiterModule) Name() string { return m.name }
func (m *limiterModule) Init(_ context.Context, h *app.Host) error {
	return h.LimitPlatform(newFakeLimiter(featuregate.Full))
}

// The module that limits the server decides, request by request, what the
// whole server serves: everything, reads and the data plane, or only the
// recovery surface and its own routes. The console manifest follows it.
func TestAPlatformLimiterDecidesWhatTheServerServes(t *testing.T) {
	limiter := newFakeLimiter(featuregate.Full)
	first := &fakeModule{name: "fake", source: newFakeSource(featuregate.Full), limiter: limiter}
	e := testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{first}}, nil)
	client := e.API(t)
	notice := messages.NewProductError("ACME_LAPSED", "the acme term ended", nil)
	for _, c := range []struct {
		limit                       api.PlatformLimit
		read, change, own, manifest int
		code                        string
		platform                    string
	}{
		{api.PlatformLimit{Access: featuregate.Full}, 200, 400, 200, 200, "", ""},
		{api.PlatformLimit{Access: featuregate.ReadOnly}, 200, 403, 200, 200, messages.ErrorPlatformReadOnly, `"platform":{"access":"read_only"`},
		{api.PlatformLimit{Access: featuregate.Locked, Link: "/x/fake"}, 403, 403, 200, 200, messages.ErrorPlatformLocked, `"platform":{"access":"locked","link":"/x/fake"`},
		{api.PlatformLimit{Access: featuregate.Locked, Notice: notice}, 403, 403, 200, 200, "ACME_LAPSED", `"platform":{"access":"locked","message":{"code":"ACME_LAPSED"`},
	} {
		limiter.set(c.limit)
		name := c.limit.Access.String()
		if status, body := send(t, client, http.MethodGet, "/api/v1/nodes", ""); status != c.read {
			t.Fatalf("%s: a read = %d %s", name, status, body)
		}
		status, body := send(t, client, http.MethodPost, "/api/v1/apikeys", "{}")
		if code, _ := productError(body); status != c.change || (c.code != "" && code != c.code) {
			t.Fatalf("%s: a change = %d %s", name, status, body)
		}
		// The limiting module's own routes are served under any limit, so
		// its page can lift it; they still answer to their feature.
		if status, body := send(t, client, http.MethodPost, "/api/v1/x/fake/reports", "{}"); status != c.own {
			t.Fatalf("%s: the module's own route = %d %s", name, status, body)
		}
		status, manifest := send(t, client, http.MethodGet, "/api/v1/console/manifest", "")
		if status != c.manifest || strings.Contains(string(manifest), `"platform"`) != (c.platform != "") || !strings.Contains(string(manifest), c.platform) {
			t.Fatalf("%s: manifest = %d %s", name, status, manifest)
		}
	}
	limiter.set(api.PlatformLimit{Access: featuregate.Full})

	if err := first.host.LimitPlatform(newFakeLimiter(featuregate.Full)); !errors.Is(err, app.ErrHostClosed) {
		t.Fatalf("LimitPlatform after Init = %v", err)
	}
}

// A nil limiter is refused, and no module that limits the server leaves it
// unlimited.
func TestHostRefusesANilPlatformLimiter(t *testing.T) {
	e := testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{&nilLimiterModule{}}}, nil)
	if status, body := send(t, e.API(t), http.MethodGet, "/api/v1/nodes", ""); status != http.StatusOK {
		t.Fatalf("a server nobody limits = %d %s", status, body)
	}
}

type nilLimiterModule struct{}

func (*nilLimiterModule) Name() string { return "fake" }
func (*nilLimiterModule) Init(_ context.Context, h *app.Host) error {
	if err := h.LimitPlatform(nil); err == nil {
		return errors.New("LimitPlatform(nil) was accepted")
	}
	return nil
}
