package pluginexec

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func validProfile() *Profile {
	return &Profile{ID: "acme_backup", ParentPID: 1, Dir: "/var/lib/gpuworks/plugins/acme_backup/data", Nice: 5, NoFile: 4096,
		Landlock: &Landlock{ReadExec: []string{"/pkg"}, ReadWrite: []string{"/data"}}}
}

func TestProfileRoundTripsAndRefusesBadInput(t *testing.T) {
	args, err := Args(validProfile(), "/pkg/bin/acme", "serve")
	if err != nil {
		t.Fatal(err)
	}
	if args[0] != Command {
		t.Fatalf("args = %v", args)
	}
	p, argv, err := ParseArgs(args[1:])
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "acme_backup" || p.NoFile != 4096 || !slices.Equal(argv, []string{"/pkg/bin/acme", "serve"}) {
		t.Fatalf("parsed %+v %v", p, argv)
	}
	withDirs := validProfile()
	withDirs.Landlock = &Landlock{DeviceDirs: []string{"/dev"}, Read: []string{"/sys/devices"}}
	enc, _ := withDirs.Encode()
	if back, err := DecodeProfile(enc); err != nil || !slices.Equal(back.Landlock.DeviceDirs, []string{"/dev"}) ||
		!strings.Contains(enc, `"device_dirs":["/dev"]`) {
		t.Fatalf("device_dirs round trip: %s, %+v, %v", enc, back, err)
	}
	enc, _ = validProfile().Encode()
	for name, a := range map[string][]string{
		"short":          {"--profile", enc},
		"no separator":   {"--profile", enc, "x", "/bin/true"},
		"relative exe":   {"--profile", enc, "--", "bin/acme"},
		"unknown field":  {"--profile", `{"id":"a","parent_pid":1,"dir":"/d","extra":1}`, "--", "/bin/true"},
		"trailing":       {"--profile", `{"id":"a","parent_pid":1,"dir":"/d"}{}`, "--", "/bin/true"},
		"relative dir":   {"--profile", `{"id":"a","parent_pid":1,"dir":"d"}`, "--", "/bin/true"},
		"no parent":      {"--profile", `{"id":"a","dir":"/d"}`, "--", "/bin/true"},
		"id with a path": {"--profile", `{"id":"../a","parent_pid":1,"dir":"/d"}`, "--", "/bin/true"},
		"nice":           {"--profile", `{"id":"a","parent_pid":1,"dir":"/d","nice":20}`, "--", "/bin/true"},
		"landlock path":  {"--profile", `{"id":"a","parent_pid":1,"dir":"/d","landlock":{"read":["etc"]}}`, "--", "/bin/true"},
		"device dir":     {"--profile", `{"id":"a","parent_pid":1,"dir":"/d","landlock":{"device_dirs":["dev"]}}`, "--", "/bin/true"},
		"cgroup escape":  {"--profile", `{"id":"a","parent_pid":1,"dir":"/d","cgroup":{"root":"/sys/fs/cgroup","parent":"../x"}}`, "--", "/bin/true"},
	} {
		if _, _, err := ParseArgs(a); err == nil {
			t.Errorf("%s: parsed %v", name, a)
		}
	}
}

// Only the allowed variables of the server's environment reach a plugin;
// the server's secrets and configuration do not.
func TestEnvironKeepsOnlyTheAllowlist(t *testing.T) {
	server := []string{"PATH=/usr/bin", "GPUWORKS_DB=/var/lib/gpuworks/gpuworks.db", "HOME=/root", "https_proxy=http://p:3128",
		"SSL_CERT_FILE=/etc/ca.pem", "AWS_SECRET_ACCESS_KEY=x", "TZ=UTC", "PATH=/evil", "GWX_SERVE_FD=9", "NOEQUALS"}
	got := Environ(server, "/data", "/tmp/p", 1)
	want := []string{"GWX_HOST_FD=4", "GWX_PROTOCOL=1", "GWX_SERVE_FD=3", "HOME=/data", "LANG=C.UTF-8", "PATH=/usr/bin",
		"SSL_CERT_FILE=/etc/ca.pem", "TMPDIR=/tmp/p", "TZ=UTC", "https_proxy=http://p:3128"}
	if !slices.Equal(got, want) {
		t.Fatalf("Environ = %v\nwant %v", got, want)
	}
	// The trampoline drops anything else it was handed.
	kept := filterEnv(append(got, "GPUWORKS_DB=/x", "LD_PRELOAD=/evil.so"), allowed())
	if !slices.Equal(kept, got) {
		t.Fatalf("filter kept %v", kept)
	}
}

// fakeCgroupfs lays out a delegated service cgroup the way cgroupfs shows
// one: control files exist for every enabled controller and none can be
// created.
func fakeCgroupfs(t *testing.T, id string, files ...string) *Cgroup {
	t.Helper()
	root := t.TempDir()
	parent := filepath.Join(root, "system.slice", "gpuworks.service")
	dir := filepath.Join(parent, "plugins", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(parent, "cgroup.subtree_control"), filepath.Join(parent, "plugins", "cgroup.subtree_control")} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Cgroup{Root: root, Parent: "system.slice/gpuworks.service", MemoryMax: 128 << 20, MemoryHigh: 100 << 20, CPUWeight: 50, PidsMax: 512}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMoveToCgroup(t *testing.T) {
	t.Run("delegated", func(t *testing.T) {
		c := fakeCgroupfs(t, "acme", "memory.max", "memory.high", "cpu.weight", "pids.max", "cgroup.procs")
		moved, warnings, err := MoveToCgroup(c, "acme", 4242)
		if err != nil || !moved || len(warnings) != 0 {
			t.Fatalf("moved=%v warnings=%v err=%v", moved, warnings, err)
		}
		dir := c.Dir("acme")
		for file, want := range map[string]string{"memory.max": "134217728", "memory.high": "104857600", "cpu.weight": "50", "pids.max": "512", "cgroup.procs": "4242"} {
			if got := readFile(t, filepath.Join(dir, file)); got != want {
				t.Errorf("%s = %q, want %q", file, got, want)
			}
		}
		if got := readFile(t, filepath.Join(c.Root, c.Parent, "plugins", "cgroup.subtree_control")); got != "+pids" {
			t.Errorf("the plugins cgroup enables %q last, want each controller written in turn", got)
		}
		if err := RemoveCgroup(c, "acme"); err == nil {
			t.Error("a cgroup with files in it was removed; cgroupfs removes only an empty one")
		}
	})
	t.Run("controllers missing", func(t *testing.T) {
		c := fakeCgroupfs(t, "acme", "cgroup.procs", "pids.max")
		moved, warnings, err := MoveToCgroup(c, "acme", 7)
		if err != nil || !moved {
			t.Fatalf("moved=%v err=%v", moved, err)
		}
		if len(warnings) != 3 || !strings.Contains(strings.Join(warnings, ";"), "memory.max") {
			t.Fatalf("warnings = %v, want the three limits without a controller", warnings)
		}
	})
	t.Run("move fails", func(t *testing.T) {
		c := fakeCgroupfs(t, "acme", "memory.max")
		moved, _, err := MoveToCgroup(c, "acme", 7)
		if err == nil || moved {
			t.Fatalf("moved=%v err=%v without cgroup.procs", moved, err)
		}
	})
	t.Run("not delegated", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can write a read-only file")
		}
		c := fakeCgroupfs(t, "other")
		if err := os.Chmod(filepath.Join(c.Root, c.Parent, "cgroup.subtree_control"), 0o444); err != nil {
			t.Fatal(err)
		}
		moved, warnings, err := MoveToCgroup(c, "acme", 7)
		if err != nil || moved || warnings != nil {
			t.Fatalf("moved=%v warnings=%v err=%v", moved, warnings, err)
		}
		if _, err := os.Stat(c.Dir("acme")); !os.IsNotExist(err) {
			t.Fatalf("a cgroup was created under an undelegated parent: %v", err)
		}
	})
	t.Run("remove", func(t *testing.T) {
		c := fakeCgroupfs(t, "gone")
		if err := RemoveCgroup(c, "gone"); err != nil {
			t.Fatal(err)
		}
		if err := RemoveCgroup(c, "gone"); err != nil {
			t.Fatalf("removing a removed cgroup: %v", err)
		}
	})
}
