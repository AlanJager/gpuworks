package selfinstall

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestClusterMemberReadsTheMarkersOfAMember(t *testing.T) {
	twoMembers := "- ID: 1\n  Address: 10.0.0.1:9001\n  Role: 0\n- ID: 2\n  Address: 10.0.0.2:9001\n  Role: 0\n"
	oneMember := "- ID: 1\n  Address: 127.0.0.1:35979\n  Role: 0\n"
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{name: "ordinary sqlite installation", files: map[string]string{"gpuworks.db": "db"}},
		{name: "empty data directory"},
		{name: "single member replicated database", files: map[string]string{"gpuworks.db.dqlite/cluster.yaml": oneMember}},
		{name: "enrollment configuration of a seed", files: map[string]string{
			"cluster-enrollment/config.json": "{}", "gpuworks.db.dqlite/cluster.yaml": oneMember}, want: true},
		{name: "unfinished join", files: map[string]string{"gpuworks.db.join/state": "x"}, want: true},
		{name: "second database member", files: map[string]string{"gpuworks.db.dqlite/cluster.yaml": twoMembers}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tc.files {
				writeFile(t, filepath.Join(dir, name), content, 0o600)
			}
			got, err := ClusterMember(dir)
			if err != nil || got != tc.want {
				t.Fatalf("ClusterMember = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	if got, err := ClusterMember(""); err != nil || got {
		t.Fatalf("no data directory = %v, %v", got, err)
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gpuworks.db.dqlite", "cluster.yaml"), "not: [a, list", 0o600)
	if _, err := ClusterMember(dir); err == nil {
		t.Fatal("an unreadable membership file was read as an answer")
	}
}

// treeState lists every entry under root with its mode, size and, for a
// regular file, its content, so a command that touched anything shows.
func treeState(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		line := path + " " + info.Mode().String()
		if info.Mode().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			line += " " + sha256Hex(raw)
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// A command that acts on one server refuses a cluster member before it runs
// a privileged command, takes the upgrade lock, or changes a file.
func TestSingleServerCommandsRefuseAClusterMemberBeforeAnyChange(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(h *harness) error
	}{
		{name: "upgrade", run: func(h *harness) error {
			archive := filepath.Join(h.t.TempDir(), "release.tar.gz")
			writeFile(h.t, archive, string(tarGz(h.t, map[string]string{"gpuworks-v2.0.0-linux-amd64/gpuworks": "new binary"})), 0o600)
			h.offline()
			_, err := h.installer.Upgrade(context.Background(), archiveReq(archive))
			return err
		}},
		{name: "upgrade check", run: func(h *harness) error {
			req := UpgradeRequest{Check: true, Keep: DefaultBackupKeep}
			h.offline()
			_, err := h.installer.Upgrade(context.Background(), req)
			return err
		}},
		{name: "restart", run: func(h *harness) error { _, err := h.installer.Restart(context.Background()); return err }},
		{name: "stop", run: func(h *harness) error { _, err := h.installer.Stop(context.Background()); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, t.TempDir())
			h.writeExistingInstall("v1.0.0")
			writeFile(t, filepath.Join(h.layout.DataDir, "cluster-enrollment", "config.json"), `{"storage_tier":"standard"}`, 0o600)
			before := treeState(t, h.layout.Dir)
			ranBefore := len(h.run.executed())

			err := tc.run(h)
			if !errors.Is(err, ErrClusterMember) {
				t.Fatalf("err = %v", err)
			}
			for _, want := range []string{"acts on one server only", "nothing was changed", "explanation/control-plane-cluster", h.layout.DataDir} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q does not say %q", err, want)
				}
			}
			for _, cmd := range h.run.executed()[ranBefore:] {
				if strings.HasPrefix(cmd, "sudo ") || strings.Contains(cmd, "systemctl") || strings.Contains(cmd, "chown") {
					t.Fatalf("the refused command ran %q", cmd)
				}
			}
			if after := treeState(t, h.layout.Dir); after != before {
				t.Fatalf("the refused command changed the installation:\n%s\nbefore:\n%s", after, before)
			}
		})
	}
}

// The same commands still act on an installation that is not a member, also
// when it runs the replicated database with itself as the only member.
func TestSingleServerCommandsStillActOnASingleMemberDatabase(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	writeFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db.dqlite", "cluster.yaml"), "- ID: 1\n  Address: 127.0.0.1:35979\n  Role: 0\n", 0o600)
	if mode, err := h.installer.Stop(context.Background()); err != nil || mode != ModeSystemd || !h.run.ran("sudo systemctl stop gpuworks") {
		t.Fatalf("stop = %q, %v: %v", mode, err, h.run.executed())
	}
}
