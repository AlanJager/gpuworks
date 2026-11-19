//go:build linux

package extplugin

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginbridge"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func withHostClock(spec *extsign.Spec) { spec.Manifest.Requires[extapi.CapHostClock] = 1 }

// A clock reset root recorded with the server stopped reaches the plugins
// through a real server: the plugin that starts at the next boot reads the
// corrected extclock and high-water mark from the host API's meta.
func TestContractOfflineClockResetReachesThePluginsMeta(t *testing.T) {
	ahead := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	right := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
	s := newSite(t)
	s.clock.set(ahead)
	sv := s.boot()
	s.args("acme_fake", "--requires", "table.banners@1-1,host.clock@1-1", "--read-meta")
	sv.install(s.fakePackage("acme_fake", "1.0.0", withHostClock), true)
	if got := s.waitResults("acme_fake", 1)[0]; got.Kind != "meta" || !got.OK || got.ExtClock != ahead.Format(time.RFC3339) {
		t.Fatalf("meta at the clock that ran ahead = %+v", got)
	}
	sv.stop()

	// The operator puts the wall clock right and records the reset, as
	// `gpuworks plugin clock reset` does.
	s.clock.set(right)
	journal := pluginbridge.MaintenancePath(layout.New(s.dir).Root)
	if err := pluginbridge.AppendMaintenance(journal, pluginbridge.MaintenanceEntry{V: pluginbridge.MaintenanceVersion,
		Op: pluginbridge.MaintenanceClockReset, At: right, By: "alice", To: right, Reason: "clock ran ahead"}); err != nil {
		t.Fatal(err)
	}
	sv = s.boot()
	want := right.Format(time.RFC3339)
	if got := s.waitResults("acme_fake", 2)[1]; got.Kind != "meta" || !got.OK || got.ExtClock != want || got.HWM != want {
		t.Fatalf("meta after the boot applied the reset = %+v, want extclock and hwm %s", got, want)
	}
	if raw, err := os.ReadFile(journal); err != nil || len(raw) != 0 {
		t.Fatalf("journal after the boot = %q, %v", raw, err)
	}
	sv.stop()
}

// server.yaml's plugins.policy.max_down_grace reaches pushed state: an
// authority that pushed a one-hour grace clamps five minutes (plus the
// boot extension at most) after it went down.
func TestContractTheSitesMaxDownGraceLowersAPushedGrace(t *testing.T) {
	path := filepath.Join(t.TempDir(), serverconfig.FileName)
	if err := os.WriteFile(path, []byte("plugins:\n  policy:\n    max_down_grace: 5m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := serverconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s := newSite(t)
	s.policy = file.Plugins.Policy
	sv := s.boot()
	s.args("acme_fake", "--requires", requiresAll(),
		"--push", `availability={"down_grace_s":3600,"rows":[{"feature":"acme.backup","down_access":"read_only","steps":[{"access":"full"}]}]}`)
	sv.install(s.fakePackage("acme_fake", "1.0.0"), true)
	if got := s.waitResults("acme_fake", 1)[0]; got.Kind != "availability" || !got.OK {
		t.Fatalf("push = %+v", got)
	}
	sv.waitPlugin("acme_fake", "running", func(p dto.Plugin) bool { return p.State == "running" })
	if f := feature(sv.manifest(), "acme.backup"); f.Access != "full" {
		t.Fatalf("acme.backup while its authority runs = %+v", f)
	}

	// The authority goes down with the server and cannot start again.
	sv.stop()
	s.clock.advance(4 * time.Minute)
	s.args("acme_fake", "--exit-at-start", "3")
	sv = s.boot()
	sv.waitPlugin("acme_fake", "down", func(p dto.Plugin) bool { return p.State == "backoff" || p.State == "crashloop" })
	if f := feature(sv.manifest(), "acme.backup"); f.Access != "full" {
		t.Fatalf("acme.backup 4 min after its authority went down = %+v", f)
	}
	// Past the site's five minutes and the boot extension, still well
	// within the pushed hour. A disable makes pushed state recombine at
	// once instead of at its next timer.
	s.clock.advance(3*time.Minute + 10*time.Second)
	if status, body := sv.do(http.MethodPost, "/api/v1/plugins/acme_fake/disable", nil); status != http.StatusOK {
		t.Fatalf("disable = %d %s", status, body)
	}
	f := feature(sv.manifest(), "acme.backup")
	if f.Access != "read_only" || f.Message == nil || f.Message.Code != messages.ErrorPluginUnavailable {
		t.Fatalf("acme.backup past the site's grace = %+v", f)
	}
	sv.stop()
}
