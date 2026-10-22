package serverconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSecurityConfigurationLoadsAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	content := "tls_cert_file: /etc/gpuworks/server.crt\ntls_key_file: /etc/gpuworks/server.key\noutbound_allow_private_hosts: [models.example.internal, 10.20.0.0/24]\nstorage_probe_roots: [/srv/models]\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("security configuration unavailable: %v", err)
	}
	if file.TLSCertFile != "/etc/gpuworks/server.crt" || file.TLSKeyFile != "/etc/gpuworks/server.key" || !reflect.DeepEqual(file.OutboundAllowPrivateHosts, []string{"models.example.internal", "10.20.0.0/24"}) || !reflect.DeepEqual(file.StorageProbeRoots, []string{"/srv/models"}) {
		t.Fatalf("security policy lost on load: %+v", file)
	}
	if err := Write(path, file, 0o600); err != nil {
		t.Fatal(err)
	}
	if again, err := Load(path); err != nil || !reflect.DeepEqual(file, again) {
		t.Fatalf("security policy lost on round trip: %+v %v", again, err)
	}
}

func TestStorageProbeRootConfigurationRejectsUnboundedOrRelativePaths(t *testing.T) {
	for _, root := range []string{"/", "../models", "relative"} {
		path := filepath.Join(t.TempDir(), FileName)
		if err := os.WriteFile(path, []byte("storage_probe_roots: [\""+root+"\"]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("unbounded storage root accepted: %s", root)
		}
	}
}

func TestTLSConfigurationRequiresBothFiles(t *testing.T) {
	for _, content := range []string{"tls_cert_file: server.crt\n", "tls_key_file: server.key\n"} {
		path := filepath.Join(t.TempDir(), FileName)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted incomplete TLS configuration: %s", content)
		}
	}
}
