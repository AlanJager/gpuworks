package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/store/pstate"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func (e *pluginEnv) install(version string, extra ...string) {
	e.t.Helper()
	path := e.writePackage("p-"+version+".gwpkg", extsign.Package(e.t, e.pub, extsign.Minimal("acme", "acme_backup", version)))
	if _, err := e.run(append([]string{"install", "--file", path}, extra...)...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *pluginEnv) withStore(f func(*store.Store)) {
	e.t.Helper()
	st, err := store.Open(filepath.Join(e.data, "gpuworks.db"), bus.New())
	if err != nil {
		e.t.Fatal(err)
	}
	defer st.Close()
	f(st)
}

// push writes what owner pushed as the server's writer would: a node hold
// table, a down record and an administrator's override.
func (e *pluginEnv) push(owner string) {
	e.t.Helper()
	e.withStore(func(st *store.Store) {
		_, err := st.WritePushedState(context.Background(), &pstate.Batch{
			Tables: []pstate.TableWrite{{
				Record:    pstate.Owner{Owner: owner, Kind: extapi.KindNodeHolds, Schema: 1, Generation: 1, Digest: "sha256:x", PushedAtMS: 1_700_000_000_000},
				DeleteAll: true,
				Insert:    pstate.Rows{NodeHolds: []pstate.NodeHold{{Owner: owner, NodeID: 7, Code: "ACME_NODE_HELD"}}},
			}},
			Down:      []pstate.Down{{Owner: owner, SinceMS: 1_700_000_000_000}},
			Overrides: []pstate.Override{{Owner: owner, Kind: extapi.KindNodeHolds, EntryKey: "7", Generation: 1, Action: pstate.OverrideDelete, ByUser: 1, AtMS: 1_700_000_000_000, Reason: "maintenance"}},
		})
		if err != nil {
			e.t.Fatal(err)
		}
	})
}

// owned is what owner has in the database now.
func (e *pluginEnv) owned(owner string) ownedState {
	e.t.Helper()
	var own ownedState
	e.withStore(func(st *store.Store) {
		snap, err := st.LoadPushedState(context.Background())
		if err != nil {
			e.t.Fatal(err)
		}
		own = ownerState(snap, owner)
	})
	return own
}

func (e *pluginEnv) enabled() string {
	e.t.Helper()
	out, err := e.run("list")
	if err != nil {
		e.t.Fatal(err)
	}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) >= 5 && f[0] == "acme_backup" {
			return f[4]
		}
	}
	e.t.Fatalf("acme_backup not listed:\n%s", out)
	return ""
}

func TestPluginLifecycleRefusesWhileTheServerRuns(t *testing.T) {
	e := newPluginEnv(t, testPluginSite)
	e.install("1.0.0", "--disabled")
	e.push("acme_backup")
	release, err := takeServerLock(e.data, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	want := fmt.Sprintf("the server (pid %d) is running; stop it first", os.Getpid())
	for _, args := range [][]string{{"uninstall", "acme_backup"}, {"remove", "acme_backup", "--purge"}, {"enable", "acme_backup"}, {"disable", "acme_backup"}} {
		_, err := e.run(args...)
		var running *serverRunningError
		if !errors.As(err, &running) || err.Error() != want {
			t.Fatalf("%v with the server running = %v", args, err)
		}
	}
	// Nothing changed: the package, its switch and its rows are as they were.
	if got := e.enabled(); got != "false" {
		t.Fatalf("enabled = %s after a refused enable", got)
	}
	if own := e.owned("acme_backup"); len(own.Owners) != 1 || len(own.Down) != 1 || len(own.Overrides) != 1 {
		t.Fatalf("rows after refused uninstall = %+v", own)
	}
	// The read-only commands still work, and state says it may lag.
	if out, err := e.run("verify", "acme_backup"); err != nil || !strings.Contains(out, "acme_backup 1.0.0 verifies (publisher acme, project trust)") {
		t.Fatalf("verify = %q %v", out, err)
	}
	out, err := e.run("state", "acme_backup")
	if err != nil || !strings.Contains(out, fmt.Sprintf("the server (pid %d) is running", os.Getpid())) || !strings.Contains(out, "node_holds: node 7 held (ACME_NODE_HELD)") {
		t.Fatalf("state = %q %v", out, err)
	}
	// The command line exits 2 with the refusal on stderr.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, args := range [][]string{{"uninstall", "acme_backup"}, {"remove", "acme_backup", "--purge"}, {"enable", "acme_backup"}, {"disable", "acme_backup"}} {
		var stderr bytes.Buffer
		argv := append(append([]string{"plugin"}, args...), "--data", e.data, "--config", e.config)
		if code := Run(log, argv, &stderr, e.opt); code != 2 || strings.TrimSpace(stderr.String()) != want {
			t.Fatalf("%v: exit %d, stderr %q", args, code, stderr.String())
		}
	}
}

func TestPluginUninstallReleasesWhatItPushedAndAReinstallStartsClean(t *testing.T) {
	e := newPluginEnv(t, testPluginSite)
	e.install("1.0.0")
	e.push("acme_backup")
	e.push("acme_other") // another owner's rows stay
	out, err := e.run("state", "acme_backup")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"node_holds  1       1", "node_holds: node 7 held (ACME_NODE_HELD)", "down since 2023-11-14T22:13:20Z", "override 1: delete node_holds 7 value - by user 1"} {
		if !strings.Contains(out, line) {
			t.Fatalf("state lacks %q:\n%s", line, out)
		}
	}
	if strings.Contains(out, "is running") {
		t.Fatalf("state reports a server that is not running:\n%s", out)
	}
	out, err = e.run("uninstall", "acme_backup")
	if err != nil || !strings.Contains(out, "uninstalled acme_backup") || !strings.Contains(out, "1 tables, 1 node holds, 1 overrides") {
		t.Fatalf("uninstall = %q %v", out, err)
	}
	if own := e.owned("acme_backup"); !own.empty() {
		t.Fatalf("rows left after uninstall: %+v", own)
	}
	if own := e.owned("acme_other"); len(own.Owners) != 1 || len(own.Overrides) != 1 {
		t.Fatalf("another owner's rows = %+v", own)
	}
	e.install("1.0.0")
	if out, err := e.run("state", "acme_backup"); err != nil || strings.TrimSpace(out) != "acme_backup has no pushed state" {
		t.Fatalf("state after reinstall = %q %v", out, err)
	}
	// remove is the earlier name and does the same.
	e.push("acme_backup")
	if out, err := e.run("remove", "acme_backup", "--purge"); err != nil || !strings.Contains(out, "uninstalled acme_backup") {
		t.Fatalf("remove = %q %v", out, err)
	}
	if own := e.owned("acme_backup"); !own.empty() {
		t.Fatalf("rows left after remove: %+v", own)
	}
}

func TestPluginUninstallFinishesRowsWhosePackageIsGone(t *testing.T) {
	e := newPluginEnv(t, testPluginSite)
	// An uninstall that removed the package and failed before its rows.
	e.push("acme_backup")
	out, err := e.run("uninstall", "acme_backup")
	if err != nil || strings.Contains(out, "uninstalled") || !strings.Contains(out, "released what acme_backup pushed: 1 tables") {
		t.Fatalf("retry = %q %v", out, err)
	}
	if own := e.owned("acme_backup"); !own.empty() {
		t.Fatalf("rows left: %+v", own)
	}
	if _, err := e.run("uninstall", "acme_backup"); productCode(err) != messages.ErrorPluginNotInstalled {
		t.Fatalf("nothing left to uninstall = %v", err)
	}
	if _, err := e.run("uninstall"); err == nil {
		t.Fatal("uninstall without an id succeeded")
	}
}

func TestPluginEnableVerifiesAndDisableSwitchesOff(t *testing.T) {
	e := newPluginEnv(t, testPluginSite)
	e.install("1.0.0")
	if out, err := e.run("disable", "acme_backup"); err != nil || out != "disabled acme_backup\n" || e.enabled() != "false" {
		t.Fatalf("disable = %q %v", out, err)
	}
	if out, err := e.run("enable", "acme_backup"); err != nil || out != "enabled acme_backup\n" || e.enabled() != "true" {
		t.Fatalf("enable = %q %v", out, err)
	}
	for _, sub := range []string{"enable", "disable", "verify"} {
		if _, err := e.run(sub, "acme_nothing"); productCode(err) != messages.ErrorPluginNotInstalled {
			t.Fatalf("%s of an unknown id = %v", sub, err)
		}
	}
	if _, err := e.run("disable", "acme_backup"); err != nil {
		t.Fatal(err)
	}
	locale := filepath.Join(e.data, "plugins", "acme_backup", "pkg", "1.0.0", "locales", "en.json")
	os.Chmod(filepath.Dir(locale), 0o755)
	os.Chmod(locale, 0o644)
	if err := os.WriteFile(locale, []byte(`{"x":1}`), 0o444); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"enable", "verify"} {
		_, err := e.run(sub, "acme_backup")
		if productCode(err) != messages.ErrorPluginBroken || !strings.Contains(err.Error(), "plugin acme_backup no longer verifies") ||
			!strings.Contains(err.Error(), messages.ErrorPluginFileMismatch) {
			t.Fatalf("%s of a tampered package = %v", sub, err)
		}
	}
	if got := e.enabled(); got != "false" {
		t.Fatalf("a broken package was enabled: %s", got)
	}
}

// A build whose certificate a trust import revoked stays installed but no
// longer enables, and the refusal says why once.
func TestPluginEnableRefusesARevokedBuild(t *testing.T) {
	e := newPluginEnv(t, testPluginSite)
	e.install("1.0.0", "--disabled")
	crl := e.writePackage("crl.json", extsign.CRL(t, 1, "acme", []extapi.Revocation{{Publisher: "acme", CertSerial: 1}}, e.pub.Key))
	cert := e.writePackage("publisher.crt", e.pub.CertBundle(t))
	if _, err := e.run("trust", "import", "--cert", cert, crl); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"enable", "verify"} {
		_, err := e.run(sub, "acme_backup")
		if productCode(err) != messages.ErrorPluginBroken || !strings.Contains(err.Error(), "plugin acme_backup no longer verifies: its signature is revoked") ||
			strings.Count(err.Error(), "its signature is revoked") != 1 {
			t.Fatalf("%s of a revoked build = %v", sub, err)
		}
	}
	if got := e.enabled(); got != "false" {
		t.Fatalf("a revoked build was enabled: %s", got)
	}
}

// state prints notice parameters too: an owner whose only rows are
// parameters does not print a bare table header.
func TestPluginStatePrintsNoticeParams(t *testing.T) {
	name, n := "acme", int64(3)
	var out bytes.Buffer
	own := ownedState{Rows: pstate.Rows{Params: []pstate.NoticeParam{
		{Owner: "acme_backup", Kind: extapi.KindNodeHolds, Entry: "7", Slot: "notice", Key: "name", Str: &name},
		{Owner: "acme_backup", Kind: extapi.KindNodeHolds, Entry: "7", Slot: "notice", Key: "count", Int: &n},
	}}}
	if own.empty() {
		t.Fatal("parameters count as no state")
	}
	if err := printOwnedState(&out, own); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{`notice param node_holds 7 notice name: "acme"`, "notice param node_holds 7 notice count: 3"} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("state lacks %q:\n%s", line, out.String())
		}
	}
}
