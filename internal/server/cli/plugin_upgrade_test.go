package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
)

// Without --approve an upgrade prints its diff, exits non-zero and changes
// nothing; with it the new version is current and the old data is kept as
// a snapshot; rollback --restore-state goes back to the old version with
// that data, and a second rollback has nothing to go back to.
func TestPluginUpgradeAndRollbackWithTheServerStopped(t *testing.T) {
	e := newPluginEnv(t, testPluginSite)
	e.install("1.0.0")
	dir := layout.New(e.data)
	state := filepath.Join(dir.Data("acme_backup"), "state")
	if err := os.MkdirAll(filepath.Dir(state), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte("v1 rows"), 0o600); err != nil {
		t.Fatal(err)
	}
	v2 := e.writePackage("v2.gwpkg", extsign.Package(t, e.pub, extsign.Minimal("acme", "acme_backup", "2.0.0")))

	out, err := e.run("upgrade", "--file", v2)
	if err == nil || !strings.Contains(err.Error(), "nothing was changed; run plugin upgrade --file "+v2+" --approve") {
		t.Fatalf("upgrade without --approve = %v", err)
	}
	if !strings.Contains(out, "upgrading acme_backup from 1.0.0 to 2.0.0 asks for:\n  nothing it did not ask for before") {
		t.Fatalf("diff = %q", out)
	}
	if v, _ := dir.Current("acme_backup"); v != "1.0.0" {
		t.Fatalf("current after a refused upgrade = %s", v)
	}
	if list, _ := dir.ListSnapshots("acme_backup"); len(list) != 0 {
		t.Fatalf("a refused upgrade took a snapshot: %v", list)
	}

	out, err = e.run("upgrade", "--file", v2, "--approve")
	if err != nil || !strings.Contains(out, "upgraded acme_backup from 1.0.0 to 2.0.0, approved sha256:") ||
		!strings.Contains(out, "plugin rollback acme_backup --restore-state") {
		t.Fatalf("approved upgrade = %q %v", out, err)
	}
	if v, _ := dir.Current("acme_backup"); v != "2.0.0" {
		t.Fatalf("current = %s", v)
	}
	if got := e.enabled(); got != "true" {
		t.Fatalf("the upgrade changed enabled to %s", got)
	}
	if _, ok, _ := dir.ReadUpgrade("acme_backup"); ok {
		t.Fatal("the command line left an upgrade marker")
	}
	if err := os.WriteFile(state, []byte("v2 rows"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err = e.run("rollback", "acme_backup", "--restore-state")
	if err != nil || !strings.Contains(out, "rolled acme_backup back from 2.0.0 to 1.0.0\ndata restored from snapshot ") {
		t.Fatalf("rollback = %q %v", out, err)
	}
	if v, _ := dir.Current("acme_backup"); v != "1.0.0" {
		t.Fatalf("current after the rollback = %s", v)
	}
	if raw, _ := os.ReadFile(state); string(raw) != "v1 rows" {
		t.Fatalf("data after the rollback = %q", raw)
	}
	if _, err := e.run("rollback", "acme_backup"); productCode(err) != messages.ErrorPluginNoPreviousVersion {
		t.Fatalf("rollback with nothing older = %v", err)
	}
}

// With the server running both commands refuse, name the route that does
// the same, and change nothing.
func TestPluginUpgradeAndRollbackRefuseWhileTheServerRuns(t *testing.T) {
	e := newPluginEnv(t, testPluginSite)
	e.install("1.0.0")
	v2 := e.writePackage("v2.gwpkg", extsign.Package(t, e.pub, extsign.Minimal("acme", "acme_backup", "2.0.0")))
	release, err := takeServerLock(e.data, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for args, route := range map[string]string{"upgrade --file " + v2 + " --approve": "POST /api/v1/plugins/<id>/upgrade",
		"rollback acme_backup --restore-state": "POST /api/v1/plugins/acme_backup/rollback"} {
		_, err := e.run(strings.Fields(args)...)
		var running *serverRunningError
		if !errors.As(err, &running) || !strings.Contains(err.Error(), route) {
			t.Fatalf("%s with the server running = %v", args, err)
		}
	}
	if v, _ := layout.New(e.data).Current("acme_backup"); v != "1.0.0" {
		t.Fatalf("current = %s", v)
	}
}
