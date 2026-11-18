package pluginbridge

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// world is a data directory, a database and the signing material of one
// test: a project root and publisher acme certified by it.
type world struct {
	t    *testing.T
	dir  string
	st   *store.Store
	root *extsign.Key
	pub  *extsign.Publisher
}

func newWorld(t *testing.T, edit func(*extapi.PublisherCert)) *world {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { layout.RemoveAll(dir) })
	st, err := store.Open(filepath.Join(dir, "gpuworks.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	root := extsign.NewRoot(t, "1")
	return &world{t: t, dir: dir, st: st, root: root, pub: extsign.NewPublisher(t, "acme", edit, root)}
}

// refusingSpawner records what it is asked to start and starts nothing.
type refusingSpawner struct {
	mu    sync.Mutex
	specs []pluginhost.Spec
}

func (r *refusingSpawner) Spawn(_ context.Context, req pluginhost.SpawnRequest) (pluginhost.Process, error) {
	r.mu.Lock()
	r.specs = append(r.specs, *req.Spec)
	r.mu.Unlock()
	return nil, errors.New("this test starts no process")
}

func (r *refusingSpawner) started() []pluginhost.Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.specs)
}

// open opens a bridge over the world and runs it until the test ends; it
// spawns nothing.
func (w *world) open() *Bridge {
	b, _ := w.openRunning()
	return b
}

func (w *world) openRunning() (*Bridge, *refusingSpawner) {
	w.t.Helper()
	// The refusing spawner starts no process. Permit only these fixture
	// packages when Landlock is unavailable, retaining their signed grants.
	pkgs, err := extpkg.NewHost(extpkg.Setup{
		DataDir: w.dir, Roots: []extapi.PublicKey{w.root.Public}, Store: w.st,
		Config: serverconfig.Plugins{AllowUnsandboxed: []string{"acme_backup", "acme_off", "acme_driver"}},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	sp := &refusingSpawner{}
	b, err := Open(context.Background(), Config{Packages: pkgs, State: pushstate.Config{Store: w.st},
		Host: pluginhost.Config{Spawner: sp, Timing: pluginhost.Timing{BackoffMin: time.Hour}},
		Core: extapi.CoreInfo{Version: "dev", InstallID: "inst_bridge_test", OS: "linux", Arch: "amd64", HARole: extapi.HARoleSingle},
		Log:  slog.New(slog.DiscardHandler)})
	if err != nil {
		w.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	w.t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			w.t.Errorf("run: %v", err)
		}
	})
	return b, sp
}

func (w *world) pkg(id, version string, edit func(*extsign.Spec)) []byte {
	s := extsign.Minimal("acme", id, version)
	if edit != nil {
		edit(&s)
	}
	return extsign.Package(w.t, w.pub, s)
}

func approvalOf(t *testing.T, err error) extapi.Digest {
	t.Helper()
	var pe *messages.ProductError
	if !errors.As(err, &pe) || pe.Code() != messages.ErrorPluginApprovalRequired {
		t.Fatalf("install without approval = %v", err)
	}
	return extapi.Digest(pe.Params()["approve_hash"].(string))
}

func install(t *testing.T, b *Bridge, raw []byte, enable bool) *Installed {
	t.Helper()
	_, err := b.Install(context.Background(), bytes.NewReader(raw), InstallRequest{By: "admin"})
	in, err := b.Install(context.Background(), bytes.NewReader(raw), InstallRequest{Approve: approvalOf(t, err), Enable: enable, By: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// With nothing installed the bridge lists nothing, declares no feature
// and its hooks allow everything.
func TestOpenWithNothingInstalled(t *testing.T) {
	b := newWorld(t, nil).open()
	if len(b.List()) != 0 || len(b.Features()) != 0 {
		t.Fatalf("list %v features %v", b.List(), b.Features())
	}
	s := b.State()
	if s.GuardNode(1000, 8000) != nil || s.Hold(1, 8).Code != "" || len(s.Banners(true)) != 0 {
		t.Fatal("a hook refuses with nothing installed")
	}
}

// An install is approved by hash, recorded disabled unless asked, listed
// with the actions it allows, and enabling and disabling flip it. The
// feature keys it declares are declared by the next bridge.
func TestInstallEnableDisable(t *testing.T) {
	w := newWorld(t, nil)
	b := w.open()
	raw := w.pkg("acme_backup", "1.0.0", func(s *extsign.Spec) {
		s.Manifest.Permissions = []string{"push:banners"}
		s.Manifest.Uses = map[string]int{"table.banners": 1}
		s.Manifest.Features = []extapi.FeatureDecl{{Key: "acme.backup", Presentation: extapi.PresentationPromote, Gate: extapi.GateSelf}}
	})
	in := install(t, b, raw, false)
	v := in.View
	if v.State != StateDisabled || v.Enabled || v.Trust != extpkg.TrustProject || !slices.Equal(v.Actions, []string{ActionEnable, ActionUninstall}) ||
		!slices.Equal(v.Permissions, []string{"push:banners"}) || !slices.Equal(v.Parts, []string{"server"}) || v.ApproveHash == "" {
		t.Fatalf("installed = %+v", v)
	}
	if len(b.Features()) != 0 {
		t.Fatalf("features declared before the next start: %v", b.Features())
	}
	v, err := b.Enable(context.Background(), "acme_backup", "admin")
	if err != nil || !v.Enabled || !slices.Equal(v.Actions, []string{ActionDisable, ActionUninstall}) {
		t.Fatalf("enabled = %+v, %v", v, err)
	}
	waitView(t, b, "acme_backup", func(v View) bool {
		return v.State == string(pluginhost.StateBackoff) && v.Code == messages.ErrorPluginStartFailed
	})
	v, err = b.Disable(context.Background(), "acme_backup", "admin")
	if err != nil || v.Enabled || v.State != StateDisabled {
		t.Fatalf("disabled = %+v, %v", v, err)
	}
	if _, err := b.Enable(context.Background(), "acme_nothing", "admin"); !isCode(err, messages.ErrorPluginNotInstalled) {
		t.Fatalf("enable of nothing = %v", err)
	}
	if got := w.open().Features(); len(got) != 1 || got[0].Key != "acme.backup" {
		t.Fatalf("features after a reopen = %v", got)
	}
}

// A package whose files changed after install no longer verifies: it is
// listed broken with the refusal, may only be uninstalled, cannot be
// enabled, and pushed state keeps it installed but not verified.
func TestABrokenPackageIsListedAndNeverEnabled(t *testing.T) {
	w := newWorld(t, nil)
	b := w.open()
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), true)
	bin := filepath.Join(b.pkgs.Dir.Version("acme_backup", "1.0.0"), "bin", "linux_amd64", "acme_backup")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	b = w.open()
	v := b.List()[0]
	if v.State != StateBroken || v.Code != messages.ErrorPluginFileMismatch || !slices.Equal(v.Actions, []string{ActionUninstall}) {
		t.Fatalf("broken = %+v", v)
	}
	if _, err := b.Enable(context.Background(), "acme_backup", "admin"); !isCode(err, messages.ErrorPluginBroken) {
		t.Fatalf("enable of a broken package = %v", err)
	}
	if err := b.Uninstall(context.Background(), "acme_backup", false, "admin"); err != nil {
		t.Fatal(err)
	}
	if len(b.List()) != 0 {
		t.Fatalf("after uninstall: %+v", b.List())
	}
	if err := b.Uninstall(context.Background(), "acme_backup", false, "admin"); !isCode(err, messages.ErrorPluginNotInstalled) {
		t.Fatalf("second uninstall = %v", err)
	}
}

// The host connection's pushes are applied as the package started on it:
// an unknown or replaced package, another version and a patch are refused;
// the identity carries the certificate's grants, the approved permissions,
// the codes and the connection's generation, and a refusal keeps its
// gwx/1 status.
func TestStateHostPushesAsTheStartedPackage(t *testing.T) {
	w := newWorld(t, nil)
	b := w.open()
	h := stateHost{b}
	push := &extapi.StatePush{Kind: extapi.KindBanners, Schema: 1, Generation: 1, Body: []byte(`{"banners":[]}`)}
	owner := pluginhost.Owner{ID: "acme_backup", Version: "1.0.0", Lifecycle: extapi.LifecycleResident, Generation: 4}
	refusal := func(err error) *pluginhost.Error {
		var e *pluginhost.Error
		if !errors.As(err, &e) {
			t.Fatalf("refusal = %v", err)
		}
		return e
	}
	if _, err := h.Push(context.Background(), owner, http.MethodPut, push); refusal(err).Wire.Code != messages.ErrorPluginStateConnectionReplaced {
		t.Fatalf("unknown owner = %v", err)
	}
	b.pushers["acme_backup"] = identity{version: "1.0.0", pusher: pushstate.Pusher{ID: "acme_backup", Publisher: "acme", Grants: w.pub.Cert.Grants}}
	if _, err := h.Push(context.Background(), pluginhost.Owner{ID: "acme_backup", Version: "0.9.0"}, http.MethodPut, push); refusal(err).Wire.Code != messages.ErrorPluginStateConnectionReplaced {
		t.Fatalf("other version = %v", err)
	}
	if e := refusal2(h.Push(context.Background(), owner, http.MethodPatch, push)); e.Status != http.StatusNotImplemented && e.Wire.Reason != extapi.ReasonUnsupported {
		t.Fatalf("patch = %+v", e)
	}
	// No push:banners permission was approved.
	e := refusal2(h.Push(context.Background(), owner, http.MethodPut, push))
	if e.Wire.Code != messages.ErrorPluginStateNotPermitted || e.Status != extapi.StatusForReason(extapi.ReasonPermissionDenied) {
		t.Fatalf("not permitted = %+v", e)
	}
}

func refusal2(_ *extapi.StateAck, err error) *pluginhost.Error {
	var e *pluginhost.Error
	if !errors.As(err, &e) {
		panic("not a gwx refusal: " + errString(err))
	}
	return e
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// specOf runs the package's verified binary from its version directory
// with private data and tmp directories, the grant the administrator
// approved, the capabilities it may use, and Landlock required when its
// certificate asks for it on a host that has it.
func TestSpecOfAVerifiedPackage(t *testing.T) {
	w := newWorld(t, func(c *extapi.PublisherCert) { c.Grants.Roles = []string{"clock_authority"} })
	b := w.open()
	install(t, b, w.pkg("acme_backup", "1.2.0", func(s *extsign.Spec) {
		s.Manifest.Permissions = []string{"push:banners", "push:node_holds"}
		s.Manifest.Roles = []string{"clock_authority"}
		s.Manifest.Requires = map[string]int{"table.banners": 1}
		s.Manifest.Uses = map[string]int{"table.node_holds": 3, "host.unknown": 1}
		s.Manifest.Runtime.MemMiB, s.Manifest.Runtime.MaxConcurrency = 256, 4
	}), false)
	v, err := b.pkgs.Recheck("acme_backup", true)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := b.specOf(v)
	if err != nil {
		t.Fatal(err)
	}
	dir := b.pkgs.Dir.Version("acme_backup", "1.2.0")
	if spec.Exe != filepath.Join(dir, "bin", extpkg.HostPlatform().OS+"_"+extpkg.HostPlatform().Arch, "acme_backup") || spec.PackageDir != dir ||
		spec.ManifestSHA256 != v.Digest || spec.Lifecycle != extapi.LifecycleResident || spec.MemMiB != 256 || spec.MaxConcurrency != 4 {
		t.Fatalf("spec = %+v", spec)
	}
	for _, d := range []string{spec.DataDir, spec.TmpDir} {
		if st, err := os.Stat(d); err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", d, st, err)
		}
	}
	if want := map[string]int{"table.banners": 1, "table.node_holds": 1}; len(spec.Approved) != 2 || spec.Approved["table.banners"] != 1 || spec.Approved["table.node_holds"] != 1 {
		t.Fatalf("approved = %v, want %v", spec.Approved, want)
	}
	g := spec.Grant
	if g.ID != "acme_backup" || g.Version != "1.2.0" || !slices.Equal(g.PermissionsGranted, []string{"push:banners", "push:node_holds"}) ||
		!slices.Equal(g.RolesGranted, []string{"clock_authority"}) || len(g.AuthorityNamespaces) != 0 {
		t.Fatalf("grant = %+v", g)
	}
	if spec.RequireLandlock != (extpkg.HostIsolation() == extapi.IsolationLandlock) {
		t.Fatalf("require landlock = %v on a host with %s", spec.RequireLandlock, extpkg.HostIsolation())
	}
}

func TestCoreVersionOfADevelopmentBuild(t *testing.T) {
	for v, want := range map[string]bool{"v26.12.0": true, "26.12.0-rc.1": true, "dev": false, "": false, "v1": false} {
		if coreVersionPattern.MatchString(v) != want {
			t.Fatalf("%q: %v", v, !want)
		}
	}
}

func waitView(t *testing.T, b *Bridge, id string, cond func(View) bool) View {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v := b.view(id)
		if cond(v) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached the state; last %+v", id, v)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Run starts every enabled resident package with its spec, and nothing
// else: not a disabled one, not one without a server part, not an
// on-demand one.
func TestRunStartsTheEnabledResidentPackages(t *testing.T) {
	w := newWorld(t, nil)
	b := w.open()
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), true)
	install(t, b, w.pkg("acme_off", "1.0.0", nil), false)
	install(t, b, w.pkg("acme_driver", "1.0.0", func(s *extsign.Spec) { s.Manifest.Runtime.Lifecycle = extapi.LifecycleOnDemand }), true)
	install(t, b, w.pkg("acme_ui", "1.0.0", func(s *extsign.Spec) {
		s.Manifest.Runtime = nil
		s.Files = map[string]extsign.File{"locales/en.json": {Body: []byte(`{}`)}}
	}), true)
	b2, sp := w.openRunning()
	deadline := time.Now().Add(10 * time.Second)
	for len(sp.started()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	started := sp.started()
	if len(started) != 1 || started[0].ID != "acme_backup" || started[0].Lifecycle != extapi.LifecycleResident {
		t.Fatalf("started %+v", started)
	}
	states := map[string]string{}
	for _, v := range b2.List() {
		states[v.ID] = v.State
	}
	if states["acme_off"] != StateDisabled || states["acme_driver"] != StateIdle || states["acme_ui"] != StateInstalled {
		t.Fatalf("states = %v", states)
	}
}

func isCode(err error, code string) bool {
	var pe *messages.ProductError
	return errors.As(err, &pe) && pe.Code() == code
}
