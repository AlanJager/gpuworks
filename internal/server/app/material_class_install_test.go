//go:build linux && dqlite

package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Secrets stay off the target's consumer paths. Benchmark input and detail,
// the SSH host-key file, and a custom agent binary are copied from the
// manifest. A secret class pointed at another location is refused.
func TestMaterialInstallCopiesListedClassesAndKeepsSecretsOffConsumerPaths(t *testing.T) {
	target := t.TempDir()
	replicaRoot := filepath.Join(target, "material-replicas")
	if err := os.Mkdir(replicaRoot, 0700); err != nil {
		t.Fatal(err)
	}
	masterPath := filepath.Join(target, "master.key")
	authorityPath := filepath.Join(target, "cluster-enrollment", "ca.key")
	if err := os.MkdirAll(filepath.Dir(authorityPath), 0700); err != nil {
		t.Fatal(err)
	}
	master := []byte("target-master-secret-bytes")
	authority := []byte("target-own-ca-private-key")
	if err := os.WriteFile(masterPath, master, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authorityPath, authority, 0600); err != nil {
		t.Fatal(err)
	}
	receiver, err := newMaterialReceiver(replicaRoot, 8)
	if err != nil {
		t.Fatal(err)
	}
	refused := materialManifest{SourceMemberID: 7, Files: []materialManifestFile{{
		Class: MaterialMasterSecret, Path: "benchmark-artifacts/master.key", SHA256: materialTestDigest(master), Bytes: int64(len(master)),
	}}}
	if err := materializeReceivedManifest(t.Context(), receiver, refused); err == nil || !strings.Contains(err.Error(), "invalid manifest entry") {
		t.Fatalf("secret class accepted at a benchmark path: %v", err)
	}
	refused.Files[0] = materialManifestFile{Class: MaterialMemberIdentity, Path: "dl/ca.key", SHA256: materialTestDigest([]byte("source-member-ca-private-key")), Bytes: int64(len("source-member-ca-private-key"))}
	if err := materializeReceivedManifest(t.Context(), receiver, refused); err == nil || !strings.Contains(err.Error(), "invalid manifest entry") {
		t.Fatalf("member identity accepted under the agent directory: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(target, "benchmark-artifacts", "master.key")); !os.IsNotExist(err) {
		t.Fatal("refused secret path was created")
	}
	if _, err := os.Lstat(filepath.Join(target, "dl", "ca.key")); !os.IsNotExist(err) {
		t.Fatal("refused identity path was created")
	}

	sourceCA := []byte("source-member-ca-private-key")
	sourceMemberKey := []byte("source-member-private-key")
	benchmarkInput := []byte("benchmark input bytes for class copy")
	benchmarkDetail := []byte("benchmark detail bytes for class copy")
	hostKeys := []byte("ssh host key bytes for class copy")
	agent := []byte("custom agent binary bytes for class copy")
	files := []materialManifestFile{
		manifestFile(MaterialAgentOverride, "dl/gpuworks-agent-linux-amd64", agent),
		manifestFile(MaterialBenchmarkDetail, "benchmark-artifacts/detail.jsonl.gz", benchmarkDetail),
		manifestFile(MaterialBenchmarkInput, "benchmark-artifacts/tokenizer-class.json", benchmarkInput),
		manifestFile(MaterialMasterSecret, "master.key", master),
		manifestFile(MaterialMemberIdentity, "cluster-enrollment/ca.key", sourceCA),
		manifestFile(MaterialMemberIdentity, "gpuworks.db.join/member.key", sourceMemberKey),
		manifestFile(MaterialSSHHostKeys, "ssh-hostkeys.json", hostKeys),
	}
	for _, file := range files {
		body := bodyForManifest(file, master, sourceCA, sourceMemberKey, benchmarkInput, benchmarkDetail, hostKeys, agent)
		if _, err := receiver.receive(t.Context(), file.SHA256, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	masterInfo, err := os.Stat(masterPath)
	if err != nil {
		t.Fatal(err)
	}
	authorityInfo, err := os.Stat(authorityPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest := materialManifest{SourceMemberID: 7, Files: files}
	if err := materializeReceivedManifest(t.Context(), receiver, manifest); err != nil {
		t.Fatal(err)
	}
	if err := checkInstalledMaterialManifest(t.Context(), receiver, manifest); err != nil {
		t.Fatal(err)
	}
	masterAfter, err := os.Stat(masterPath)
	if err != nil || !os.SameFile(masterInfo, masterAfter) {
		t.Fatal("target master key was replaced")
	}
	if got, err := os.ReadFile(masterPath); err != nil || !bytes.Equal(got, master) {
		t.Fatalf("target master key bytes=%q %v", got, err)
	}
	authorityAfter, err := os.Stat(authorityPath)
	if err != nil || !os.SameFile(authorityInfo, authorityAfter) {
		t.Fatal("target certificate authority was replaced")
	}
	if got, err := os.ReadFile(authorityPath); err != nil || !bytes.Equal(got, authority) {
		t.Fatalf("target authority bytes=%q %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(target, "gpuworks.db.join", "member.key")); !os.IsNotExist(err) {
		t.Fatal("source member key was installed")
	}
	for path, want := range map[string][]byte{
		"benchmark-artifacts/tokenizer-class.json": benchmarkInput,
		"benchmark-artifacts/detail.jsonl.gz":      benchmarkDetail,
		"ssh-hostkeys.json":                        hostKeys,
		"dl/gpuworks-agent-linux-amd64":            agent,
	} {
		got, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(path)))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s=%q %v", path, got, err)
		}
	}
	if err := filepath.WalkDir(target, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entry.Name() == "material-replicas" {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, secret := range [][]byte{sourceCA, sourceMemberKey} {
			if bytes.Contains(body, secret) {
				t.Fatalf("source secret installed at %s", path)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func manifestFile(class MaterialClass, path string, body []byte) materialManifestFile {
	return materialManifestFile{Class: class, Path: path, SHA256: materialTestDigest(body), Bytes: int64(len(body))}
}

func bodyForManifest(file materialManifestFile, master, sourceCA, sourceMemberKey, benchmarkInput, benchmarkDetail, hostKeys, agent []byte) []byte {
	switch file.Path {
	case "master.key":
		return master
	case "cluster-enrollment/ca.key":
		return sourceCA
	case "gpuworks.db.join/member.key":
		return sourceMemberKey
	case "benchmark-artifacts/tokenizer-class.json":
		return benchmarkInput
	case "benchmark-artifacts/detail.jsonl.gz":
		return benchmarkDetail
	case "ssh-hostkeys.json":
		return hostKeys
	case "dl/gpuworks-agent-linux-amd64":
		return agent
	default:
		return nil
	}
}
