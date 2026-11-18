package pluginexec

import (
	"os"
	"path/filepath"
	"testing"
)

// Fixtures copied from real hosts: a systemd service on a unified host, a
// hybrid host, a v1-only host and a container without a cgroup namespace.
const (
	unifiedMountinfo = `22 1 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw
25 22 0:22 / /sys rw,nosuid,nodev,noexec,relatime shared:7 - sysfs sysfs rw
30 25 0:26 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime shared:9 - cgroup2 cgroup2 rw,nsdelegate,memory_recursiveprot
`
	hybridMountinfo = `25 22 0:22 / /sys rw shared:7 - sysfs sysfs rw
30 25 0:26 / /sys/fs/cgroup ro,nosuid shared:9 - tmpfs tmpfs ro,mode=755
31 30 0:27 / /sys/fs/cgroup/unified rw,nosuid shared:10 - cgroup2 cgroup2 rw,nsdelegate
33 30 0:29 / /sys/fs/cgroup/memory rw,nosuid shared:14 - cgroup cgroup rw,memory
`
	v1Mountinfo = `25 22 0:22 / /sys rw shared:7 - sysfs sysfs rw
30 25 0:26 / /sys/fs/cgroup ro,nosuid shared:9 - tmpfs tmpfs ro,mode=755
33 30 0:29 / /sys/fs/cgroup/memory rw,nosuid shared:14 - cgroup cgroup rw,memory
34 30 0:30 / /sys/fs/cgroup/pids rw,nosuid shared:15 - cgroup cgroup rw,pids
`
	containerMountinfo = `600 550 0:26 /system.slice/docker-ab12.scope /sys/fs/cgroup ro,nosuid - cgroup2 cgroup rw
`
)

func TestParseCgroupPlace(t *testing.T) {
	for name, tc := range map[string]struct {
		mountinfo, self string
		root, parent    string
		ok              bool
	}{
		"unified service": {unifiedMountinfo, "0::/system.slice/gpuworks.service\n", "/sys/fs/cgroup", "system.slice/gpuworks.service", true},
		"already in the server leaf": {unifiedMountinfo, "0::/system.slice/gpuworks.service/server\n",
			"/sys/fs/cgroup", "system.slice/gpuworks.service", true},
		"hybrid": {hybridMountinfo, "12:memory:/system.slice/gpuworks.service\n0::/system.slice/gpuworks.service\n",
			"/sys/fs/cgroup/unified", "system.slice/gpuworks.service", true},
		"v1 only":          {v1Mountinfo, "12:memory:/system.slice/gpuworks.service\n4:pids:/system.slice/gpuworks.service\n", "", "", false},
		"root cgroup":      {unifiedMountinfo, "0::/\n", "", "", false},
		"deleted":          {unifiedMountinfo, "0::/system.slice/gpuworks.service (deleted)\n", "", "", false},
		"no cgroup2 mount": {"25 22 0:22 / /sys rw - sysfs sysfs rw\n", "0::/system.slice/gpuworks.service\n", "", "", false},
		"container below its mount root": {containerMountinfo, "0::/system.slice/docker-ab12.scope/app\n",
			"/sys/fs/cgroup", "app", true},
		"container at its mount root": {containerMountinfo, "0::/system.slice/docker-ab12.scope\n", "", "", false},
		"outside the mounted subtree": {containerMountinfo, "0::/system.slice/other.service\n", "", "", false},
		"escaped mount point": {`30 25 0:26 / /mnt/cg\040v2 rw - cgroup2 cgroup2 rw` + "\n", "0::/a.service\n",
			"/mnt/cg v2", "a.service", true},
	} {
		t.Run(name, func(t *testing.T) {
			root, parent, ok := parseCgroupPlace([]byte(tc.mountinfo), []byte(tc.self))
			if root != tc.root || parent != tc.parent || ok != tc.ok {
				t.Fatalf("parseCgroupPlace = %q, %q, %v; want %q, %q, %v", root, parent, ok, tc.root, tc.parent, tc.ok)
			}
		})
	}
}

func TestEnterServerLeafMovesTheServerOutOfTheServiceCgroup(t *testing.T) {
	c := fakeCgroupfs(t, "acme")
	leaf := filepath.Join(c.Root, c.Parent, ServerLeaf)
	if err := EnterServerLeaf(c.Root, c.Parent, 4242); err == nil {
		t.Fatal("a leaf without cgroup.procs took the server; cgroupfs always has the file")
	}
	if err := os.WriteFile(filepath.Join(leaf, "cgroup.procs"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnterServerLeaf(c.Root, c.Parent, 4242); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(leaf, "cgroup.procs")); got != "4242" {
		t.Fatalf("server leaf cgroup.procs = %q", got)
	}
}

func TestPlacedNeedsThePidAndAMemoryLimit(t *testing.T) {
	c := fakeCgroupfs(t, "acme", "cgroup.procs")
	dir := c.Dir("acme")
	write := func(file, body string) {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cgroup.procs", "4242\n4243\n")
	if Placed(c, "acme", 4242) {
		t.Fatal("placed without memory.max: the memory controller is not enabled, nothing bounds the plugin")
	}
	write("memory.max", "134217728\n")
	if !Placed(c, "acme", 4242) {
		t.Fatal("the plugin is listed and limited but not reported placed")
	}
	if Placed(c, "acme", 424) || Placed(c, "other", 4242) {
		t.Fatal("a pid not in the plugin's cgroup was reported placed")
	}
}
