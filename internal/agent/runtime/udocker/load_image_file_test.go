package udocker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/test/ocifixture"
)

// TestLoadImageFileLoadsInPlaceWithoutASecondCopy pins the reason
// ImageFileLoader exists: udocker's reader-based LoadImage stages the stream to
// a temporary file first, so a node that already downloaded a multi-gigabyte
// archive would briefly hold three copies of it. Loading by path holds one.
func TestLoadImageFileLoadsInPlaceWithoutASecondCopy(t *testing.T) {
	// This pins the udocker CLI path, used only when the native pipeline is
	// switched off; the native loader is covered by the tests below.
	t.Setenv(disableNativeEnv, "1")
	base := t.TempDir()
	argsLog := filepath.Join(base, "args")
	archive := filepath.Join(base, "downloaded.tar")
	if err := os.WriteFile(archive, []byte("docker-save-archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(base, "udocker-stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> '"+argsLog+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := New(base)
	d.provisioned = true
	d.udockerExe = stub
	d.runUser = ""
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := d.LoadImageFile(context.Background(), "gpuworks/partition-probe:v1", archive); err != nil {
		t.Fatalf("LoadImageFile: %v", err)
	}

	args, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	// udocker is pointed at the downloaded file itself and told the repository
	// explicitly, which is why it needs no tag step afterwards.
	if !strings.Contains(string(args), "load -i="+archive+" gpuworks/partition-probe") {
		t.Fatalf("udocker args = %q", args)
	}
	entries, err := os.ReadDir(d.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "image-load-") {
			t.Fatalf("a staged copy was made: %s", entry.Name())
		}
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("the caller's archive must survive the load: %v", err)
	}
}

func TestLoadImageFileRejectsMissingArguments(t *testing.T) {
	d := New(t.TempDir())
	d.provisioned = true
	if err := d.LoadImageFile(context.Background(), "", "/tmp/x.tar"); err == nil {
		t.Error("a missing image reference must be refused")
	}
	if err := d.LoadImageFile(context.Background(), "gpuworks/probe:v1", "   "); err == nil {
		t.Error("a missing archive path must be refused")
	}
}

func TestLoadImageNativelyRegistersTheCanonicalReference(t *testing.T) {
	const canonical = "registry.example/team/runner:v1"
	archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{RepoTags: []string{"baked/name:other"}, LayerBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	for name, load := range map[string]func(*Driver) error{
		"file": func(d *Driver) error {
			path := filepath.Join(t.TempDir(), "downloaded.tar")
			if err := os.WriteFile(path, archive, 0o644); err != nil {
				t.Fatal(err)
			}
			return d.LoadImageFile(context.Background(), canonical, path)
		},
		"stream": func(d *Driver) error {
			return d.LoadImage(context.Background(), canonical, bytes.NewReader(archive))
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := cliFreeDriver(t) // any udocker CLI call fails
			if err := load(d); err != nil {
				t.Fatalf("native load: %v", err)
			}
			if ok, err := d.ImageExists(context.Background(), canonical); err != nil || !ok {
				t.Fatalf("canonical reference not registered: %v %v", ok, err)
			}
			inspection, err := d.InspectImage(context.Background(), canonical)
			if err != nil || len(inspection.Entrypoint) != 1 || inspection.Entrypoint[0] != "/entrypoint.sh" {
				t.Fatalf("inspection %+v %v", inspection, err)
			}
			if handled, err := d.nativeCreate(context.Background(), "c", canonical, nil); !handled || err != nil {
				t.Fatalf("a loaded image must be creatable natively: handled=%v err=%v", handled, err)
			}
			if _, err := os.Stat(filepath.Join(d.udockerDir, "containers", "c", "ROOT", "etc", "gpuworks-fixture")); err != nil {
				t.Fatal(err)
			}
			entries, _ := os.ReadDir(d.stateDir)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "image-load-") {
					t.Fatalf("native load staged a copy: %s", entry.Name())
				}
			}
		})
	}
}

func TestLoadImageNativeRejectsCorruptArchiveWithoutCLI(t *testing.T) {
	d := cliFreeDriver(t)
	err := d.LoadImage(context.Background(), "a/b:c", strings.NewReader("definitely not a tar archive, but long enough to be read as one............................................................................................................................................................................................................................................................................................................................................................................................................................................................................................................................"))
	if err == nil || strings.Contains(err.Error(), cliUnavailable) {
		t.Fatalf("corrupt archive: %v", err)
	}
	if ok, _ := d.ImageExists(context.Background(), "a/b:c"); ok {
		t.Fatal("corrupt archive registered an image")
	}
}
