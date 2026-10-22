//go:build linux && l4

package nfsfault

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The watchdog is a separate process, outside the server's freezer/network
// namespace. EOF (including test-process death) or its deadline restores NFS.
func TestMain(m *testing.M) {
	if name := os.Getenv("GPUWORKS_NFS_WATCHDOG"); name != "" {
		eof := make(chan struct{})
		go func() { _, _ = io.Copy(io.Discard, os.Stdin); close(eof) }()
		select {
		case <-eof:
		case <-time.After(4 * time.Minute):
		}
		if device := os.Getenv("GPUWORKS_NFS_FAULT_DEVICE"); device != "" {
			if err := setDeviceTable(device, os.Getenv("GPUWORKS_NFS_FAULT_TABLE")); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		restoreNFS(name)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func command(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func mustCommand(t *testing.T, args ...string) string {
	t.Helper()
	out, err := command(args...)
	if err != nil {
		t.Fatalf("%s: %v: %s", strings.Join(args, " "), err, out)
	}
	return out
}

func restoreNFS(name string) {
	_, _ = command("docker", "unpause", name)
	_, _ = command("docker", "start", name)
	_, _ = command("docker", "exec", name, "ip", "link", "set", "dev", "eth0", "up")
	_, _ = command("docker", "exec", name, "tc", "qdisc", "del", "dev", "eth0", "root")
}

type nfsLab struct {
	root, export, mount, name, address string
	device                             exportDevice
}

func newNFSLab(t *testing.T) *nfsLab {
	t.Helper()
	image := os.Getenv("GPUWORKS_NFS_FAULT_IMAGE")
	if image == "" {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal("GPUWORKS_NFS_FAULT_IMAGE must name the dedicated Ganesha fixture image")
		}
		t.Skip("explicit isolated NFS fault fixture required")
	}
	self, _ := os.Readlink("/proc/self/ns/mnt")
	init, _ := os.Readlink("/proc/1/ns/mnt")
	if os.Geteuid() != 0 || self == init {
		t.Fatal("run as root inside unshare --mount --propagation private")
	}
	root, err := os.MkdirTemp("", "gpuworks-nfs-fault-")
	if err != nil {
		t.Fatal(err)
	}
	f := &nfsLab{root: root, name: fmt.Sprintf("gpuworks-nfs-fault-%d-%d", os.Getpid(), time.Now().UnixNano())}
	f.export, f.mount = filepath.Join(f.root, "export"), filepath.Join(f.root, "mount")
	for _, path := range []string{f.export, f.mount, filepath.Join(f.root, "recovery")} {
		if err := os.Mkdir(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	config := `NFS_CORE_PARAM { Protocols = 4; }
NFSv4 { Grace_Period = 10; Lease_Lifetime = 10; RecoveryBackend = fs; }
EXPORT { Export_Id = 1; Path = /export; Pseudo = /models;
 Access_Type = RO; Squash = No_Root_Squash; Protocols = 4; Transports = TCP;
 FSAL { Name = VFS; }
}
LOG { Default_Log_Level = WARN; }
`
	t.Cleanup(func() {
		if !t.Failed() {
			if err := os.RemoveAll(f.root); err != nil {
				t.Errorf("scoped cleanup: %v", err)
			}
		}
	})
	f.device = newExportDevice(t, f.root, f.export)
	// Docker 19 can assign a different default-bridge IP after SIGKILL.
	// A private network and explicit address keep the NFS server identity stable.
	mustCommand(t, "docker", "network", "create", "--label", "gpuworks.test=nfs-fault", f.name)
	t.Cleanup(func() {
		if !t.Failed() {
			mustCommand(t, "docker", "network", "rm", f.name)
		}
	})
	subnet := mustCommand(t, "docker", "network", "inspect", "-f", "{{(index .IPAM.Config 0).Subnet}}", f.name)
	prefix, err := netip.ParsePrefix(subnet)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() > 30 {
		t.Fatalf("fixture network subnet: %q %v", subnet, err)
	}
	// Old Docker requires --subnet to mark IPAM as user configured before
	// accepting --ip. Recreate only our empty network with its reserved subnet.
	mustCommand(t, "docker", "network", "rm", f.name)
	mustCommand(t, "docker", "network", "create", "--label", "gpuworks.test=nfs-fault", "--subnet", subnet, f.name)
	f.address = prefix.Addr().Next().Next().String()
	conf := filepath.Join(f.root, "ganesha.conf")
	if err := os.WriteFile(conf, []byte(config), 0644); err != nil {
		t.Fatal(err)
	}
	mounted := false
	t.Cleanup(func() {
		restoreNFS(f.name)
		logs, _ := command("docker", "logs", f.name)
		if t.Failed() {
			t.Logf("NFS failure evidence (before cleanup):\n%s", logs)
		}
		if mounted {
			if err := unmountAfterDrain(f.mount); err != nil {
				t.Errorf("scoped unmount: %v", err)
				return
			}
		}
		if t.Failed() {
			_, _ = command("docker", "stop", "--time", "5", f.name)
			t.Logf("preserved failed fixture: container=%s root=%s", f.name, f.root)
			return
		}
		mustCommand(t, "docker", "rm", "-f", f.name)

		t.Logf("cleanup: unmounted isolated NFS and removed %s", f.name)
	})
	mustCommand(t, "docker", "run", "-d", "--name", f.name, "--label", "gpuworks.test=nfs-fault",
		"--cap-add=DAC_READ_SEARCH", "--cap-add=NET_ADMIN", "--cap-add=SYS_ADMIN", "--device", "/dev/mapper/"+f.device.name+":/dev/nfs-export:rw", "--security-opt", "apparmor=unconfined",
		"--cpus=1", "--memory=256m", "--pids-limit=128", "--network", f.name, "--ip", f.address,
		"-v", conf+":/etc/ganesha/ganesha.conf:ro",
		"-v", filepath.Join(f.root, "recovery")+":/var/lib/nfs/ganesha", image)
	actual := mustCommand(t, "docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", f.name)
	if actual != f.address {
		t.Fatalf("fixture address changed: %s != %s", actual, f.address)
	}
	// Retries only readiness, never a test failure. No access to host exports.
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, err := command("mount", "-t", "nfs", "-o", "vers=4.1,proto=tcp,ro,hard,timeo=10,retrans=2,rsize=65536,nosharecache", f.address+":/models", f.mount)
		if err == nil {
			mounted = true
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fixture NFS mount: %v %s", err, out)
		}
		timer := time.NewTimer(200 * time.Millisecond)
		<-timer.C
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(f.mount, &stat); err != nil || uint64(stat.Type) != 0x6969 {
		t.Fatalf("not kernel NFS: %+v %v", stat, err)
	}
	t.Logf("real NFSv4.1 hard mount; private container=%s image=%s", f.name, mustCommand(t, "docker", "inspect", "-f", "{{.Image}}", f.name))
	return f
}

func (f *nfsLab) fault(t *testing.T, mode string) func() {
	t.Helper()
	// Arm recovery before modifying the fault path. The child has no test flag
	// filtering dependency and survives a test timeout, fatal assertion or SIGKILL.
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), "GPUWORKS_NFS_WATCHDOG="+f.name, "GPUWORKS_NFS_FAULT_DEVICE="+f.device.name, "GPUWORKS_NFS_FAULT_TABLE="+f.device.table)
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	pipe, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		_ = pipe.Close()
		if err := child.Wait(); err != nil {
			t.Errorf("recovery watchdog: %v", err)
		}
		actual, outErr := command("docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", f.name)
		if outErr != nil || actual != f.address {
			t.Errorf("NFS address after restore: %s want %s err=%v", actual, f.address, outErr)
		}
		t.Logf("fault restored: %s", mode)
	}
	t.Cleanup(restore)
	switch mode {
	case "blackhole", "cancel":
		if os.Getenv("GPUWORKS_NFS_FAULT_NETWORK") == "link" {
			mustCommand(t, "docker", "exec", f.name, "ip", "link", "set", "dev", "eth0", "down")
			t.Log("network fault uses isolated interface down (netem unavailable on this host)")
			break
		}
		mustCommand(t, "docker", "exec", f.name, "tc", "qdisc", "replace", "dev", "eth0", "root", "netem", "loss", "100%")
	case "loss":
		mustCommand(t, "docker", "exec", f.name, "tc", "qdisc", "replace", "dev", "eth0", "root", "netem", "delay", "50ms", "10ms", "loss", "10%")
	case "pause":
		mustCommand(t, "docker", "pause", f.name)
	case "eio":
		mustCommand(t, "sync", "-f", f.export)
		mustCommand(t, "mount", "-o", "remount,ro", f.export)
		if err := setDeviceTable(f.device.name, fmt.Sprintf("0 %d error", exportSectors)); err != nil {
			t.Fatal(err)
		}
	case "crash":
		mustCommand(t, "docker", "kill", "--signal=KILL", f.name)
	default:
		t.Fatalf("unknown fault %q", mode)
	}
	t.Logf("fault injected: %s", mode)
	return restore
}

// Agent shutdown cancels transfers asynchronously. Wait for its open NFS
// references to drain, without force/lazy unmount hiding a resource leak.
func unmountAfterDrain(path string) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		out, err := command("umount", path)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%v: %s", err, out)
		}
		timer := time.NewTimer(200 * time.Millisecond)
		<-timer.C
	}
}
