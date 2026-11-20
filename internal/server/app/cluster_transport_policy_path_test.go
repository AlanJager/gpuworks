package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

func policyPathJoinedFixture(t *testing.T) (Config, ClusterJoinInfo) {
	t.Helper()
	data := t.TempDir()
	join := filepath.Join(data, "gpuworks.db.join")
	if err := os.Mkdir(join, 0700); err != nil {
		t.Fatal(err)
	}
	key, err := identity.CreateMemberKey(join + "/member.key")
	if err != nil {
		t.Fatal(err)
	}
	csr, _ := identity.NewMemberCSR(key)
	fingerprint, _ := identity.CSRKeyFingerprint(csr)
	info := ClusterJoinInfo{ClusterJoinTarget: ClusterJoinTarget{ClusterID: "00112233445566778899aabbccddeeff", CAFingerprint: strings.Repeat("a", 64), Protocol: proto.ClusterProtocolVersion, Address: "192.0.2.2:9001", EnrollmentURL: "https://192.0.2.1:9443/api/v1/cluster/enrollment/certificate", EnrollmentOrigin: "https://192.0.2.2:9443", EnrollmentListen: "192.0.2.2:9443"}, KeyFingerprint: fingerprint}
	raw, _ := json.Marshal(info)
	if err := os.WriteFile(join+"/config.json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	return Config{DataDir: data, DBPath: data + "/gpuworks.db"}, info
}

func TestClusterTransportPolicyPathFollowsPersistedStartupIdentity(t *testing.T) {
	cfg, info := policyPathJoinedFixture(t)
	path, got, err := ClusterTransportPolicyPath(cfg)
	if err != nil || path != cfg.DBPath+".join/transport-policy.json" || got == nil || *got != info {
		t.Fatalf("joined startup path/identity differs: %s %#v %v", path, got, err)
	}
	// Selection does not probe policy existence or choose a different source.
	// A resumed joined server may have no seed transport directory at all.
	if _, err := os.Stat(cfg.DBPath + ".dqlite/.transport"); !os.IsNotExist(err) {
		t.Fatal("fixture unexpectedly created seed transport")
	}
	seed := Config{DataDir: t.TempDir()}
	path, got, err = ClusterTransportPolicyPath(seed)
	if err != nil || got != nil || path != seed.DataDir+"/gpuworks.db.dqlite/.transport/policy.json" {
		t.Fatalf("seed path differs: %s %#v %v", path, got, err)
	}
}

func TestClusterTransportPolicyPathRefusesDamagedJoinMetadataWithoutSeedFallback(t *testing.T) {
	for _, failure := range []string{"symlink", "nonprivate", "missing-config", "config-symlink", "config-public", "unknown-config", "trailing-config", "key-mismatch", "missing-key", "outside-data"} {
		t.Run(failure, func(t *testing.T) {
			cfg, _ := policyPathJoinedFixture(t)
			join := cfg.DBPath + ".join"
			// The alternate seed path exists, but invalid joined metadata is fatal.
			if err := os.MkdirAll(cfg.DBPath+".dqlite/.transport", 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfg.DBPath+".dqlite/.transport/policy.json", []byte(`{}`), 0600); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "symlink":
				moved := join + "-actual"
				if err := os.Rename(join, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, join); err != nil {
					t.Fatal(err)
				}
			case "nonprivate":
				if err := os.Chmod(join, 0755); err != nil {
					t.Fatal(err)
				}
			case "missing-config":
				if err := os.Remove(join + "/config.json"); err != nil {
					t.Fatal(err)
				}
			case "config-symlink":
				actual := join + "/config-actual.json"
				if err := os.Rename(join+"/config.json", actual); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(actual, join+"/config.json"); err != nil {
					t.Fatal(err)
				}
			case "config-public":
				if err := os.Chmod(join+"/config.json", 0644); err != nil {
					t.Fatal(err)
				}
			case "unknown-config":
				if err := os.WriteFile(join+"/config.json", []byte(`{"unknown":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "trailing-config":
				f, err := os.OpenFile(join+"/config.json", os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = f.WriteString(` {}`)
				f.Close()
			case "key-mismatch":
				key, _ := identity.NewMemberKey()
				if err := os.WriteFile(join+"/member.key", key, 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-key":
				if err := os.Remove(join + "/member.key"); err != nil {
					t.Fatal(err)
				}
			case "outside-data":
				cfg.DataDir = t.TempDir()
			}
			if path, info, err := ClusterTransportPolicyPath(cfg); err == nil || path != "" || info != nil {
				t.Fatalf("invalid metadata fell back or selected a usable policy: %s %#v %v", path, info, err)
			}
		})
	}
}
