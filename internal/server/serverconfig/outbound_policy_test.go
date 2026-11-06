package serverconfig_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

func TestNormalizeOutboundPrivateHostsCanonicalizesWithoutDNS(t *testing.T) {
	got, err := serverconfig.NormalizeOutboundPrivateHosts([]string{" Mirror.Corp. ", "192.168.1.2", "10.0.0.9/24", "FD00::1234/64", "", "mirror.corp", "::ffff:192.168.1.2"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"mirror.corp", "192.168.1.2", "10.0.0.0/24", "fd00::/64"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hosts=%v, want %v", got, want)
	}
	for _, value := range []string{"https://user:secret@mirror.example/archive?token=secret", "mirror.example:8443", "*.internal", "bad_name.internal", "bücher.example", "fe80::1%eth0", "999.1.1.1", strings.Repeat("a", 254)} {
		if _, err := serverconfig.NormalizeOutboundPrivateHosts([]string{value}); !errors.Is(err, serverconfig.ErrOutboundPolicyInvalid) || strings.Contains(err.Error(), value) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid host must return safe validation error: %v", err)
		}
	}
	if _, err := serverconfig.NormalizeOutboundPrivateHosts(make([]string, 129)); !errors.Is(err, serverconfig.ErrOutboundPolicyInvalid) {
		t.Fatalf("entry limit error=%v", err)
	}
}

func TestOutboundPolicyReadRevisionAndSavePreserveOtherConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.yaml")
	initial := "# keep document header\nlisten:\n  console: [127.0.0.1:8800]\nssh_proxy: socks5://user:private-secret@proxy.example:1080 # keep proxy comment\noutbound_restrict_private_hosts: true # keep policy comment\noutbound_allow_private_hosts: [OLD.internal]\n# keep footer\n"
	writeRawOutboundConfig(t, path, initial)
	if err := os.Chmod(path, 0660); err != nil {
		t.Fatal(err)
	}
	restricted, hosts, revision, err := serverconfig.ReadOutboundPolicy(path)
	if err != nil || !restricted || !reflect.DeepEqual(hosts, []string{"OLD.internal"}) || len(revision) != 64 {
		t.Fatalf("read=(%v,%v,%q,%v)", restricted, hosts, revision, err)
	}
	changed := strings.Replace(initial, "private-secret", "other-private-secret", 1)
	writeRawOutboundConfig(t, path, changed)
	_, _, sameRevision, err := serverconfig.ReadOutboundPolicy(path)
	if err != nil || sameRevision != revision {
		t.Fatalf("unrelated credential changed revision: %q %v", sameRevision, err)
	}
	expected, err := serverconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	next, err := serverconfig.SaveOutboundPolicy(path, false, []string{" Next.Internal. ", "10.2.3.4/24"}, revision)
	if err != nil || next == revision {
		t.Fatalf("save=(%q,%v)", next, err)
	}
	expected.OutboundRestrictPrivateHosts = false
	expected.OutboundAllowPrivateHosts = []string{"next.internal", "10.2.3.0/24"}
	got, err := serverconfig.Load(path)
	if err != nil || !reflect.DeepEqual(got, expected) {
		t.Fatalf("unrelated fields changed: %+v want %+v err=%v", got, expected, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"keep document header", "keep proxy comment", "keep policy comment", "keep footer", "other-private-secret"} {
		if !strings.Contains(string(raw), text) {
			t.Fatalf("lost configuration/comment %q", text)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0660 {
		t.Fatalf("mode changed: %v %v", info, err)
	}
	restricted, hosts, afterRevision, err := serverconfig.ReadOutboundPolicy(path)
	if err != nil || restricted || !reflect.DeepEqual(hosts, expected.OutboundAllowPrivateHosts) || afterRevision != next {
		t.Fatalf("saved read=(%v,%v,%q,%v)", restricted, hosts, afterRevision, err)
	}
}

func TestOutboundPolicyStaleRevisionAndValidationNeverOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.yaml")
	writeRawOutboundConfig(t, path, "outbound_restrict_private_hosts: true\noutbound_allow_private_hosts: [first.internal]\n")
	_, _, revision, err := serverconfig.ReadOutboundPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	writeRawOutboundConfig(t, path, "outbound_restrict_private_hosts: false\noutbound_allow_private_hosts: [operator.internal]\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serverconfig.SaveOutboundPolicy(path, true, []string{"next.internal"}, revision); !errors.Is(err, serverconfig.ErrOutboundPolicyConflict) {
		t.Fatalf("stale error=%v", err)
	}
	if _, err := serverconfig.SaveOutboundPolicy(path, true, []string{"https://user:private-secret@mirror.example/"}, revision); !errors.Is(err, serverconfig.ErrOutboundPolicyInvalid) || strings.Contains(err.Error(), "private-secret") {
		t.Fatalf("unsafe validation error=%v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("rejected save changed configuration: %v", err)
	}
}

func TestOutboundPolicyRejectsUnsafeDocumentsWithoutEchoingContents(t *testing.T) {
	for _, raw := range []string{
		"outbound_allow_private_hosts: [first.internal]\n---\nssh_proxy: private-secret\n",
		"outbound_allow_private_hosts: [first.internal]\noutbound_allow_private_hosts: [private-secret]\n",
		"unknown_private_secret: hidden-value\n",
		"outbound_restrict_private_hosts: [private-secret]\n",
	} {
		t.Run("document", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "server.yaml")
			writeRawOutboundConfig(t, path, raw)
			_, _, _, err := serverconfig.ReadOutboundPolicy(path)
			if !errors.Is(err, serverconfig.ErrOutboundPolicyInvalid) || strings.Contains(err.Error(), "private-secret") || strings.Contains(err.Error(), "hidden-value") {
				t.Fatalf("read error=%v", err)
			}
			if _, err := serverconfig.SaveOutboundPolicy(path, true, nil, "revision"); !errors.Is(err, serverconfig.ErrOutboundPolicyInvalid) {
				t.Fatalf("save error=%v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != raw {
				t.Fatalf("rejected document changed: %v", err)
			}
		})
	}
	for _, raw := range []string{
		"trusted_proxies: &hosts [mirror.internal]\noutbound_allow_private_hosts: *hosts\n",
		"<<: {outbound_restrict_private_hosts: true}\noutbound_allow_private_hosts: [mirror.internal]\n",
	} {
		t.Run("aliases-and-merges", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "server.yaml")
			writeRawOutboundConfig(t, path, raw)
			_, _, revision, err := serverconfig.ReadOutboundPolicy(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := serverconfig.SaveOutboundPolicy(path, false, []string{"next.internal"}, revision); !errors.Is(err, serverconfig.ErrOutboundPolicyInvalid) {
				t.Fatalf("unsafe edit error=%v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != raw {
				t.Fatalf("rejected alias document changed: %v", err)
			}
		})
	}
}

func TestOutboundPolicyDoesNotReplaceSymlinksOrNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "actual.yaml")
	path := filepath.Join(dir, "server.yaml")
	raw := "outbound_allow_private_hosts: [first.internal]\n"
	writeRawOutboundConfig(t, target, raw)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := serverconfig.ReadOutboundPolicy(path); err == nil {
		t.Fatal("symlink exposed as editable configuration")
	}
	if _, err := serverconfig.SaveOutboundPolicy(path, true, nil, "revision"); err == nil {
		t.Fatal("symlink replaced")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink changed: %v %v", info, err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != raw {
		t.Fatalf("symlink target changed: %v", err)
	}
	if _, _, _, err := serverconfig.ReadOutboundPolicy(dir); err == nil {
		t.Fatal("directory accepted")
	}
	large := filepath.Join(dir, "large.yaml")
	writeRawOutboundConfig(t, large, strings.Repeat("# padding\n", 220000))
	if _, _, _, err := serverconfig.ReadOutboundPolicy(large); !errors.Is(err, serverconfig.ErrOutboundPolicyInvalid) {
		t.Fatalf("large file error=%v", err)
	}
}

func TestOutboundPolicyConcurrentSavesRejectOneStaleRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.yaml")
	writeRawOutboundConfig(t, path, "outbound_allow_private_hosts: [first.internal]\n")
	_, _, revision, err := serverconfig.ReadOutboundPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, host := range []string{"one.internal", "two.internal"} {
		go func(host string) {
			<-start
			_, err := serverconfig.SaveOutboundPolicy(path, true, []string{host}, revision)
			results <- err
		}(host)
	}
	close(start)
	passes, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			passes++
		} else if errors.Is(err, serverconfig.ErrOutboundPolicyConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if passes != 1 || conflicts != 1 {
		t.Fatalf("saves=%d conflicts=%d", passes, conflicts)
	}
	if _, err := serverconfig.Load(path); err != nil {
		t.Fatal(err)
	}
}

func TestOutboundPolicySaveKeepsConcurrentUnrelatedKeyEdits(t *testing.T) {
	for i := 0; i < 10; i++ {
		path := filepath.Join(t.TempDir(), "server.yaml")
		writeRawOutboundConfig(t, path, "outbound_allow_private_hosts: [first.internal]\n")
		_, _, revision, err := serverconfig.ReadOutboundPolicy(path)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := serverconfig.SaveOutboundPolicy(path, true, []string{"next.internal"}, revision); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := serverconfig.SetUsageReport(path, false); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		cfg, err := serverconfig.Load(path)
		if err != nil || !cfg.OutboundRestrictPrivateHosts || !reflect.DeepEqual(cfg.OutboundAllowPrivateHosts, []string{"next.internal"}) || cfg.UsageReport == nil || *cfg.UsageReport {
			t.Fatalf("lost concurrent edit: %+v %v", cfg, err)
		}
	}
}
