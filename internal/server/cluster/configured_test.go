package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

type configuredFixture struct {
	dir, address, id, pin string
	ca, cert, key         []byte
	policy                identity.TransportPolicy
}

func newConfiguredFixture(t *testing.T) configuredFixture {
	t.Helper()
	f := configuredFixture{dir: t.TempDir(), address: "127.0.0.1:19877", id: strings.Repeat("a", 32)}
	if err := os.Chmod(f.dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	var err error
	var caKey []byte
	f.ca, caKey, err = identity.NewAuthority(f.id, now)
	if err != nil {
		t.Fatal(err)
	}
	f.pin, err = identity.CertificateFingerprint(f.ca)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := identity.LoadAuthority(f.ca, caKey, f.id, f.pin, now)
	if err != nil {
		t.Fatal(err)
	}
	f.key, err = identity.NewMemberKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := identity.NewMemberCSR(f.key)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := identity.CSRKeyFingerprint(csr)
	if err != nil {
		t.Fatal(err)
	}
	f.cert, err = authority.IssueMember(csr, fingerprint, f.address, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.policy = identity.TransportPolicy{ClusterID: f.id, CAFingerprint: f.pin, Revision: 1, AllowedKeys: []string{fingerprint}}
	return f
}
func (f configuredFixture) prepare() error {
	return PrepareDatabaseTransport(f.dir, f.address, f.id, f.pin, f.ca, f.cert, f.key, f.policy)
}
func TestConfiguredTransportPublication(t *testing.T) {
	f := newConfiguredFixture(t)
	if err := f.prepare(); err != nil {
		t.Fatal(err)
	}
	if err := f.prepare(); err != nil {
		t.Fatal("retry", err)
	}
	for _, name := range []string{"config.json", "ca.pem", "member.pem", "member.key", "policy.json"} {
		st, err := os.Lstat(filepath.Join(f.dir, ".transport", name))
		if err != nil || st.Mode().Perm() != 0600 {
			t.Fatalf("%s: %v %v", name, st, err)
		}
	}
	original := f.key
	f.key = []byte("damaged")
	if err := f.prepare(); err == nil {
		t.Fatal("invalid key accepted")
	}
	f.key = original
	if _, err := OpenConfiguredDatabase(context.Background(), f.dir, "test.db"); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing Raft identity did not fail before opening: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "info.yaml"), []byte("ID: 1\nAddress: "+f.address+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "cluster.yaml"), []byte("- ID: 1\n  Address: "+f.address+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Canceled context is checked only after loading and validating the bundle,
	// exercising the default and native reader without bootstrapping fake Raft.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := OpenConfiguredDatabase(ctx, f.dir, "test.db"); !errors.Is(err, context.Canceled) {
		t.Fatalf("valid configuration was not selected: %v", err)
	}
}
func TestConfiguredTransportInterruptedPublication(t *testing.T) {
	f := newConfiguredFixture(t)
	if err := f.prepare(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.dir, ".transport")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenConfiguredDatabase(context.Background(), f.dir, "test.db"); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("incomplete intent fell back: %v", err)
	}
	other := f
	other.address = "127.0.0.1:19878"
	if err := other.prepare(); err == nil {
		t.Fatal("different interrupted intent accepted")
	}
	if err := f.prepare(); err != nil {
		t.Fatal("same intent retry", err)
	}
}
func TestConfiguredTransportRejectsDamage(t *testing.T) {
	for _, name := range []string{"missing marker", "symlink marker", "symlink key", "public key", "corrupt config", "missing policy", "public directory", "symlink directory", "missing bundle"} {
		t.Run(name, func(t *testing.T) {
			f := newConfiguredFixture(t)
			if err := f.prepare(); err != nil {
				t.Fatal(err)
			}
			bundle := filepath.Join(f.dir, ".transport")
			marker := filepath.Join(f.dir, ".transport-required")
			var err error
			switch name {
			case "missing marker":
				err = os.Remove(marker)
			case "symlink marker":
				err = os.Rename(marker, marker+".real")
				if err == nil {
					err = os.Symlink(marker+".real", marker)
				}
			case "symlink key":
				path := filepath.Join(bundle, "member.key")
				err = os.Rename(path, path+".real")
				if err == nil {
					err = os.Symlink(path+".real", path)
				}
			case "public key":
				err = os.Chmod(filepath.Join(bundle, "member.key"), 0644)
			case "corrupt config":
				err = os.WriteFile(filepath.Join(bundle, "config.json"), []byte("{}"), 0600)
			case "missing policy":
				err = os.Remove(filepath.Join(bundle, "policy.json"))
			case "public directory":
				err = os.Chmod(bundle, 0755)
			case "symlink directory":
				err = os.Rename(bundle, bundle+".real")
				if err == nil {
					err = os.Symlink(bundle+".real", bundle)
				}
			case "missing bundle":
				err = os.RemoveAll(bundle)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := OpenConfiguredDatabase(context.Background(), f.dir, "test.db"); err == nil || errors.Is(err, ErrUnavailable) {
				t.Fatalf("damaged configuration accepted: %v", err)
			}
			if name != "missing bundle" {
				if err := f.prepare(); err == nil {
					t.Fatal("damaged bundle silently repaired")
				}
			}
		})
	}
}
func TestConfiguredTransportValidationBeforeIntent(t *testing.T) {
	for _, name := range []string{"address", "SAN", "policy", "revision", "pin", "private directory"} {
		t.Run(name, func(t *testing.T) {
			f := newConfiguredFixture(t)
			switch name {
			case "address":
				f.address = "0.0.0.0:10"
			case "SAN":
				f.address = "192.0.2.1:10"
			case "policy":
				f.policy.AllowedKeys = []string{strings.Repeat("b", 64)}
			case "revision":
				f.policy.Revision = 2
			case "pin":
				f.pin = strings.Repeat("0", 64)
			case "private directory":
				if err := os.Chmod(f.dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.prepare(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
			if _, err := os.Lstat(filepath.Join(f.dir, ".transport-required")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid input published intent: %v", err)
			}
		})
	}
}

func TestConfiguredTransportPolicyRevisionAndRevocation(t *testing.T) {
	f := newConfiguredFixture(t)
	if err := f.prepare(); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"info.yaml": "ID: 1\nAddress: " + f.address + "\n", "cluster.yaml": "- ID: 1\n  Address: " + f.address + "\n"} {
		if err := os.WriteFile(filepath.Join(f.dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(f.dir, ".transport", "policy.json")
	next := f.policy
	next.Revision = 2
	next.SourceRevision = 41
	if err := identity.PublishTransportPolicy(path, 1, next); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancel()
	if _, err := OpenConfiguredDatabase(ctx, f.dir, "test.db"); !errors.Is(err, context.Canceled) {
		t.Fatalf("updated policy rejected: %v", err)
	}
	if err := f.prepare(); err == nil {
		t.Fatal("initialization reverted newer authorization")
	}
	revoked := next
	revoked.Revision = 3
	revoked.SourceRevision = 49
	revoked.RevokedKeys = append([]string(nil), next.AllowedKeys...)
	revoked.AllowedKeys = []string{strings.Repeat("b", 64)}
	if err := identity.PublishTransportPolicy(path, 2, revoked); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenConfiguredDatabase(ctx, f.dir, "test.db"); err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("revoked member reached database startup: %v", err)
	}
}
