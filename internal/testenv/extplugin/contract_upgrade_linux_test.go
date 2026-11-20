//go:build linux

package extplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// refusal is a product error's wire body.
type refusal struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params"`
}

// upgrade uploads raw as the upgrade of id, reads the approval hash the
// first answer names, and upgrades with it; it returns the second answer.
func (sv *server) upgrade(id string, raw []byte) (int, []byte) {
	sv.t.Helper()
	path := "/api/v1/plugins/" + id + "/upgrade"
	status, body := sv.do(http.MethodPost, path, bytes.NewReader(raw))
	var r refusal
	if status != http.StatusConflict || json.Unmarshal(body, &r) != nil || r.Code != messages.ErrorPluginApprovalRequired || r.Params["from_version"] == "" {
		sv.t.Fatalf("upgrade of %s without approval = %d %s", id, status, body)
	}
	return sv.do(http.MethodPost, path+"?approve="+r.Params["approve_hash"].(string), bytes.NewReader(raw))
}

// stateFile is the fake plugin's file "state" in the data directory of id.
func (s *site) stateFile(id string) string { return filepath.Join(layout.New(s.dir).Data(id), "state") }

// A new version that exits during its handshake is rolled back through
// the API: the answer is PLUGIN_UPGRADE_ROLLED_BACK, the old version runs
// again on the data it had (what the new one wrote is gone), and the node
// it holds stays held throughout.
func TestContractUpgradeRollsBackAVersionThatCannotStart(t *testing.T) {
	s := newSite(t)
	sv := s.boot()
	st := sv.a.Store()
	h := node(t, st, "h", 2)
	storage := func(v int) func(*extsign.Spec) {
		return func(spec *extsign.Spec) {
			spec.Manifest.Storage = &extapi.StorageDecl{Kind: extapi.StorageSQLite, HA: extapi.StorageHAShipped, StateVersion: v}
		}
	}
	s.args("acme_fake", "--requires", requiresAll(), "--push", "node_holds="+holdsBody(h.ID), "--fail-version", "2.0.0")
	sv.install(s.fakePackage("acme_fake", "1.0.0", storage(1)), true)
	if r := s.waitResults("acme_fake", 1)[0]; !r.OK {
		t.Fatalf("push = %+v", r)
	}
	sv.waitPlugin("acme_fake", "running", func(p dto.Plugin) bool { return p.State == "running" })
	waitFor(t, "node h held", func() bool { return heldByPlugins(t, st, h.UUID) })
	if err := os.WriteFile(s.stateFile("acme_fake"), []byte("v1 rows"), 0o600); err != nil {
		t.Fatal(err)
	}

	var released atomic.Bool
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// The polling goroutine reads the node itself: t.Fatal is for
			// the test's own goroutine.
			if n, err := st.NodeByUUID(h.UUID); err == nil && (n.Scheduling != store.NodeCordoned || n.SchedulingBy != pushstate.HoldSource) {
				released.Store(true)
			}
		}
	}()
	status, body := sv.upgrade("acme_fake", s.fakePackage("acme_fake", "2.0.0", storage(2)))
	close(stop)
	wg.Wait()
	var r refusal
	if status != http.StatusUnprocessableEntity || json.Unmarshal(body, &r) != nil || r.Code != messages.ErrorPluginUpgradeRolledBack ||
		r.Params["version"] != "2.0.0" || r.Params["previous"] != "1.0.0" || r.Params["cause_code"] == "" {
		t.Fatalf("failed upgrade = %d %s", status, body)
	}
	if released.Load() {
		t.Fatal("the plugin's hold lapsed during the upgrade")
	}
	p := sv.waitPlugin("acme_fake", "running 1.0.0 again", func(p dto.Plugin) bool { return p.State == "running" && p.Version == "1.0.0" })
	if p.Previous != "" {
		t.Fatalf("after the rollback previous = %q", p.Previous)
	}
	if got, _ := os.ReadFile(s.stateFile("acme_fake")); string(got) != "v1 rows" {
		t.Fatalf("data after the rollback = %q", got)
	}
	if _, err := os.Stat(filepath.Join(layout.New(s.dir).Package("acme_fake"), "upgrade.json")); !os.IsNotExist(err) {
		t.Fatalf("upgrade marker after the rollback: %v", err)
	}
	if !heldByPlugins(t, st, h.UUID) {
		t.Fatal("the hold was released")
	}
}

// A server stopped after an upgrade made the new version current rolls the
// upgrade back at its next boot: the old version is current and runs with
// its data from the snapshot, and the marker is gone.
func TestContractBootRollsBackAnInterruptedUpgrade(t *testing.T) {
	s := newSite(t)
	sv := s.boot()
	sv.install(s.fakePackage("acme_fake", "1.0.0"), true)
	sv.waitPlugin("acme_fake", "running", func(p dto.Plugin) bool { return p.State == "running" })
	if err := os.WriteFile(s.stateFile("acme_fake"), []byte("v1 rows"), 0o600); err != nil {
		t.Fatal(err)
	}
	sv.stop()

	// The disk as a server leaves it when it dies after making 2.0.0
	// current: the marker, the snapshot, the new version current, and
	// data the new version changed.
	st, err := store.OpenRuntime(context.Background(), s.db, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := extpkg.NewHost(extpkg.Setup{DataDir: s.dir, Roots: []extapi.PublicKey{s.root.Public}, Store: st,
		Config: serverconfig.Plugins{AllowUnsandboxed: []string{"acme_fake"}}})
	if err != nil {
		t.Fatal(err)
	}
	raw := s.fakePackage("acme_fake", "2.0.0")
	opt := extpkg.UpgradeOptions{ID: "acme_fake", Marker: true, Snapshot: true}
	_, err = pkgs.Upgrade(bytes.NewReader(raw), opt)
	var pe *messages.ProductError
	if !errors.As(err, &pe) || pe.Code() != messages.ErrorPluginApprovalRequired {
		t.Fatalf("upgrade without approval = %v", err)
	}
	opt.Approve = extapi.Digest(pe.Params()["approve_hash"].(string))
	if _, err := pkgs.Upgrade(bytes.NewReader(raw), opt); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if err := os.WriteFile(s.stateFile("acme_fake"), []byte("v2 rows"), 0o600); err != nil {
		t.Fatal(err)
	}

	sv = s.boot()
	p := sv.waitPlugin("acme_fake", "running 1.0.0", func(p dto.Plugin) bool { return p.State == "running" })
	if p.Version != "1.0.0" {
		t.Fatalf("after the boot %s runs", p.Version)
	}
	if got, _ := os.ReadFile(s.stateFile("acme_fake")); string(got) != "v1 rows" {
		t.Fatalf("data after the boot = %q", got)
	}
	if _, err := os.Stat(filepath.Join(layout.New(s.dir).Package("acme_fake"), "upgrade.json")); !os.IsNotExist(err) {
		t.Fatalf("upgrade marker after the boot: %v", err)
	}
	if status, body := sv.do(http.MethodPost, "/api/v1/plugins/acme_fake/rollback", nil); status != http.StatusConflict ||
		!bytes.Contains(body, []byte(messages.ErrorPluginNoPreviousVersion)) {
		t.Fatalf("rollback with nothing older = %d %s", status, body)
	}
}
