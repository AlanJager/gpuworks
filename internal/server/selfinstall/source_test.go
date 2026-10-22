package selfinstall

import (
	"context"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeArchive lays down a release archive the way scripts/package_release.sh
// builds one (the binary and INSTALL.txt under a versioned directory) and makes
// the binary staged from it answer `version` as a release build would.
func (h *harness) writeArchive(releaseID string, binary []byte) string {
	h.t.Helper()
	dir := fmt.Sprintf("gpuworks-%s-linux-amd64", releaseID)
	body := tarGz(h.t, map[string]string{
		dir + "/" + binaryName: string(binary),
		dir + "/INSTALL.txt":   "install notes",
	})
	path := filepath.Join(h.t.TempDir(), dir+".tar.gz")
	writeFile(h.t, path, string(body), 0o644)
	h.stagedReports("gpuworks " + releaseID + " (cafe1234, 2026-10-08T00:00:00Z)\n")
	return path
}

// stagedReports sets what the binary unpacked from an archive prints for
// `version`. It also answers the console upgrade protocol, as every release
// since console upgrades does; stagedPredatesConsoleUpgrades undoes that.
func (h *harness) stagedReports(out string) {
	h.run.mu.Lock()
	defer h.run.mu.Unlock()
	h.run.output[h.installer.stagedBinary()+" version"] = out
	h.run.output[h.installer.stagedBinary()+" "+ConsoleUpgradeProtocolCommand] = "1\n"
}

// stagedPredatesConsoleUpgrades makes the staged binary a release from before
// console upgrades: like any gpuworks binary it rejects a subcommand it does
// not know with exit status 2.
func (h *harness) stagedPredatesConsoleUpgrades() {
	h.run.mu.Lock()
	defer h.run.mu.Unlock()
	delete(h.run.output, h.installer.stagedBinary()+" "+ConsoleUpgradeProtocolCommand)
	h.run.fail[h.installer.stagedBinary()+" "+ConsoleUpgradeProtocolCommand] = errors.New("exit status 2")
}

// offline makes every HTTP request fail the test: an archive upgrade is for a
// host that cannot reach the download site, so it must not try.
func (h *harness) offline() {
	h.deps.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		h.t.Errorf("an archive upgrade made an HTTP request: %s", req.URL)
		return nil, errors.New("offline")
	})}
	h.installer = New(h.layout, h.deps, h.io)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func archiveReq(path string) UpgradeRequest {
	return UpgradeRequest{Archive: path, Keep: DefaultBackupKeep}
}

// assertNotStaged checks that nothing unpacked from an archive is left behind.
func assertNotStaged(t *testing.T, h *harness) {
	t.Helper()
	if _, err := os.Stat(h.installer.stagedBinary()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the staged binary was left behind: %v", err)
	}
}

func TestUpgradeFromAnArchiveReplacesTheBinaryAndKeepsABackup(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.offline()
	archive := h.writeArchive("v2.0.0", []byte("new binary"))

	report, err := h.installer.Upgrade(context.Background(), archiveReq(archive))
	if err != nil {
		t.Fatalf("upgrade: %v (stderr: %s)", err, h.errOut.String())
	}
	if got := readFile(t, h.layout.Binary()); got != "new binary" {
		t.Fatalf("binary = %q", got)
	}
	if report.From != "v1.0.0" || report.To != "v2.0.0" || report.RolledBack {
		t.Fatalf("report = %+v", report)
	}
	// What the release is comes from the binary itself, as the site's manifest
	// would otherwise say.
	if report.Commit != "cafe1234" || report.BuiltAt != "2026-10-08T00:00:00Z" || report.Source != "" {
		t.Fatalf("report = %+v", report)
	}
	if !h.run.ranInOrder(h.installer.stagedBinary()+" version", "sudo systemctl stop gpuworks", "sudo systemctl restart gpuworks") {
		t.Fatalf("the archive's binary was not checked before the service was stopped: %v", h.run.executed())
	}
	if got := readArchive(t, report.Backup)["bin/gpuworks"]; got != "old binary" {
		t.Fatalf("backup binary = %q", got)
	}
	entries, err := ReadHistory(h.layout.HistoryPath())
	if err != nil || len(entries) != 1 {
		t.Fatalf("history = %+v, %v", entries, err)
	}
	if e := entries[0]; e.Action != ActionUpgrade || e.Result != "ok" || e.ReleaseID != "v2.0.0" || e.PreviousReleaseID != "v1.0.0" {
		t.Fatalf("history entry = %+v", e)
	}
	// The operator's archive is theirs: the upgrade reads it and leaves it.
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("the archive was removed: %v", err)
	}
	assertNotStaged(t, h)
}

func TestUpgradeFromAnArchiveRollsBackWhenTheNewVersionNeverAnswers(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.offline()
	archive := h.writeArchive("v2.0.0", []byte("new binary"))
	h.deps.Health = func(context.Context, string) (string, error) { return "", errors.New("connection refused") }
	h.installer = New(h.layout, h.deps, h.io)

	report, err := h.installer.Upgrade(context.Background(), archiveReq(archive))
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v", err)
	}
	if !report.RolledBack || report.Backup == "" {
		t.Fatalf("report = %+v", report)
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" {
		t.Fatalf("the previous binary was not restored: %q", got)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("the archive was removed: %v", err)
	}
	assertNotStaged(t, h)
}

func TestUpgradeFromAnArchiveCheckAndUpToDateTouchNothing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed string
		check     bool
		want      func(UpgradeReport) bool
	}{
		{"check", "v1.0.0", true, func(r UpgradeReport) bool { return r.Checked && !r.UpToDate && r.To == "v2.0.0" }},
		{"current", "v2.0.0", false, func(r UpgradeReport) bool { return r.UpToDate && !r.Checked }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, t.TempDir())
			h.writeExistingInstall(tc.installed)
			h.offline()
			req := archiveReq(h.writeArchive("v2.0.0", []byte("new binary")))
			req.Check = tc.check

			report, err := h.installer.Upgrade(context.Background(), req)
			if err != nil {
				t.Fatalf("upgrade: %v", err)
			}
			if !tc.want(report) {
				t.Fatalf("report = %+v", report)
			}
			if got := readFile(t, h.layout.Binary()); got != "old binary" {
				t.Fatalf("binary = %q", got)
			}
			if h.run.ran("sudo systemctl stop gpuworks") {
				t.Fatalf("the service was stopped: %v", h.run.executed())
			}
			if _, err := os.Stat(h.layout.BackupsDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a backup was taken: %v", err)
			}
			assertNotStaged(t, h)
		})
	}
}

func TestUpgradeFromAnArchiveHonoursAPinnedVersion(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.offline()
	req := archiveReq(h.writeArchive("v2.0.0", []byte("new binary")))
	req.Version = "v1.5.0"

	_, err := h.installer.Upgrade(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "the archive is at v2.0.0") {
		t.Fatalf("err = %v", err)
	}
	if h.run.ran("sudo systemctl stop gpuworks") {
		t.Fatalf("the service was stopped for a release that was not asked for: %v", h.run.executed())
	}
	assertNotStaged(t, h)

	req.Version = "v2.0.0"
	if _, err := h.installer.Upgrade(context.Background(), req); err != nil {
		t.Fatalf("a matching --version must be accepted: %v", err)
	}
	if got := readFile(t, h.layout.Binary()); got != "new binary" {
		t.Fatalf("binary = %q", got)
	}
}

// Every archive that cannot be used is refused before the service is stopped,
// and leaves nothing staged.
func TestUpgradeFromAnArchiveRefusesWhatItCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		archive func(h *harness) string
		want    string
	}{
		{"missing file", func(h *harness) string {
			return filepath.Join(t.TempDir(), "gpuworks-v2.0.0-linux-amd64.tar.gz")
		}, "no such file"},
		{"a directory", func(h *harness) string { return t.TempDir() }, "is not a file"},
		{"not gzip", func(h *harness) string {
			path := filepath.Join(t.TempDir(), "gpuworks.tar.gz")
			writeFile(t, path, "plain text", 0o644)
			return path
		}, "gzip"},
		{"no binary inside", func(h *harness) string {
			path := filepath.Join(t.TempDir(), "gpuworks.tar.gz")
			writeFile(t, path, string(tarGz(t, map[string]string{"INSTALL.txt": "notes"})), 0o644)
			return path
		}, "does not contain a gpuworks binary"},
		{"binary for another platform", func(h *harness) string {
			path := h.writeArchive("v2.0.0", []byte("new binary"))
			h.run.fail[h.installer.stagedBinary()+" version"] = errors.New("exec format error")
			return path
		}, "does not run on this"},
		{"local build", func(h *harness) string {
			path := h.writeArchive("v2.0.0", []byte("new binary"))
			h.stagedReports("gpuworks dev (none, unknown)\n")
			return path
		}, "local build"},
		{"dirty build", func(h *harness) string {
			path := h.writeArchive("v2.0.0", []byte("new binary"))
			h.stagedReports("gpuworks v2.0.0-dirty (cafe1234, 2026-10-08)\n")
			return path
		}, "local build"},
		{"unrecognised version output", func(h *harness) string {
			path := h.writeArchive("v2.0.0", []byte("new binary"))
			h.stagedReports("something else entirely\n")
			return path
		}, "carries no release id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, t.TempDir())
			h.writeExistingInstall("v1.0.0")
			h.offline()

			_, err := h.installer.Upgrade(context.Background(), archiveReq(tc.archive(h)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if got := readFile(t, h.layout.Binary()); got != "old binary" {
				t.Fatalf("binary = %q", got)
			}
			if h.run.ran("sudo systemctl stop gpuworks") {
				t.Fatalf("the service was stopped: %v", h.run.executed())
			}
			assertNotStaged(t, h)
		})
	}
}

// The fakes above stand in for the binary answering `version`; this runs real
// executables, so it also proves the unpacked binary is executable and that
// one which cannot run is caught by actually running it.
func TestUpgradeFromAnArchiveAsksTheRealBinary(t *testing.T) {
	script := func(release string) []byte {
		return []byte("#!/bin/sh\necho 'gpuworks " + release + " (cafe1234, 2026-10-08T00:00:00Z)'\n")
	}
	newRealHarness := func(t *testing.T) *harness {
		h := newHarness(t, t.TempDir())
		h.writeExistingInstall("v1.0.0")
		writeFile(t, h.layout.Binary(), string(script("v1.0.0")), 0o755)
		h.deps.Run = ExecRunner{Euid: h.deps.Euid, Stderr: io.Discard}
		h.offline()
		return h
	}

	h := newRealHarness(t)
	req := archiveReq(h.writeArchive("v2.0.0", script("v2.0.0")))
	req.Check = true
	report, err := h.installer.Upgrade(context.Background(), req)
	if err != nil {
		t.Fatalf("upgrade --check: %v", err)
	}
	if report.From != "v1.0.0" || report.To != "v2.0.0" || report.Commit != "cafe1234" || !report.Checked {
		t.Fatalf("report = %+v", report)
	}
	assertNotStaged(t, h)

	// A file the kernel will not execute is what a binary for another
	// architecture looks like from here.
	h = newRealHarness(t)
	_, err = h.installer.Upgrade(context.Background(), archiveReq(h.writeArchive("v2.0.0", []byte("not an executable"))))
	if err == nil || !strings.Contains(err.Error(), "does not run on this") {
		t.Fatalf("err = %v", err)
	}
	assertNotStaged(t, h)
}

// elfHeader is the smallest ELF file debug/elf accepts: a 64-bit header
// naming machine, with no program or section headers.
func elfHeader(machine elf.Machine) []byte {
	h := make([]byte, 64)
	copy(h, elf.ELFMAG)
	h[elf.EI_CLASS], h[elf.EI_DATA], h[elf.EI_VERSION] = byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)
	le := binary.LittleEndian
	le.PutUint16(h[16:], uint16(elf.ET_EXEC))
	le.PutUint16(h[18:], uint16(machine))
	le.PutUint32(h[20:], uint32(elf.EV_CURRENT))
	le.PutUint16(h[52:], 64) // e_ehsize
	le.PutUint16(h[54:], 56) // e_phentsize
	le.PutUint16(h[58:], 64) // e_shentsize
	return h
}

// foreignMachine is an architecture releases are built for, other than this one.
func foreignMachine(t *testing.T) elf.Machine {
	_, goarch := currentPlatform()
	if _, ok := elfMachines[goarch]; !ok {
		t.Skipf("no release is built for %s", goarch)
	}
	if goarch == "amd64" {
		return elf.EM_AARCH64
	}
	return elf.EM_X86_64
}

func TestCheckBinaryMachine(t *testing.T) {
	_, goarch := currentPlatform()
	foreign := foreignMachine(t)
	for _, tc := range []struct {
		name, content, want string
	}{
		{"native ELF", string(elfHeader(elfMachines[goarch])), ""},
		{"foreign ELF", string(elfHeader(foreign)), "built for " + foreign.String()},
		{"truncated ELF", elf.ELFMAG + "\x02", "EOF"},
		// What is not ELF is left to running it.
		{"script", "#!/bin/sh\necho hi\n", ""},
		{"shorter than the magic", "ab", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gpuworks")
			writeFile(t, path, tc.content, 0o755)
			err := checkBinaryMachine(path, goarch)
			if tc.want == "" && err != nil {
				t.Fatalf("err = %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A host with qemu binfmt handlers would run a foreign binary and report its
// version, so the architecture is read from the binary before it is run.
func TestUpgradeFromAnArchiveRefusesAnotherArchitectureWithoutRunningIt(t *testing.T) {
	foreign := foreignMachine(t)
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.offline()
	archive := h.writeArchive("v2.0.0", elfHeader(foreign))

	_, err := h.installer.Upgrade(context.Background(), archiveReq(archive))
	if err == nil || !strings.Contains(err.Error(), "built for "+foreign.String()) {
		t.Fatalf("err = %v", err)
	}
	if h.run.ran(h.installer.stagedBinary() + " version") {
		t.Fatalf("a binary for another architecture was run: %v", h.run.executed())
	}
	if h.run.ran("sudo systemctl stop gpuworks") {
		t.Fatalf("the service was stopped: %v", h.run.executed())
	}
	assertNotStaged(t, h)
}

func TestParseVersionLine(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want binaryBuild
	}{
		{"gpuworks v2.0.0 (cafe1234, 2026-10-08T00:00:00Z)\n", binaryBuild{"v2.0.0", "cafe1234", "2026-10-08T00:00:00Z"}},
		{"gpuworks dev (none, unknown)", binaryBuild{"dev", "none", "unknown"}},
		{"gpuworks v2.0.0", binaryBuild{ReleaseID: "v2.0.0"}},
		{"gpuworks (cafe1234, x)", binaryBuild{}},
		{"other v2.0.0 (a, b)", binaryBuild{}},
		{"", binaryBuild{}},
	} {
		if got := parseVersionLine(tc.in); got != tc.want {
			t.Errorf("parseVersionLine(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}
