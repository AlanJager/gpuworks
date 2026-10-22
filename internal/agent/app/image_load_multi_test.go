package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	"gpuworks.ai/gpuworks/internal/agent/runtime/nerdctl"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
)

func TestArchiveLoadKeepsRequestedEntryFromMultiImageOutput(t *testing.T) {
	const canonical, other = "catalog/runner:latest", "publisher/other:new"
	for _, cli := range []string{"docker", "nerdctl"} {
		for _, file := range []bool{false, true} {
			for _, first := range []bool{true, false} {
				name := cli + "/stream"
				if file {
					name = cli + "/file"
				}
				if first {
					name += "/canonical-first"
				} else {
					name += "/canonical-last"
				}
				t.Run(name, func(t *testing.T) {
					dir := t.TempDir()
					refs := []string{canonical, other}
					if !first {
						refs[0], refs[1] = refs[1], refs[0]
					}
					var lines []string
					for _, ref := range refs {
						line := "Loaded image: " + ref
						if cli == "nerdctl" {
							line = "unpacking " + ref + " (sha256:abcd)..."
						}
						lines = append(lines, "echo '"+line+"'")
					}
					check := "#!/bin/sh\nset -eu\n"
					if cli == "nerdctl" {
						check += "[ \"$1\" = '-n' ] && [ \"$2\" = 'gpuworks' ]\nshift 2\n"
					}
					check += "[ \"$1\" = 'load' ]\nshift\nif [ \"$#\" -eq 0 ]; then cat >/dev/null; else [ \"$#\" -eq 2 ] && [ \"$1\" = '-i' ]; cat \"$2\" >/dev/null; fi\n"
					if err := os.WriteFile(filepath.Join(dir, "archive.tar"), []byte("multi-image archive"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, cli), []byte(check+strings.Join(lines, "\n")+"\n"), 0700); err != nil {
						t.Fatal(err)
					}
					t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
					var loader runtime.StreamedRefReporter = docker.New()
					if cli == "nerdctl" {
						loader = nerdctl.New()
					}
					d := &archiveIdentityDriver{Driver: recording.New(filepath.Join(dir, "spec.json")), ids: map[string]string{canonical: "old-A"}}
					status, err := loadImageArchive(context.Background(), d, imageArchiveLoad{CanonicalRef: canonical, RuntimeName: cli, Load: func(ctx context.Context) (string, error) {
						// A real multi-image load imports both identities before reporting each reference.
						d.ids[canonical], d.ids[other] = "new-A", "new-B"
						if file {
							return loader.(runtime.LoadedRefReporter).LoadImageFileRef(ctx, canonical, filepath.Join(dir, "archive.tar"))
						}
						return loader.LoadImageRef(ctx, canonical, strings.NewReader("multi-image archive"))
					}})
					if err != nil || status.State != "present" || d.ids[canonical] != "new-A" || status.ImageID != "new-A" || d.ids[other] != "new-B" {
						t.Fatalf("load changed requested image identity: status=%+v error=%v images=%v", status, err, d.ids)
					}
					if d.tagCalls != 0 {
						t.Fatalf("retagged an image already imported under the requested reference: %d", d.tagCalls)
					}
				})
			}
		}
	}
}

func TestArchiveLoadDoesNotConfuseRegistryEndpointWithCanonicalName(t *testing.T) {
	const canonical = "docker.io/library/runner:latest"
	const loaded = "registry-1.docker.io/library/runner:latest"
	d := &archiveIdentityDriver{Driver: recording.New(filepath.Join(t.TempDir(), "spec.json")), ids: map[string]string{canonical: "old-A"}}
	status, err := loadImageArchive(context.Background(), d, imageArchiveLoad{CanonicalRef: canonical, RuntimeName: "docker", Load: func(context.Context) (string, error) {
		d.ids[loaded] = "new-B"
		return runtime.ParseLoadedImageRefFor("Loaded image: "+loaded+"\n", canonical), nil
	}})
	if err != nil || status.State != "present" || status.ImageID != "new-B" || d.ids[canonical] != "new-B" || d.tagCalls != 1 {
		t.Fatalf("separate registry name bypassed replacement: status=%+v err=%v images=%v tags=%d", status, err, d.ids, d.tagCalls)
	}
}
