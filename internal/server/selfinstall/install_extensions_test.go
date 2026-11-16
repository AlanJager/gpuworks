package selfinstall

import (
	"os"
	"testing"
)

// Installing over an installation starts from its server.yaml, so the
// extensions sections a separately built server reads survive a reinstall.
func TestInstallKeepsTheExtensionSections(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	raw, err := os.ReadFile(h.layout.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("extensions:\n  quota:\n    key: /etc/gpuworks/quota.txt\n")...)
	if err := os.WriteFile(h.layout.ConfigPath(), raw, 0o664); err != nil {
		t.Fatal(err)
	}
	h.installSelf("binary v2")
	h.mustInstall(h.quiet())

	var quota struct {
		Key string `yaml:"key"`
	}
	if err := h.config().Section("quota", &quota); err != nil || quota.Key != "/etc/gpuworks/quota.txt" {
		t.Fatalf("quota section after reinstall = %+v, %v", quota, err)
	}
}
