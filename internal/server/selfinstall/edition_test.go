package selfinstall

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installedEdition makes the installed binary answer EditionCommand; an empty
// edition makes it a binary from before editions, which exits 2.
func (h *harness) installedEdition(edition string) {
	h.answerEdition(h.layout.Binary(), edition)
}

// stagedEdition does the same for the binary an upgrade stages.
func (h *harness) stagedEdition(edition string) {
	h.answerEdition(h.installer.stagedBinary(), edition)
}

func (h *harness) answerEdition(binary, edition string) {
	h.run.mu.Lock()
	defer h.run.mu.Unlock()
	key := binary + " " + EditionCommand
	if edition == "" {
		delete(h.run.output, key)
		h.run.fail[key] = errors.New("exit status 2")
		return
	}
	delete(h.run.fail, key)
	h.run.output[key] = edition + "\n"
}

func TestBinaryEdition(t *testing.T) {
	h := newHarness(t, t.TempDir())
	ctx := context.Background()
	cases := map[string]struct {
		answer func()
		want   string
	}{
		"names its edition":      {func() { h.run.output["/x edition"] = "acme\n" }, "acme"},
		"says community":         {func() { h.run.output["/x edition"] = "community" }, "community"},
		"predates editions":      {func() { h.run.fail["/x edition"] = errors.New("exit status 2") }, "community"},
		"prints nothing":         {func() {}, "community"},
		"prints the usage":       {func() { h.run.output["/x edition"] = "usage: gpuworks <subcommand>" }, "community"},
		"first line counts only": {func() { h.run.output["/x edition"] = "oem-a\nextra" }, "oem-a"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h.run.output, h.run.fail = map[string]string{}, map[string]error{}
			tc.answer()
			if got := h.installer.binaryEdition(ctx, "/x"); got != tc.want {
				t.Fatalf("edition = %q, want %q", got, tc.want)
			}
		})
	}
	// The probe's complaint about an unknown subcommand is not shown.
	for _, call := range h.run.calls {
		if call.Args[0] == EditionCommand && call.Stderr == nil {
			t.Fatalf("the edition probe writes to the operator's stderr: %+v", call)
		}
	}
}

func TestUpgradeFromAnArchiveRefusesAnotherEdition(t *testing.T) {
	cases := map[string]struct{ installed, staged string }{
		"community to acme":               {"", "acme"},
		"acme to community":               {"acme", ""},
		"acme to named community":         {"acme", "community"},
		"one edition to another":          {"acme", "oem-a"},
		"pre-edition release onto an oem": {"oem-a", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, t.TempDir())
			h.writeExistingInstall("v1.0.0")
			h.offline()
			archive := h.writeArchive("v2.0.0", []byte("new binary"))
			h.installedEdition(tc.installed)
			h.stagedEdition(tc.staged)

			_, err := h.installer.Upgrade(context.Background(), archiveReq(archive))
			var mismatch *EditionMismatchError
			if !errors.As(err, &mismatch) || mismatch.Console || mismatch.ReleaseID != "v2.0.0" ||
				!strings.Contains(err.Error(), "--allow-edition-change") || !strings.Contains(err.Error(), "nothing was changed") {
				t.Fatalf("err = %v", err)
			}
			if got := readFile(t, h.layout.Binary()); got != "old binary" {
				t.Fatalf("binary = %q", got)
			}
			if h.run.ran("sudo systemctl stop gpuworks") {
				t.Fatalf("a refused upgrade stopped the service: %v", h.run.executed())
			}
			if backups, _ := filepath.Glob(filepath.Join(h.layout.BackupsDir, "*.tar.gz")); len(backups) != 0 {
				t.Fatalf("a refused upgrade left backups: %v", backups)
			}
			assertNotStaged(t, h)
		})
	}
}

func TestUpgradeAllowsAnEditionChangeWhenAsked(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.offline()
	archive := h.writeArchive("v2.0.0", []byte("acme binary"))
	h.stagedEdition("acme")
	req := archiveReq(archive)
	req.AllowEditionChange = true

	report, err := h.installer.Upgrade(context.Background(), req)
	if err != nil {
		t.Fatalf("upgrade: %v (stderr: %s)", err, h.errOut.String())
	}
	if got := readFile(t, h.layout.Binary()); got != "acme binary" || report.To != "v2.0.0" {
		t.Fatalf("binary = %q, report = %+v", got, report)
	}
	warned := h.errOut.String()
	if !strings.Contains(warned, "from the community edition to the acme edition") || !strings.Contains(warned, h.layout.Binary()+" install --dir "+h.layout.Dir) {
		t.Fatalf("the switch was not announced: %q", warned)
	}
}

func TestUpgradeWithinAnEditionNeedsNoFlag(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.offline()
	archive := h.writeArchive("v2.0.0", []byte("new acme binary"))
	h.installedEdition("acme")
	h.stagedEdition("acme")
	if _, err := h.installer.Upgrade(context.Background(), archiveReq(archive)); err != nil {
		t.Fatalf("upgrade: %v (stderr: %s)", err, h.errOut.String())
	}
	if strings.Contains(h.errOut.String(), "edition") {
		t.Fatalf("an upgrade within one edition talked about editions: %q", h.errOut.String())
	}
}

// The channel path stages the release later than --archive does, and is
// guarded the same way: a mirror serving another edition's binary under a
// matching index is still caught by asking the binary.
func TestUpgradeFromTheChannelRefusesAnotherEditionsBinary(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	mirror := newChannelMirror(t)
	publishChannel(t, channelRelease{releaseID: "v2.0.0", binary: []byte("acme binary")}, mirror)
	h.useChannel(mirror)
	h.stagedEdition("acme")

	_, err := h.installer.Upgrade(context.Background(), channelReq())
	var mismatch *EditionMismatchError
	if !errors.As(err, &mismatch) || mismatch.Installed != "community" || mismatch.Release != "acme" {
		t.Fatalf("err = %v", err)
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" || h.run.ran("sudo systemctl stop gpuworks") {
		t.Fatalf("binary = %q, commands = %v", got, h.run.executed())
	}
}

// The console has no flag to allow a switch: it is refused, and nothing is
// staged, snapshotted or marked.
func TestStageInServiceRefusesAnotherEdition(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.prepareInServiceDatabase()
	archive := h.writeArchive("v2.0.0", []byte("acme binary"))
	h.stagedEdition("acme")

	_, err := h.installer.StageInService(context.Background(), "v1.0.0", bytes.NewReader([]byte(readFile(t, archive))), writeSnapshot("snap"), "admin")
	var mismatch *EditionMismatchError
	if !errors.As(err, &mismatch) || !mismatch.Console || mismatch.Installed != "community" || mismatch.Release != "acme" ||
		!strings.Contains(err.Error(), "gpuworks upgrade --archive <file> --allow-edition-change") {
		t.Fatalf("err = %v", err)
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" {
		t.Fatalf("binary = %q", got)
	}
	if pendingOf(t, h.layout) != nil {
		t.Fatal("a refused upgrade left a marker")
	}
	if backups, _ := filepath.Glob(filepath.Join(h.layout.BackupsDir, "*.tar.gz")); len(backups) != 0 {
		t.Fatalf("a refused upgrade left backups: %v", backups)
	}
	if _, err := os.Stat(h.installer.stagedBinary()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the staged binary was left behind: %v", err)
	}
}
