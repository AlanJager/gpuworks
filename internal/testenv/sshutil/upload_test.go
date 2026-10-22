package sshutil

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAtomicUploadCommandUsesSiblingAndAtomicRename(t *testing.T) {
	command, err := atomicUploadCommand("/tmp/gpuworks hosted/bin/server", 0o755, 42)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"mkdir -p '/tmp/gpuworks hosted/bin'",
		"cat > '/tmp/gpuworks hosted/bin/server.upload-42'",
		"chmod 0755 '/tmp/gpuworks hosted/bin/server.upload-42'",
		"mv -f '/tmp/gpuworks hosted/bin/server.upload-42' '/tmp/gpuworks hosted/bin/server'",
	} {
		if !strings.Contains(command, required) {
			t.Fatalf("upload command missing %q: %s", required, command)
		}
	}
}

func TestAtomicUploadCommandRejectsUnsafeDestination(t *testing.T) {
	for _, destination := range []string{"", ".", "relative/file", "/"} {
		if command, err := atomicUploadCommand(destination, os.FileMode(0o755), 1); err == nil {
			t.Fatalf("destination %q produced unsafe command %q", destination, command)
		}
	}
}

func TestUploadFingerprintFileRewindsSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server")
	data := []byte("gpuworks-hosted-server")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fingerprint, err := fingerprintFile(file, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkUploadFingerprint("22 c50cc7cb66da55deda8d74f1234c19cc34e54193631fd4cd04da1a2982c73d7b 755\n", fingerprint); err != nil {
		t.Fatal(err)
	}
	var reread bytes.Buffer
	if _, err := reread.ReadFrom(file); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reread.Bytes(), data) {
		t.Fatalf("fingerprinting left upload source at the wrong offset: %q", reread.Bytes())
	}
}

func TestUploadVerificationCommandAndFingerprint(t *testing.T) {
	command, err := uploadVerificationCommand("/tmp/gpuworks hosted/bin/server")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"test -f '/tmp/gpuworks hosted/bin/server'",
		"wc -c < '/tmp/gpuworks hosted/bin/server'",
		"sha256sum '/tmp/gpuworks hosted/bin/server'",
		"stat -c %a '/tmp/gpuworks hosted/bin/server'",
	} {
		if !strings.Contains(command, required) {
			t.Fatalf("verification command missing %q: %s", required, command)
		}
	}
	want := uploadFingerprint{size: 12, sha256: strings.Repeat("a", 64), mode: 0o700}
	if err := checkUploadFingerprint("12 "+strings.Repeat("a", 64)+" 700\n", want); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{
		"11 " + strings.Repeat("a", 64) + " 700",
		"12 " + strings.Repeat("b", 64) + " 700",
		"12 " + strings.Repeat("a", 64) + " 600",
		"garbage",
	} {
		if err := checkUploadFingerprint(output, want); err == nil {
			t.Fatalf("mismatched remote fingerprint %q was accepted", output)
		}
	}
}
