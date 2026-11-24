package extmodule

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
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
// whole server serves: everything; reads and the data plane; or reads
// alone. Its own routes are served under any limit. The console manifest
// follows it.
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
		{api.PlatformLimit{Access: featuregate.Locked, Link: "/x/fake"}, 200, 403, 200, 200, messages.ErrorPlatformLocked, `"platform":{"access":"locked","link":"/x/fake"`},
		{api.PlatformLimit{Access: featuregate.Locked, Notice: notice}, 200, 403, 200, 200, "ACME_LAPSED", `"platform":{"access":"locked","message":{"code":"ACME_LAPSED"`},
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

// A module on a single server registers what it answers the other members
// and has nobody to ask: AskMember says so instead of failing some other
// way, so one code path serves a single server and a cluster.
func TestASingleServerHasNoMemberToAsk(t *testing.T) {
	m := &askingModule{}
	testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{m}}, nil)
	if _, err := m.host.AskMember(context.Background(), 2, []byte("x")); !errors.Is(err, app.ErrNotClusterMember) {
		t.Fatalf("AskMember on a single server = %v, want %v", err, app.ErrNotClusterMember)
	}
	if !m.twicePanicked || !m.nilPanicked {
		t.Fatalf("a second answerer panicked: %t, a nil one: %t", m.twicePanicked, m.nilPanicked)
	}
	if !panics(func() { m.host.AnswerMembers(func(context.Context, []byte) ([]byte, error) { return nil, nil }) }) {
		t.Fatal("AnswerMembers after Init must panic")
	}
}

type askingModule struct {
	host                       *app.Host
	twicePanicked, nilPanicked bool
}

func (*askingModule) Name() string { return "fake" }
func (m *askingModule) Init(_ context.Context, h *app.Host) error {
	m.host = h
	answer := func(context.Context, []byte) ([]byte, error) { return []byte("here"), nil }
	m.nilPanicked = panics(func() { h.AnswerMembers(nil) })
	h.AnswerMembers(answer)
	m.twicePanicked = panics(func() { h.AnswerMembers(answer) })
	return nil
}

// On a limited server a machine of a cluster that already joined gets its
// identity again when it asks without one (its pod was replaced and the
// saved identity is gone), and a machine that is new is refused by the
// module's insert guard, with the refusal the agent reads as final.
func TestAJoinedClusterNodeEnrollsAgainOnALimitedServer(t *testing.T) {
	limiter := newFakeLimiter(featuregate.Full)
	guard := func(store.FleetCapacity) *messages.ProductError {
		if limiter.PlatformLimit().Access == featuregate.Full {
			return nil
		}
		return messages.NewProductError("FAKE_LAPSED", "the fake term ended", nil)
	}
	e := testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{&fakeModule{name: "fake", limiter: limiter, guard: guard}}}, nil)
	client := e.API(t)
	cluster, err := e.Store().CreateKubeCluster(store.KubeCluster{Name: "joined"})
	if err != nil {
		t.Fatal(err)
	}
	enroll := func(node string) (int, string, map[string]any) {
		status, body := send(t, client, http.MethodPost, "/api/v1/kube/enroll", `{"token":"`+cluster.Token+`","cluster_uid":"uid-1","node_name":"`+node+`"}`)
		var out struct {
			NodeUUID  string `json:"node_uuid"`
			NodeToken string `json:"node_token"`
		}
		_ = json.Unmarshal(body, &out)
		_, params := productError(body)
		if status == http.StatusOK && out.NodeToken == "" {
			t.Fatalf("enrolled %s without a node token: %s", node, body)
		}
		return status, out.NodeUUID, params
	}
	status, first, _ := enroll("worker-1")
	if status != http.StatusOK || first == "" {
		t.Fatalf("enroll while unlimited = %d", status)
	}
	for _, access := range []featuregate.Access{featuregate.ReadOnly, featuregate.Locked} {
		limiter.set(api.PlatformLimit{Access: access})
		if status, again, _ := enroll("worker-1"); status != http.StatusOK || again != first {
			t.Fatalf("%s: the joined node enrolling again = %d as %q, want 200 as %q", access, status, again, first)
		}
		status, _, params := enroll("worker-2")
		if status != http.StatusForbidden || params["reason"] != "enrollment_refused" || params["refusal_code"] != "FAKE_LAPSED" {
			t.Fatalf("%s: a new node = %d %v, want the insert guard's refusal", access, status, params)
		}
	}
	if nodes, err := e.Store().ListNodes(); err != nil || len(nodes) != 1 {
		t.Fatalf("nodes = %d, %v; want the one that joined", len(nodes), err)
	}
}
