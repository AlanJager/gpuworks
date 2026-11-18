package serverconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const issuerKey = "0RZF/KSeB5I5emmRFX7vHLklBOUA2A8ot0mLncySqxs="

func TestLoadReadsThePluginsSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	content := `listen:
  console: [127.0.0.1:8800]
plugins:
  publishers:
    - keyid: acme-issuer-1
      alg: ed25519
      public_key: ` + issuerKey + `
      roles: [availability_authority]
      tables: [fleet_capacity]
      authority: true
  policy:
    allow_publishers: [acme, gpuworks]
    deny_packages: [acme_beta]
    deny_permissions: [read:directory]
    deny_roles: [key_wrap]
    auto_approve_unchanged: true
  allow_unsandboxed: [acme_backup]
  require_isolation: landlock
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Plugins{
		Publishers: []PluginPublisher{{KeyID: "acme-issuer-1", Alg: "ed25519", PublicKey: issuerKey,
			Roles: []string{"availability_authority"}, Tables: []string{"fleet_capacity"}, Authority: true}},
		Policy: PluginPolicy{AllowPublishers: []string{"acme", "gpuworks"}, DenyPackages: []string{"acme_beta"},
			DenyPermissions: []string{"read:directory"}, DenyRoles: []string{"key_wrap"},
			AutoApproveUnchanged: true},
		AllowUnsandboxed: []string{"acme_backup"}, RequireIsolation: "landlock",
	}
	if !reflect.DeepEqual(f.Plugins, want) {
		t.Fatalf("plugins = %+v", f.Plugins)
	}
	k, err := f.Plugins.Publishers[0].Key()
	if err != nil || k.KeyID != "acme-issuer-1" || len(k.Key) != 32 {
		t.Fatalf("key = %+v %v", k, err)
	}
}

// Without the key every upgrade needs a new approval.
func TestAutoApproveUnchangedDefaultsOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("plugins:\n  policy:\n    deny_roles: [key_wrap]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil || f.Plugins.Policy.AutoApproveUnchanged {
		t.Fatalf("policy = %+v, %v", f.Plugins.Policy, err)
	}
}

func TestLoadRefusesABadPluginsSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	for name, c := range map[string]struct{ yaml, want string }{
		"unknown key":    {"plugins:\n  trust_everything: true\n", "trust_everything"},
		"root key id":    {"plugins:\n  publishers:\n    - {keyid: gwroot-9, alg: ed25519, public_key: " + issuerKey + "}\n", "gwroot-"},
		"bad base64":     {"plugins:\n  publishers:\n    - {keyid: acme-1, alg: ed25519, public_key: '***'}\n", "base64"},
		"short key":      {"plugins:\n  publishers:\n    - {keyid: acme-1, alg: ed25519, public_key: AAAA}\n", "acme-1"},
		"unknown alg":    {"plugins:\n  publishers:\n    - {keyid: acme-1, alg: rsa, public_key: " + issuerKey + "}\n", "acme-1"},
		"listed twice":   {"plugins:\n  publishers:\n    - {keyid: acme-1, alg: ed25519, public_key: " + issuerKey + "}\n    - {keyid: acme-1, alg: ed25519, public_key: " + issuerKey + "}\n", "twice"},
		"isolation":      {"plugins:\n  require_isolation: gvisor\n", "require_isolation"},
		"package id":     {"plugins:\n  allow_unsandboxed: [../x]\n", "package id"},
		"publisher name": {"plugins:\n  policy:\n    allow_publishers: [Acme_Corp]\n", "publisher"},
		"unknown policy": {"plugins:\n  policy:\n    allow_all: true\n", "allow_all"},
	} {
		if err := os.WriteFile(path, []byte(c.yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
}
