package runveilcli

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker"
	"gpuworks.ai/gpuworks/test/ocifixture"
)

// catalogRef is the reference the platform's image catalog knows; the archive
// was saved under a mirror's name, as an operator's copy often is.
const (
	catalogRef = "gpuworks/gpuworks-runner:cuda13.0-sglang0.5.22-r1"
	mirrorTag  = "harbor.internal/ai/gpuworks-runner:cuda13.0-sglang0.5.22-r1"
)

func writeArchive(t *testing.T, gzipped bool) string {
	t.Helper()
	archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{RepoTags: []string{mirrorTag}, LayerBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	name := "runner.tar"
	if gzipped {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		must2(zw.Write(archive))
		must(zw.Close())
		archive, name = buf.Bytes(), "runner.tar.gz"
	}
	path := filepath.Join(t.TempDir(), name)
	must(os.WriteFile(path, archive, 0o644))
	return path
}

func must2(_ int, err error) { must(err) }

// loadCLI is a fresh CLI on n's state, as a separate process beside the
// agent sees it: nothing provisioned in this driver instance.
func loadCLI(n *node) (*CLI, *bytes.Buffer, *bytes.Buffer) {
	base := filepath.Dir(n.state)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return &CLI{Driver: udocker.New(base), Stdout: stdout, Stderr: stderr}, stdout, stderr
}

func listed(t *testing.T, d *udocker.Driver, ref string) bool {
	t.Helper()
	images, err := d.Images()
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range images {
		if image.Reference == ref {
			return true
		}
	}
	return false
}

func TestLoadRegistersTheArchiveUnderTheCatalogReference(t *testing.T) {
	for _, gzipped := range []bool{false, true} {
		t.Run(fmt.Sprintf("gzip=%t", gzipped), func(t *testing.T) {
			n := newNode(t)
			cli, stdout, stderr := loadCLI(n)
			path := writeArchive(t, gzipped)
			// The flag may follow the reference, as with docker.
			if code := cli.Execute(context.Background(), []string{"load", catalogRef, "-i", path}); code != 0 {
				t.Fatalf("load: code %d stderr %q", code, stderr)
			}
			if got := stdout.String(); got != "Loaded image: "+catalogRef+"\n" {
				t.Fatalf("stdout = %q", got)
			}
			// The agent's own driver, which answers the platform's presence
			// check, now finds the image under the catalog reference.
			present, err := n.driver.ImageExists(context.Background(), catalogRef)
			if err != nil || !present {
				t.Fatalf("ImageExists(%s) = %v, %v", catalogRef, present, err)
			}
			if listed(t, n.driver, mirrorTag) {
				t.Errorf("the archive's own tag %s was registered too", mirrorTag)
			}
			if _, err := n.driver.InspectImage(context.Background(), catalogRef); err != nil {
				t.Errorf("inspect loaded image: %v", err)
			}
		})
	}
}

func TestLoadUsageErrors(t *testing.T) {
	n := newNode(t)
	path := writeArchive(t, false)
	cases := map[string][]string{
		"no input":       {"load", catalogRef},
		"no reference":   {"load", "-i", path},
		"two references": {"load", "-i", path, catalogRef, "other:1"},
		"unknown flag":   {"load", "-x", path, catalogRef},
	}
	for name, args := range cases {
		cli, stdout, stderr := loadCLI(n)
		if code := cli.Execute(context.Background(), args); code != 2 || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "gpuworks-agent runveil load: ") {
			t.Errorf("%s: code %d stdout %q stderr %q", name, code, stdout, stderr)
		}
	}
	if listed(t, n.driver, catalogRef) {
		t.Fatal("a rejected command registered the image")
	}
}

func TestLoadFailuresLeaveNothingBehind(t *testing.T) {
	n := newNode(t)
	udockerDir, _ := n.driver.StateDirs()
	layers := filepath.Join(udockerDir, "layers")
	before, err := os.ReadDir(layers)
	if err != nil {
		t.Fatal(err)
	}
	truncated := filepath.Join(t.TempDir(), "truncated.tar")
	full, err := os.ReadFile(writeArchive(t, false))
	if err != nil {
		t.Fatal(err)
	}
	must(os.WriteFile(truncated, full[:len(full)/2], 0o644))
	cases := []struct {
		name, ref, path, want string
	}{
		{"missing file", catalogRef, filepath.Join(t.TempDir(), "absent.tar"), "open image archive"},
		{"truncated archive", catalogRef, truncated, "image archive"},
		{"digest reference", "gpuworks/gpuworks-runner@sha256:" + strings.Repeat("a", 64), writeArchive(t, false), "digest image references are not supported"},
	}
	for _, tc := range cases {
		cli, stdout, stderr := loadCLI(n)
		if code := cli.Execute(context.Background(), []string{"load", "-i", tc.path, tc.ref}); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%s: code %d stdout %q stderr %q, want %q", tc.name, code, stdout, stderr, tc.want)
		}
	}
	if listed(t, n.driver, catalogRef) {
		t.Fatal("a failed load registered the image")
	}
	after, err := os.ReadDir(layers)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		var names []string
		for _, entry := range after {
			names = append(names, entry.Name())
		}
		t.Fatalf("layers went from %d to %d entries: %v", len(before), len(after), names)
	}
}

// Writing images is for root or the state's owner: anything else would leave
// files the run user cannot read or replace.
func TestLoadRefusesOtherUsers(t *testing.T) {
	n := newNode(t)
	owner := os.Geteuid()
	path := writeArchive(t, false)
	cli, _, stderr := loadCLI(n)
	cli.Geteuid = func() int { return owner + 1 }
	if code := cli.Execute(context.Background(), []string{"load", "-i", path, catalogRef}); code != 1 || !strings.Contains(stderr.String(), "load needs root (sudo) or the owner of") {
		t.Fatalf("load as another user: code %d stderr %q", code, stderr)
	}
	if listed(t, n.driver, catalogRef) {
		t.Fatal("a refused load registered the image")
	}
	cli, _, stderr = loadCLI(n)
	cli.Geteuid = func() int { return owner }
	if code := cli.Execute(context.Background(), []string{"load", "-i", path, catalogRef}); code != 0 || !listed(t, n.driver, catalogRef) {
		t.Fatalf("load as the owner: code %d stderr %q", code, stderr)
	}
}

func TestLoadWithoutRunveilStateIsExplained(t *testing.T) {
	stderr := &bytes.Buffer{}
	cli := &CLI{Driver: udocker.New(t.TempDir()), Stdout: &bytes.Buffer{}, Stderr: stderr}
	if code := cli.Execute(context.Background(), []string{"load", "-i", "x.tar", catalogRef}); code != 1 || !strings.Contains(stderr.String(), "no runveil state") {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
}
