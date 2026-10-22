package selfinstall

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/nodeaddr"
)

func TestRenderUnitQuotesEverythingThatNeedsIt(t *testing.T) {
	cfg := UnitConfig{
		Binary:      "/opt/gpu works/100%/gpuworks",
		ConfigPath:  "/opt/gpu works/100%/server.yaml",
		DataDir:     "/opt/gpu works/100%/data",
		ServiceUser: "gpuworks",
	}
	unit := RenderUnit(cfg)

	// A bare % is a specifier in a unit file; a literal one must be doubled.
	if !strings.Contains(unit, `ExecStart="/opt/gpu works/100%%/gpuworks" serve --config "/opt/gpu works/100%%/server.yaml"`) {
		t.Fatalf("ExecStart is not quoted:\n%s", unit)
	}
	if !strings.Contains(unit, "ExecStartPre=/usr/bin/test -x \"/opt/gpu works/100%%/gpuworks\"") {
		t.Fatalf("ExecStartPre is not quoted:\n%s", unit)
	}
	if !strings.Contains(unit, "RequiresMountsFor=\"/opt/gpu works/100%%/data\"") {
		t.Fatalf("RequiresMountsFor is not quoted:\n%s", unit)
	}
	if !strings.Contains(unit, "User=gpuworks") {
		t.Fatalf("the service user is missing:\n%s", unit)
	}
	// Every setting is in server.yaml; the unit carries none.
	if strings.Contains(unit, "Environment") {
		t.Fatalf("the unit carries settings:\n%s", unit)
	}
	// The server owns no containers, so its children must die with it, unlike
	// the agent's unit.
	if strings.Contains(unit, "KillMode") {
		t.Fatalf("the server unit must keep the default KillMode:\n%s", unit)
	}
	// What the unit starts reads back as this installation.
	if !unitRuns([]byte(unit), cfg.Binary) || unitUser([]byte(unit)) != "gpuworks" {
		t.Fatalf("the unit does not read back: runs=%v user=%q", unitRuns([]byte(unit), cfg.Binary), unitUser([]byte(unit)))
	}
}

func TestRenderUnitOmitsWhatWasNotConfigured(t *testing.T) {
	unit := RenderUnit(UnitConfig{
		Binary:     "/opt/gpuworks/server/gpuworks",
		ConfigPath: "/opt/gpuworks/server/server.yaml",
		DataDir:    "/opt/gpuworks/server/data",
	})
	if strings.Contains(unit, "User=") || unitUser([]byte(unit)) != "" {
		t.Fatalf("a root install must not carry a User= line:\n%s", unit)
	}
	if !strings.Contains(unit, "WantedBy=multi-user.target") {
		t.Fatalf("the unit is not enabled at boot:\n%s", unit)
	}
}

func TestServeArgsMatchTheUnit(t *testing.T) {
	cfg := UnitConfig{
		Binary:      "/opt/gpuworks/server/gpuworks",
		ConfigPath:  "/opt/gpuworks/server/server.yaml",
		DataDir:     "/opt/gpuworks/server/data",
		ServiceUser: "gpuworks",
	}
	args := serveArgs(cfg)
	if strings.Join(args, " ") != "serve --config /opt/gpuworks/server/server.yaml" {
		t.Fatalf("serveArgs = %v", args)
	}
	if !strings.Contains(RenderUnit(cfg), "ExecStart=/opt/gpuworks/server/gpuworks "+strings.Join(args, " ")+"\n") {
		t.Fatalf("the unit and the nohup command line differ:\n%s", RenderUnit(cfg))
	}
}

func TestSplitAddrAndHealthURL(t *testing.T) {
	cases := []struct {
		addr       string
		wantHost   string
		wantURL    string
		wantErrSub string
	}{
		{addr: ":8800", wantHost: "", wantURL: "http://127.0.0.1:8800/healthz"},
		{addr: "8800", wantHost: "", wantURL: "http://127.0.0.1:8800/healthz"},
		{addr: "0.0.0.0:8800", wantHost: "0.0.0.0", wantURL: "http://127.0.0.1:8800/healthz"},
		{addr: "[::]:8800", wantHost: "::", wantURL: "http://[::1]:8800/healthz"},
		{addr: "127.0.0.1:8800", wantHost: "127.0.0.1", wantURL: "http://127.0.0.1:8800/healthz"},
		{addr: "10.0.0.5:8800", wantHost: "10.0.0.5", wantURL: "http://10.0.0.5:8800/healthz"},
		{addr: "", wantErrSub: "empty"},
		{addr: "host:port:extra", wantErrSub: "invalid --addr"},
		{addr: "1.2.3.4:", wantErrSub: "no port"},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			host, port, err := splitAddr(tc.addr)
			if tc.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("err = %v, want %q", err, tc.wantErrSub)
				}
				if _, err := healthURL(tc.addr); err == nil {
					t.Fatal("healthURL accepted an address splitAddr rejects")
				}
				return
			}
			if err != nil {
				t.Fatalf("splitAddr: %v", err)
			}
			if host != tc.wantHost {
				t.Fatalf("host = %q, want %q", host, tc.wantHost)
			}
			if port == "" {
				t.Fatal("port is empty")
			}
			url, err := healthURL(tc.addr)
			if err != nil {
				t.Fatalf("healthURL: %v", err)
			}
			if url != tc.wantURL {
				t.Fatalf("url = %q, want %q", url, tc.wantURL)
			}
		})
	}
}

func TestNewLayoutDefaults(t *testing.T) {
	l := NewLayout("", "", "")
	if l.Dir != DefaultDir {
		t.Fatalf("dir = %q", l.Dir)
	}
	if l.DataDir != "/opt/gpuworks/server/data" || l.LogsDir != "/opt/gpuworks/server/logs" {
		t.Fatalf("layout = %+v", l)
	}
	if l.LogFile() != "/opt/gpuworks/server/logs/gpuworks-server.log" {
		t.Fatalf("log file = %q", l.LogFile())
	}
	if l.CredentialsPath() != "/opt/gpuworks/server/data/initial-credentials" {
		t.Fatalf("credentials = %q", l.CredentialsPath())
	}
	if l.UnitPath() != "/etc/systemd/system/gpuworks.service" {
		t.Fatalf("unit = %q", l.UnitPath())
	}

	custom := NewLayout("/srv/gpuworks/", "/var/lib/gpuworks", "")
	if custom.Dir != "/srv/gpuworks" || custom.DataDir != "/var/lib/gpuworks" {
		t.Fatalf("layout = %+v", custom)
	}
	if custom.LogsDir != "/srv/gpuworks/logs" {
		t.Fatalf("logs dir = %q", custom.LogsDir)
	}
}

func TestBackupPathIsAReadableTimeline(t *testing.T) {
	l := NewLayout("/opt/gpuworks/server", "", "")
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	path := l.BackupPath("v1.2.3+build.4", at)
	if filepath.Base(path) != "v1.2.3+build.4-20260304T050607Z.tar.gz" {
		t.Fatalf("backup name = %q", filepath.Base(path))
	}
	// A release id comes from the download site and must never escape the
	// backup directory.
	evil := l.BackupPath("../../etc/passwd", at)
	if filepath.Dir(evil) != l.BackupsDir {
		t.Fatalf("a release id escaped the backup directory: %q", evil)
	}
	if filepath.Base(evil) != "etcpasswd-20260304T050607Z.tar.gz" {
		t.Fatalf("sanitised name = %q", filepath.Base(evil))
	}
	if got := filepath.Base(l.BackupPath("", at)); got != "unknown-20260304T050607Z.tar.gz" {
		t.Fatalf("empty release id → %q", got)
	}
}

func TestLocalBaseURLShape(t *testing.T) {
	// The default route decides which address other hosts can use; a machine
	// with no network at all reports nothing rather than a wrong guess.
	got := nodeaddr.LocalBaseURL(":8800")
	if got == "" {
		t.Skip("no default route on this host")
	}
	if !strings.HasPrefix(got, "http://") || !strings.HasSuffix(got, ":8800") {
		t.Fatalf("LocalBaseURL = %q", got)
	}
	if nodeaddr.LocalBaseURL("nonsense") != "" {
		t.Fatal("an address without a port must not produce a base URL")
	}
}

func TestInstallCredentialPasswordIsNeverInTheUnit(t *testing.T) {
	// A unit file is 0644 and world-readable. This is the invariant the whole
	// credential hand-off exists for, so it is asserted on the rendered text
	// rather than through the install flow.
	secret := "super-secret-password"
	unit := RenderUnit(UnitConfig{
		Binary: "/opt/gpuworks/server/gpuworks", ConfigPath: "/opt/gpuworks/server/server.yaml",
		DataDir: "/opt/gpuworks/server/data", ServiceUser: "gpuworks",
	})
	if strings.Contains(unit, secret) {
		t.Fatalf("the secret leaked into the unit:\n%s", unit)
	}
	if strings.Contains(unit, "PASSWORD") {
		t.Fatalf("the unit mentions a password at all:\n%s", unit)
	}
}

func TestUnitFileIsNotWorldWritable(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	h.mustInstall(h.quiet())
	if content, ok := h.run.writtenFile(h.layout.UnitPath()); !ok || content == "" {
		t.Fatal("the unit was never written")
	}
	if !h.run.ran("sudo chmod 0644 " + h.layout.UnitPath()) {
		t.Fatalf("the unit was not given a readable mode: %v", h.run.executed())
	}
}

func TestServiceActiveChecksBothSupervisionModes(t *testing.T) {
	h := newHarness(t, t.TempDir())
	// The nohup pid file names no live process, so only systemd can report the
	// installation as active.
	writeFile(t, h.layout.PIDFile(), "999999\n", 0o644)
	if !h.installer.serviceActive(context.Background()) {
		t.Fatal("an active unit was not recognised")
	}
	h.run.fail["sudo systemctl is-active --quiet gpuworks"] = os.ErrProcessDone
	if h.installer.serviceActive(context.Background()) {
		t.Fatal("a dead pid file and an inactive unit must not count as running")
	}
}
