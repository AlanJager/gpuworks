package layout_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
)

func TestControlRoundTripsAndRefusesUnknownFields(t *testing.T) {
	d := layout.New(t.TempDir())
	if _, ok, err := d.ReadControl("acme_x"); ok || err != nil {
		t.Fatalf("missing control = %v %v", ok, err)
	}
	want := layout.Control{Enabled: true, ApprovedHash: "sha256:" + string(make([]byte, 0)), ApprovedBy: "root", At: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)}
	if err := d.WriteControl("acme_x", want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := d.ReadControl("acme_x")
	if err != nil || !ok || got != want {
		t.Fatalf("control = %+v %v %v", got, ok, err)
	}
	info, err := os.Stat(filepath.Join(d.Package("acme_x"), "control.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("control file mode = %v %v", info, err)
	}
	if err := os.WriteFile(filepath.Join(d.Package("acme_x"), "control.json"), []byte(`{"enabled":true,"grant_everything":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.ReadControl("acme_x"); err == nil {
		t.Fatal("a control file with an unknown field was read")
	}
}

func TestActivateRefusesAMissingVersion(t *testing.T) {
	d := layout.New(t.TempDir())
	if _, err := d.Activate("acme_x", "1.0.0"); err == nil {
		t.Fatal("activated a version that is not unpacked")
	}
	if v, err := d.Current("acme_x"); v != "" || err != nil {
		t.Fatalf("current = %q %v", v, err)
	}
	if ids, err := d.Installed(); len(ids) != 0 || err != nil {
		t.Fatalf("installed = %v %v", ids, err)
	}
}

func TestStagingCommitRefusesAnExistingVersion(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() { layout.RemoveAll(dataDir) })
	d := layout.New(dataDir)
	for i := range 2 {
		s, err := d.Stage()
		if err != nil {
			t.Fatal(err)
		}
		err = s.Commit(d, "acme_x", "1.0.0")
		if i == 0 && err != nil {
			t.Fatal(err)
		}
		if i == 1 && err == nil {
			t.Fatal("a second commit replaced the version directory")
		}
		if err := s.Abort(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(d.Version("acme_x", "1.0.0")); err != nil {
		t.Fatal("abort after commit removed the committed version")
	}
}
