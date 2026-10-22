package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/expose"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

func TestUninstallExposeRejectsUnsafeStateWithoutMutation(t *testing.T) {
	for _, name := range []string{".lock", ".supervisor.lock", "config.json", "logs", "credentials", "bin", "data", "ancestor"} {
		t.Run(name, func(t *testing.T) {
			layout := selfinstall.NewLayout(t.TempDir(), "", "")
			store := expose.NewStore(layout.DataDir)
			if err := store.WriteConfig(&expose.Config{Revision: "before", Provider: expose.ProviderQuick, Desired: expose.DesiredEnabled}); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(store.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "private")
			var targetMode os.FileMode = 0o600
			if name == "logs" || name == "credentials" || name == "bin" {
				targetMode = 0o700
				if err := os.Mkdir(target, targetMode); err != nil {
					t.Fatal(err)
				}
			} else if name == "data" || name == "ancestor" {
				targetMode = 0o700
				source := layout.DataDir
				if name == "ancestor" {
					source = layout.Dir
				}
				if err := os.Rename(source, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(target, targetMode); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, source); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(target, []byte("private secret"), targetMode); err != nil {
				t.Fatal(err)
			}
			if name != "data" && name != "ancestor" {
				path := filepath.Join(store.Root(), name)
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			streams, _ := uninstallStreams("", false)
			runner := new(uninstallRecordingRunner)
			if _, err := uninstallExpose(context.Background(), layout, runner, streams, t.TempDir()); err == nil {
				t.Error("unsafe state accepted")
			}
			info, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != targetMode {
				t.Errorf("target mode changed: %o want %o", info.Mode().Perm(), targetMode)
			}
			if !info.IsDir() {
				raw, err := os.ReadFile(target)
				if err != nil || string(raw) != "private secret" {
					t.Errorf("target content changed: %q %v", raw, err)
				}
			}
			if name != "config.json" {
				after, err := os.ReadFile(store.ConfigPath())
				if err != nil || !bytes.Equal(before, after) {
					t.Errorf("configuration changed: %s %v", after, err)
				}
			}
			if len(runner.commands) != 0 {
				t.Errorf("service changed: %v", runner.commands)
			}
		})
	}
}
