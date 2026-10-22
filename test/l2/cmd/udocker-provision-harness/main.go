//go:build linux

// udocker-provision-harness exercises the ownership migration boundary that
// cannot be reproduced on the macOS developer host. It deliberately creates a
// legacy root-owned UDOCKER_DIR with many fake rootfs trees, runs the real
// Driver provisioning twice, and records the chown invocations. The first run
// may migrate the legacy tree; a fresh Driver using the same directory must
// reuse the marker and issue no further recursive chown.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker"
)

func main() {
	if os.Geteuid() != 0 {
		fatalf("requires root to create a legacy root-owned UDOCKER_DIR")
	}
	base, err := os.MkdirTemp("", "gpuworks-udocker-provision-")
	if err != nil {
		fatalf("tempdir: %v", err)
	}
	defer os.RemoveAll(base)
	if err := os.Chmod(base, 0o755); err != nil {
		fatalf("chmod base: %v", err)
	}
	legacy := filepath.Join(base, "udocker", "containers")
	for i := 0; i < 62; i++ {
		root := filepath.Join(legacy, fmt.Sprintf("legacy-%02d", i), "ROOT", "etc")
		if err := os.MkdirAll(root, 0o755); err != nil {
			fatalf("create legacy rootfs %d: %v", i, err)
		}
		if err := os.WriteFile(filepath.Join(root, "release"), []byte("legacy\n"), 0o644); err != nil {
			fatalf("write legacy rootfs %d: %v", i, err)
		}
	}
	// The test process is root, so all pre-created files intentionally have
	// uid/gid 0 while provisioning selects the dedicated udocker user.
	logPath := filepath.Join(base, "chown.log")
	bin := filepath.Join(base, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		fatalf("mkdir bin: %v", err)
	}
	wrapper := filepath.Join(bin, "chown")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuote(logPath) + "\nexec /usr/bin/chown \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		fatalf("write chown wrapper: %v", err)
	}
	originalPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", bin+string(os.PathListSeparator)+originalPath); err != nil {
		fatalf("set PATH: %v", err)
	}

	run := func(label string) int {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		driver := udocker.New(base)
		// ImageExists forces the real ensureProvisioned path, but does not pull
		// or create an image. A missing image is expected after provisioning.
		_, _ = driver.ImageExists(ctx, "gpuworks/provisioning-fixture:missing")
		data, readErr := os.ReadFile(logPath)
		if readErr != nil && !os.IsNotExist(readErr) {
			fatalf("%s read chown log: %v", label, readErr)
		}
		return len(strings.Split(strings.TrimSpace(string(data)), "\n"))
	}

	first := run("first")
	if first == 0 {
		fatalf("first provisioning did not migrate the legacy tree")
	}
	second := run("restart")
	if second != first {
		fatalf("chown calls increased after restart: first=%d second=%d", first, second)
	}
	marker := filepath.Join(base, "udocker", ".gpuworks-owner")
	if _, err := os.Stat(marker); err != nil {
		fatalf("ownership marker missing: %v", err)
	}
	fmt.Printf("udocker-provision-harness-ok chown_calls=%d rootfs=%s\n", first, strconv.Itoa(62))
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func fatalf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
