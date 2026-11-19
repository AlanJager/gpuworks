//go:build linux || darwin

package pluginbridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
)

// upgradeSpawner runs each plugin over the real SDK. A build whose version
// is in failing writes "<version> was here" to its data directory in its
// initialize and then refuses it, exiting during the handshake; a package
// id in crashing fails every spawn.
type upgradeSpawner struct {
	mu       sync.Mutex
	failing  map[string]bool
	crashing map[string]bool
	spawned  []string // id@version of every spawn
}

func (s *upgradeSpawner) Spawn(_ context.Context, req pluginhost.SpawnRequest) (pluginhost.Process, error) {
	spec := req.Spec
	s.mu.Lock()
	s.spawned = append(s.spawned, spec.ID+"@"+spec.Version)
	fail, crash := s.failing[spec.Version], s.crashing[spec.ID]
	s.mu.Unlock()
	if crash {
		return nil, errors.New("exec format error")
	}
	serveCore, servePlugin, err := pipePair()
	if err != nil {
		return nil, err
	}
	hostCore, hostPlugin, err := pipePair()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &sdkProcess{serve: serveCore, host: hostCore, plugin: []net.Conn{servePlugin, hostPlugin}, cancel: cancel, done: make(chan struct{}), pid: 3000}
	cfg := pluginsdk.Config{ID: spec.ID, Version: spec.Version, ManifestSHA256: spec.ManifestSHA256, Logger: slog.New(slog.DiscardHandler),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, spec.Version) })}
	if fail {
		dataDir := spec.DataDir
		cfg.Init = func(context.Context, *pluginsdk.Plugin) error {
			_ = os.WriteFile(filepath.Join(dataDir, "state"), []byte(spec.Version+" was here"), 0o600)
			return errors.New("cannot read the store")
		}
	}
	go func() {
		err := pluginsdk.Serve(ctx, cfg, servePlugin, hostPlugin)
		code := 0
		if err != nil {
			code = 1
		}
		p.finish(pluginhost.ExitStatus{Code: code})
	}()
	return p, nil
}

func (s *upgradeSpawner) spawns() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.spawned...)
}

// openUpgrades opens and runs a bridge over sp whose upgrades must stay up
// for gate.
func (w *world) openUpgrades(sp pluginhost.Spawner, gate time.Duration) (*Bridge, func()) {
	w.t.Helper()
	pkgs, err := extpkg.NewHost(extpkg.Setup{
		DataDir: w.dir, Roots: []extapi.PublicKey{w.root.Public}, Store: w.st,
		Config: serverconfig.Plugins{AllowUnsandboxed: []string{"acme_backup", "acme_driver"}},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	b, err := Open(context.Background(), Config{Packages: pkgs, State: pushstate.Config{Store: w.st},
		Features: featuregate.NewDynamicSet(nil), Host: pluginhost.Config{Spawner: sp, Timing: callTiming()}, UpgradeGate: gate,
		Core: extapi.CoreInfo{Version: "dev", InstallID: "inst_bridge_test", OS: "linux", Arch: "amd64", HARole: extapi.HARoleSingle},
		Log:  slog.New(slog.NewTextHandler(&w.logs, nil))})
	if err != nil {
		w.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	stop := sync.OnceFunc(func() {
		cancel()
		if err := <-done; err != nil {
			w.t.Errorf("run: %v", err)
		}
	})
	w.t.Cleanup(stop)
	waitBridgeRunning(w.t, b)
	return b, stop
}

// withStorage gives a package storage at state version v.
func withStorage(v int) func(*extsign.Spec) {
	return func(s *extsign.Spec) {
		s.Manifest.Storage = &extapi.StorageDecl{Kind: extapi.StorageSQLite, HA: extapi.StorageHAShipped, StateVersion: v}
	}
}

// upgrade upgrades id to raw as the console does: the first try answers
// the approval it needs, the second carries it.
func upgrade(t *testing.T, b *Bridge, id string, raw []byte) (*Upgraded, error) {
	t.Helper()
	_, err := b.Upgrade(context.Background(), id, bytes.NewReader(raw), UpgradeRequest{By: "admin"})
	return b.Upgrade(context.Background(), id, bytes.NewReader(raw), UpgradeRequest{Approve: approvalOf(t, err), By: "admin"})
}

func writeState(t *testing.T, w *world, id, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(w.dir, "plugins", id, "data", "state"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readState(t *testing.T, w *world, id string) string {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(w.dir, "plugins", id, "data", "state"))
	return string(raw)
}

func noUpgradeMarker(t *testing.T, w *world, id string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(w.dir, "plugins", id, "upgrade.json")); !os.IsNotExist(err) {
		t.Fatalf("upgrade marker of %s: %v", id, err)
	}
}

// A new version that exits during its handshake fails the gate at once:
// it is stopped, the old version runs again with the data it had (what
// the new one wrote is gone), and the refusal names both versions and the
// cause. What the package pushed stays enforced the whole time, and its
// down record is written once, at the first stop, and not renewed by the
// restarts (A2).
func TestUpgradeThatFailsItsGateRollsBack(t *testing.T) {
	w := newWorld(t, nil)
	sp := &upgradeSpawner{failing: map[string]bool{"2.0.0": true}}
	b, _ := w.openUpgrades(sp, time.Hour)
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), false)
	pushHolds(t, b, w, "acme_backup", 1, holdOf(3))
	if _, err := b.Enable(context.Background(), "acme_backup", "admin"); err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "acme_backup", string(pluginhost.StateRunning))
	writeState(t, w, "acme_backup", "v1 rows")

	var watching atomic.Bool
	watching.Store(true)
	var lapses []string
	var downs []time.Time
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		for watching.Load() {
			held := len(b.State().Held("acme_backup")) > 0
			r, _ := b.Report("acme_backup")
			mu.Lock()
			if !held {
				lapses = append(lapses, "holds dropped")
			}
			if !r.DownSince.IsZero() && (len(downs) == 0 || !downs[len(downs)-1].Equal(r.DownSince)) {
				downs = append(downs, r.DownSince)
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	}()
	start := time.Now()
	_, err := upgrade(t, b, "acme_backup", w.pkg("acme_backup", "2.0.0", nil))
	watching.Store(false)
	<-done
	var pe *messages.ProductError
	if !errors.As(err, &pe) || pe.Code() != messages.ErrorPluginUpgradeRolledBack {
		t.Fatalf("upgrade = %v, want rolled back", err)
	}
	if p := pe.Params(); p["id"] != "acme_backup" || p["version"] != "2.0.0" || p["previous"] != "1.0.0" || p["cause_code"] == "" {
		t.Fatalf("params = %v", p)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("the gate took %v to see a crash", took)
	}
	v := waitState(t, b, "acme_backup", string(pluginhost.StateRunning))
	if v.Version != "1.0.0" {
		t.Fatalf("running %s after the rollback", v.Version)
	}
	if got := readState(t, w, "acme_backup"); got != "v1 rows" {
		t.Fatalf("data after the rollback = %q", got)
	}
	if spawns := strings.Join(sp.spawns(), ","); !strings.Contains(spawns, "acme_backup@2.0.0") || !strings.HasSuffix(spawns, "acme_backup@1.0.0") {
		t.Fatalf("spawns = %s", spawns)
	}
	noUpgradeMarker(t, w, "acme_backup")
	mu.Lock()
	defer mu.Unlock()
	if len(lapses) > 0 {
		t.Fatalf("pushed state lapsed during the upgrade: %v", lapses[0])
	}
	if len(downs) > 1 {
		t.Fatalf("the down record was renewed during the upgrade: %v", downs)
	}
	if lines := auditLines(w, "upgrade"); len(lines) != 2 || !strings.Contains(lines[1], "code=PLUGIN_UPGRADE_ROLLED_BACK") {
		t.Fatalf("audit = %v", lines)
	}
}

// A new version that stays up for the gate is the upgrade's outcome; the
// old data is kept as a snapshot, the old version for rollback, and the
// marker is gone.
func TestUpgradeThatPassesItsGate(t *testing.T) {
	w := newWorld(t, nil)
	b, _ := w.openUpgrades(&upgradeSpawner{}, 300*time.Millisecond)
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), true)
	waitState(t, b, "acme_backup", string(pluginhost.StateRunning))
	writeState(t, w, "acme_backup", "v1 rows")
	start := time.Now()
	up, err := upgrade(t, b, "acme_backup", w.pkg("acme_backup", "2.0.0", nil))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatal("the upgrade returned before its gate")
	}
	if up.View.Version != "2.0.0" || up.View.State != string(pluginhost.StateRunning) || up.Previous != "1.0.0" || up.View.Previous != "1.0.0" ||
		!strings.Contains(strings.Join(up.View.Actions, ","), ActionRollback) {
		t.Fatalf("upgraded = %+v", up)
	}
	noUpgradeMarker(t, w, "acme_backup")
	snaps, err := b.pkgs.Dir.ListSnapshots("acme_backup")
	if err != nil || len(snaps) != 1 || snaps[0].From != "1.0.0" {
		t.Fatalf("snapshots = %+v %v", snaps, err)
	}
	raw, _ := os.ReadFile(filepath.Join(b.pkgs.Dir.Snapshots("acme_backup"), snaps[0].Name, "data", "state"))
	if string(raw) != "v1 rows" {
		t.Fatalf("snapshot holds %q", raw)
	}
	if ctl, _, _ := b.pkgs.Dir.ReadControl("acme_backup"); !ctl.Enabled || ctl.ApprovedBy != "admin" {
		t.Fatalf("control = %+v", ctl)
	}
}

// An upgrade that is refused before it starts changes nothing: the running
// version keeps its process.
func TestRefusedUpgradesLeaveTheRunningVersion(t *testing.T) {
	w := newWorld(t, nil)
	sp := &upgradeSpawner{}
	b, _ := w.openUpgrades(sp, time.Hour)
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), true)
	before := waitState(t, b, "acme_backup", string(pluginhost.StateRunning))
	for _, c := range []struct {
		id   string
		raw  []byte
		code string
	}{
		{"acme_backup", w.pkg("acme_driver", "2.0.0", nil), messages.ErrorPluginManifestMismatch},
		{"acme_backup", w.pkg("acme_backup", "2.0.0", nil), messages.ErrorPluginApprovalRequired},
		{"acme_driver", w.pkg("acme_driver", "2.0.0", nil), messages.ErrorPluginNotInstalled},
	} {
		_, err := b.Upgrade(context.Background(), c.id, bytes.NewReader(c.raw), UpgradeRequest{By: "admin"})
		if code, _, _ := codeOf(err); code != c.code {
			t.Fatalf("upgrade of %s = %v, want %s", c.id, err, c.code)
		}
	}
	after := b.view("acme_backup")
	if after.Version != "1.0.0" || after.State != string(pluginhost.StateRunning) || after.Since != before.Since || len(sp.spawns()) != 1 {
		t.Fatalf("the running version changed: %+v (spawns %v)", after, sp.spawns())
	}
	noUpgradeMarker(t, w, "acme_backup")
	if _, err := b.Rollback(context.Background(), "acme_backup", RollbackRequest{By: "admin"}); !hasCode(err, messages.ErrorPluginNoPreviousVersion) {
		t.Fatalf("rollback without a previous version = %v", err)
	}
}

// A rollback to a version older than the data a newer one wrote is
// refused unless the data is restored with it; restored, the old version
// runs on the snapshot's data with the data marker put back.
func TestRollbackGuardsTheDataMarker(t *testing.T) {
	w := newWorld(t, nil)
	b, _ := w.openUpgrades(&upgradeSpawner{}, 100*time.Millisecond)
	install(t, b, w.pkg("acme_backup", "1.0.0", withStorage(1)), true)
	waitState(t, b, "acme_backup", string(pluginhost.StateRunning))
	waitMarker(t, w, "acme_backup", 1)
	writeState(t, w, "acme_backup", "v1 rows")
	if _, err := upgrade(t, b, "acme_backup", w.pkg("acme_backup", "2.0.0", withStorage(2))); err != nil {
		t.Fatal(err)
	}
	waitMarker(t, w, "acme_backup", 2)
	writeState(t, w, "acme_backup", "v2 rows")
	_, err := b.Rollback(context.Background(), "acme_backup", RollbackRequest{By: "admin"})
	if !hasCode(err, extapi.ErrorStateTooNew) {
		t.Fatalf("rollback over newer data = %v", err)
	}
	if v := b.view("acme_backup"); v.Version != "2.0.0" || v.State != string(pluginhost.StateRunning) {
		t.Fatalf("after the refused rollback: %+v", v)
	}
	v, err := b.Rollback(context.Background(), "acme_backup", RollbackRequest{RestoreState: true, By: "admin"})
	if err != nil || v.Version != "1.0.0" {
		t.Fatalf("rollback with the data = %+v %v", v, err)
	}
	waitState(t, b, "acme_backup", string(pluginhost.StateRunning))
	if got := readState(t, w, "acme_backup"); got != "v1 rows" {
		t.Fatalf("data after the restore = %q", got)
	}
	if m := marker(t, w, "acme_backup"); m.Version != 1 || m.WrittenBy != "1.0.0" {
		t.Fatalf("data marker after the restore = %+v", m)
	}
	noUpgradeMarker(t, w, "acme_backup")
}

// A server stopped after the new version became current rolls the upgrade
// back at its next boot, before it starts anything: the old version, its
// data from the snapshot, the marker gone, the decision audited.
func TestBootRollsBackAnInterruptedUpgrade(t *testing.T) {
	w := newWorld(t, nil)
	sp := &upgradeSpawner{}
	b, stop := w.openUpgrades(sp, time.Hour)
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), true)
	waitState(t, b, "acme_backup", string(pluginhost.StateRunning))
	writeState(t, w, "acme_backup", "v1 rows")
	stop()

	// What the disk holds when the server dies after activating 2.0.0.
	raw := w.pkg("acme_backup", "2.0.0", nil)
	_, err := b.pkgs.Upgrade(bytes.NewReader(raw), extpkg.UpgradeOptions{ID: "acme_backup", Marker: true, Snapshot: true})
	_, err = b.pkgs.Upgrade(bytes.NewReader(raw), extpkg.UpgradeOptions{ID: "acme_backup", Marker: true, Snapshot: true, Approve: approvalOf(t, err)})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := b.pkgs.Dir.Current("acme_backup"); v != "2.0.0" {
		t.Fatalf("current = %s", v)
	}
	writeState(t, w, "acme_backup", "v2 rows")

	b2, _ := w.openUpgrades(sp, time.Hour)
	if v, _ := b2.pkgs.Dir.Current("acme_backup"); v != "1.0.0" {
		t.Fatalf("current after the boot = %s", v)
	}
	v := waitState(t, b2, "acme_backup", string(pluginhost.StateRunning))
	if v.Version != "1.0.0" || readState(t, w, "acme_backup") != "v1 rows" {
		t.Fatalf("after the boot: %+v data %q", v, readState(t, w, "acme_backup"))
	}
	for _, s := range sp.spawns() {
		if s == "acme_backup@2.0.0" {
			t.Fatal("the interrupted version was started")
		}
	}
	noUpgradeMarker(t, w, "acme_backup")
	if !strings.Contains(w.logs.String(), "rolled back at boot") || !strings.Contains(w.logs.String(), "audit=true plugin=acme_backup from=1.0.0 to=2.0.0") {
		t.Fatalf("logs = %s", w.logs.String())
	}
}

// While package A waits out a long gate, disabling a crashlooping package
// B returns at once; disabling A itself ends A's gate after the wait,
// which rolls A back, and then disables it.
func TestGateHoldsOnlyItsOwnPackage(t *testing.T) {
	w := newWorld(t, nil)
	sp := &upgradeSpawner{crashing: map[string]bool{"acme_driver": true}}
	b, _ := w.openUpgrades(sp, time.Hour)
	b.cancelWait = 300 * time.Millisecond
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), true)
	install(t, b, w.pkg("acme_driver", "1.0.0", nil), true)
	waitState(t, b, "acme_backup", string(pluginhost.StateRunning))
	raw := w.pkg("acme_backup", "2.0.0", nil)
	_, err := b.Upgrade(context.Background(), "acme_backup", bytes.NewReader(raw), UpgradeRequest{By: "admin"})
	hash := approvalOf(t, err)
	result := make(chan error, 1)
	go func() {
		_, err := b.Upgrade(context.Background(), "acme_backup", bytes.NewReader(raw), UpgradeRequest{Approve: hash, By: "admin"})
		result <- err
	}()
	waitView(t, b, "acme_backup", func(v View) bool { return v.Version == "2.0.0" && v.State == string(pluginhost.StateRunning) })

	start := time.Now()
	if v, err := b.Disable(context.Background(), "acme_driver", "admin"); err != nil || v.State != StateDisabled {
		t.Fatalf("disable of the other package = %+v %v", v, err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("disabling another package waited %v for the gate", took)
	}
	select {
	case err := <-result:
		t.Fatalf("the upgrade ended early: %v", err)
	default:
	}

	start = time.Now()
	v, err := b.Disable(context.Background(), "acme_backup", "admin")
	if err != nil || v.State != StateDisabled || v.Version != "1.0.0" {
		t.Fatalf("disable during the gate = %+v %v", v, err)
	}
	if took := time.Since(start); took < b.cancelWait {
		t.Fatalf("the disable did not wait for the upgrade (%v)", took)
	}
	err = <-result
	var pe *messages.ProductError
	if !errors.As(err, &pe) || pe.Code() != messages.ErrorPluginUpgradeRolledBack || pe.Params()["cause_code"] != causeCancelled {
		t.Fatalf("upgrade = %v", err)
	}
	noUpgradeMarker(t, w, "acme_backup")
}

func hasCode(err error, code string) bool {
	c, _, _ := codeOf(err)
	return err != nil && c == code
}

func marker(t *testing.T, w *world, id string) extapi.Marker {
	t.Helper()
	all, err := w.st.PluginMarkers()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		if m.Name == extapi.PluginDataMarker(id) {
			return m
		}
	}
	return extapi.Marker{}
}

func waitMarker(t *testing.T, w *world, id string, version int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for marker(t, w, id).Version != version {
		if time.Now().After(deadline) {
			t.Fatalf("data marker of %s = %+v, want %d", id, marker(t, w, id), version)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
