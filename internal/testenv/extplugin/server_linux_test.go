//go:build linux

package extplugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The contract suite runs real servers (app.New over a data directory and a
// database file that outlive each boot) with real plugin processes: the
// fake plugin, installed as a signed package through /api/v1/plugins and
// started through this test binary as the trampoline.

const adminPassword = "gpuworks-test-password-2026"

// acmeCodes are the codes the fake package's manifest lists.
var acmeCodes = []string{"ACME_CARD_LIMIT", "ACME_GRACE", "ACME_MAINTENANCE", "ACME_NODE_LIMIT", "ACME_OVER_CAPACITY"}

// fakeClock is the clock pushed state follows across every boot of a site.
type fakeClock struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

func (c *fakeClock) clock() pushstate.Clock {
	return pushstate.Clock{
		Wall: func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.wall },
		Mono: func() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.mono },
	}
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.wall, c.mono = c.wall.Add(d), c.mono+d
	c.mu.Unlock()
}

// site is one installation: its data directory, database, the project
// root its build trusts and publisher acme's certificate under it, which
// names acme_fake the authority of the acme namespace and grants it the
// fleet_capacity table.
type site struct {
	t     *testing.T
	dir   string
	db    string
	root  *extsign.Key
	pub   *extsign.Publisher
	clock *fakeClock
	// modules are compiled into the server beside the plugin host.
	modules []app.Module
	// timing, when set, adjusts the supervisor's intervals of timing().
	timing func(*pluginhost.Timing)
}

func newSite(t *testing.T) *site {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { layout.RemoveAll(dir) })
	root := extsign.NewRoot(t, "1")
	pub := extsign.NewPublisher(t, "acme", func(c *extapi.PublisherCert) {
		c.Grants.AuthorityID = "acme_fake"
		c.Grants.Roles = []string{"availability_authority"}
		c.Grants.Tables = []string{extapi.KindFleetCapacity}
	}, root)
	return &site{t: t, dir: dir, db: filepath.Join(dir, "gpuworks.db"), root: root, pub: pub,
		clock: &fakeClock{wall: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)}}
}

// server is one boot of a site.
type server struct {
	t      *testing.T
	a      *app.App
	cancel context.CancelFunc
	done   chan error
	url    string
	token  string
}

// open builds the server without running it: what New does, before any
// plugin can start.
func (s *site) open() *app.App {
	s.t.Helper()
	self, err := os.Executable()
	if err != nil {
		s.t.Fatal(err)
	}
	tm := timing()
	if s.timing != nil {
		s.timing(&tm)
	}
	cfg := app.Config{Addr: "127.0.0.1:0", DataDir: s.dir, DBPath: s.db, AdminUser: "admin", AdminPassword: adminPassword,
		PluginRoots:   []extapi.PublicKey{s.root.Public},
		PluginRuntime: app.PluginRuntime{Trampoline: self, Clock: s.clock.clock(), Timing: tm, TickInterval: 20 * time.Millisecond}}
	// This test site explicitly permits its signed fixture packages when
	// Landlock is unavailable. Available Landlock remains required by their
	// certificates and the dedicated isolation tests still verify it.
	cfg.Plugins = serverconfig.Plugins{AllowUnsandboxed: []string{"acme_fake", "acme_other"}}
	cfg.Extensions.Modules = s.modules
	a, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		s.t.Fatalf("server: %v", err)
	}
	return a
}

// boot builds and runs the server and signs in as the administrator.
func (s *site) boot() *server {
	return s.run(s.open())
}

func (s *site) run(a *app.App) *server {
	s.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sv := &server{t: s.t, a: a, cancel: cancel, done: make(chan error, 1)}
	go func() { sv.done <- a.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for a.Addr() == "127.0.0.1:0" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	sv.url = "http://" + a.Addr()
	s.t.Cleanup(sv.stop)
	status, body := sv.do(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"`+adminPassword+`"}`))
	var login struct {
		Token string `json:"token"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &login) != nil || login.Token == "" {
		s.t.Fatalf("login = %d %s", status, body)
	}
	sv.token = login.Token
	return sv
}

// stop stops the server, its plugins with it, and closes its database.
func (sv *server) stop() {
	if sv.cancel == nil {
		return
	}
	sv.cancel()
	sv.cancel = nil
	select {
	case err := <-sv.done:
		if err != nil {
			sv.t.Errorf("server stopped with %v", err)
		}
	case <-time.After(90 * time.Second):
		sv.t.Error("the server did not stop")
	}
	_ = sv.a.Store().Close()
}

func (sv *server) do(method, path string, body io.Reader) (int, []byte) {
	sv.t.Helper()
	req, err := http.NewRequest(method, sv.url+path, body)
	if err != nil {
		sv.t.Fatal(err)
	}
	if sv.token != "" {
		req.Header.Set("Authorization", "Bearer "+sv.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		sv.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// install uploads a package, reads the approval hash the first answer
// names, and installs it with that approval.
func (sv *server) install(raw []byte, enable bool) dto.PluginInstalled {
	sv.t.Helper()
	status, body := sv.do(http.MethodPost, "/api/v1/plugins/install", bytes.NewReader(raw))
	var refusal struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if status != http.StatusConflict || json.Unmarshal(body, &refusal) != nil || refusal.Code != "PLUGIN_APPROVAL_REQUIRED" {
		sv.t.Fatalf("install without approval = %d %s", status, body)
	}
	hash, _ := refusal.Params["approve_hash"].(string)
	status, body = sv.do(http.MethodPost, fmt.Sprintf("/api/v1/plugins/install?approve=%s&enable=%t", hash, enable), bytes.NewReader(raw))
	var out dto.PluginInstalled
	if status != http.StatusOK || json.Unmarshal(body, &out) != nil {
		sv.t.Fatalf("install = %d %s", status, body)
	}
	return out
}

func (sv *server) plugin(id string) dto.Plugin {
	sv.t.Helper()
	status, body := sv.do(http.MethodGet, "/api/v1/plugins", nil)
	var list dto.PluginList
	if status != http.StatusOK || json.Unmarshal(body, &list) != nil {
		sv.t.Fatalf("list = %d %s", status, body)
	}
	for _, p := range list.Items {
		if p.ID == id {
			return p
		}
	}
	return dto.Plugin{ID: id, State: "absent"}
}

func (sv *server) waitPlugin(id, what string, cond func(dto.Plugin) bool) dto.Plugin {
	sv.t.Helper()
	var p dto.Plugin
	waitFor(sv.t, id+" "+what, func() bool { p = sv.plugin(id); return cond(p) })
	return p
}

func (sv *server) manifest() dto.ConsoleManifest {
	sv.t.Helper()
	status, body := sv.do(http.MethodGet, "/api/v1/console/manifest", nil)
	var m dto.ConsoleManifest
	if status != http.StatusOK || json.Unmarshal(body, &m) != nil {
		sv.t.Fatalf("manifest = %d %s", status, body)
	}
	return m
}

func feature(m dto.ConsoleManifest, key string) dto.ConsoleFeature {
	for _, f := range m.Features {
		if f.Key == key {
			return f
		}
	}
	return dto.ConsoleFeature{Key: key, Access: "undeclared"}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// fakePackage is the fake plugin as package id of acme: its server part
// requires the five pushed tables; it may push node holds and banners,
// claims the authority role, and declares acme.backup (answered by the
// authority) and acme.tools (by itself).
func (s *site) fakePackage(id, version string, edit ...func(*extsign.Spec)) []byte {
	s.t.Helper()
	return s.fakePackageWith(id, version, func(spec *extsign.Spec) {
		for _, e := range edit {
			e(spec)
		}
	})
}

// fakePackageWith is fakePackage with its spec changed by edit first.
func (s *site) fakePackageWith(id, version string, edit func(*extsign.Spec)) []byte {
	s.t.Helper()
	bin, err := os.ReadFile(fakeBin)
	if err != nil {
		s.t.Fatal(err)
	}
	requires := map[string]int{}
	for _, kind := range extapi.TableKinds() {
		requires[extapi.TableCapability(kind)] = 1
	}
	spec := extsign.Spec{
		Manifest: extapi.Manifest{
			Schema: extapi.ManifestSchema, ID: id, Publisher: "acme", Version: version,
			Created: extapi.NewTime(extsign.Epoch.Add(24 * time.Hour)), Protocol: extapi.Range{Min: 1, Max: 1},
			Runtime:     &extapi.RuntimeDecl{Lifecycle: extapi.LifecycleResident},
			Requires:    requires,
			Permissions: []string{"push:banners", "push:node_holds"},
			Roles:       []string{"availability_authority"},
			Features: []extapi.FeatureDecl{
				{Key: "acme.backup", Presentation: extapi.PresentationPromote, Gate: extapi.GateAuthority},
				{Key: "acme.tools", Presentation: extapi.PresentationPromote, Gate: extapi.GateSelf},
			},
			Codes: acmeCodes,
		},
		Files: map[string]extsign.File{
			"bin/linux_" + runtime.GOARCH + "/" + id: {Body: bin, Exec: true},
			"locales/en.json":                        {Body: []byte(`{}`)},
		},
	}
	if id != "acme_fake" {
		// Only the certificate's authority package may claim the role.
		spec.Manifest.Roles = nil
		spec.Manifest.Features = nil
	}
	if edit != nil {
		edit(&spec)
	}
	return extsign.Package(s.t, s.pub, spec)
}

// args sets how the installed fake plugin id behaves from its next start.
func (s *site) args(id string, lines ...string) {
	s.t.Helper()
	dir := layout.New(s.dir).Data(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fakeplugin.args"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

// result is one line of the fake plugin's results.jsonl.
type result struct {
	Kind       string `json:"kind"`
	Generation uint64 `json:"generation"`
	OK         bool   `json:"ok"`
	Code       string `json:"code"`
	Probe      string `json:"probe"`
	Read       string `json:"read"`
	Write      string `json:"write"`
	// Nodes, Identities and HostH are a read_nodes outcome.
	Nodes      int    `json:"nodes"`
	Identities int    `json:"identities"`
	HostH      string `json:"host_h"`
	// Route, User and Access are a served route's action, caller and
	// feature access (Kind is the caller's kind).
	Route  string `json:"route"`
	User   string `json:"user"`
	Access string `json:"access"`
}

func (s *site) results(id string) []result {
	f, err := os.Open(filepath.Join(layout.New(s.dir).Data(id), "results.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []result
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r result
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func (s *site) waitResults(id string, n int) []result {
	s.t.Helper()
	var got []result
	waitFor(s.t, fmt.Sprintf("%d results from %s", n, id), func() bool { got = s.results(id); return len(got) >= n })
	return got
}

// node enrolls a node and reports cards for it, the way its agent would.
func node(t *testing.T, st *store.Store, name string, cards int) *store.Node {
	t.Helper()
	n, _, err := st.CreateNodePending(name)
	if err != nil {
		t.Fatalf("enroll %s: %v", name, err)
	}
	if cards > 0 {
		report(t, st, n.UUID, cards)
	}
	return n
}

func report(t *testing.T, st *store.Store, uuid string, cards int) {
	t.Helper()
	ok := true
	gpus := make([]proto.GPUInfo, cards)
	for i := range gpus {
		gpus[i] = proto.GPUInfo{Index: i, UUID: fmt.Sprintf("GPU-%s-%d", uuid, i), Vendor: "nvidia",
			PCIeBDF: fmt.Sprintf("0000:%02x:00.0", 0x30+i), MemoryTotalBytes: 24 << 30, Healthy: true}
	}
	if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{GPUProbeOK: &ok, GPUs: gpus, Sys: proto.SysInfo{RuntimeDriver: "docker"}}); err != nil {
		t.Fatal(err)
	}
}

func scheduling(t *testing.T, st *store.Store, uuid string) (store.NodeScheduling, store.SchedulingSource) {
	t.Helper()
	n, err := st.NodeByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	return n.Scheduling, n.SchedulingBy
}

func heldByPlugins(t *testing.T, st *store.Store, uuid string) bool {
	s, by := scheduling(t, st, uuid)
	return s == store.NodeCordoned && by == pushstate.HoldSource
}
