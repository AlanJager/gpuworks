//go:build linux || darwin

package pluginbridge

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
)

// A route snapshot authorizes only the package that declared it. A later
// build can tighten a public route without changing its path or action;
// that build must never receive the earlier public principal. Restarting
// the same build, including an idle on-demand start, remains allowed.
func TestForwardKeepsAuthorizationWithTheMatchedBuild(t *testing.T) {
	const id, action = "acme_driver", "acme_driver.jobs_get"
	var calls atomic.Int32
	sp := &sdkSpawner{handler: pluginsdk.RequirePrincipal(action, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		p := pluginsdk.PrincipalFrom(r)
		body := p.Kind
		if p.PlatformAdmin {
			body += ":admin"
		}
		_, _ = io.WriteString(w, body)
	}))}
	w := newWorld(t, nil)
	b, _ := w.openWith(sp, callTiming())
	pkg := func(version, access string) []byte {
		return w.pkg(id, version, func(s *extsign.Spec) {
			onDemand(s)
			s.Manifest.Permissions = []string{"http:public", "http:routes"}
			s.Manifest.Requires = map[string]int{extapi.CapRoutes: 1}
			s.Manifest.Routes = []extapi.RouteDecl{{Method: http.MethodGet, Pattern: "/jobs", Action: action, Access: access, Ungated: true}}
		})
	}
	match := func() *Route {
		t.Helper()
		r, _, found := b.Routes().Match(http.MethodGet, "/"+id+"/jobs")
		if found != MatchFound {
			t.Fatalf("installed route not found: %v", found)
		}
		return r
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	forward := func(r *Route, caller Caller) (string, error) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, extapi.PathRoutePrefix+"/jobs", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := b.Forward(ctx, r, req, caller)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("plugin response = %d %s", resp.StatusCode, body)
		}
		return string(body), err
	}

	install(t, b, pkg("1.0.0", extapi.RoutePublic), true)
	before := match()
	public := Caller{Kind: extapi.PrincipalPublic, RequestID: "before-upgrade"}
	for range 2 {
		if body, err := forward(before, public); err != nil || body != extapi.PrincipalPublic {
			t.Fatalf("same build public call = %q, %v", body, err)
		}
		waitState(t, b, id, StateIdle)
	}
	if sp.count() != 2 || calls.Load() != 2 {
		t.Fatalf("same-build idle restarts: %d spawns, %d calls", sp.count(), calls.Load())
	}

	install(t, b, pkg("2.0.0", extapi.RouteAdmin), true)
	if before.Decl.Access != extapi.RoutePublic || match().Decl.Access != extapi.RouteAdmin {
		t.Fatal("the upgrade did not keep the old snapshot and replace its policy")
	}
	if body, err := forward(before, public); !isCode(err, messages.ErrorPluginUnavailable) {
		t.Errorf("stale public authorization reached the replacement: body %q, error %v", body, err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("replacement handled a stale authorization: %d calls, want 2", n)
	}
	admin := Caller{Kind: extapi.PrincipalSession, UserID: 1, Username: "ada", Role: "admin", RequestID: "after-upgrade"}
	if body, err := forward(match(), admin); err != nil || body != extapi.PrincipalSession+":admin" {
		t.Fatalf("fresh administrator authorization = %q, %v", body, err)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("authorized calls = %d, want 3", n)
	}
}
