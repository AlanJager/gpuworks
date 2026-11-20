//go:build linux

package packages

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

type packageRootObservedContext struct {
	context.Context
	observe func()
}

func (c packageRootObservedContext) Err() error {
	c.observe()
	return c.Context.Err()
}

func packageRootFixture(t *testing.T) (*os.Root, string) {
	t.Helper()
	name := filepath.Join(t.TempDir(), "packages")
	if err := os.Mkdir(name, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	return root, name
}

func packageRootStage(t *testing.T, root *os.Root) string {
	t.Helper()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".package-") {
			return entry.Name()
		}
	}
	return ""
}

func TestPackageRootContextHealthyDigestProjectionAndNoop(t *testing.T) {
	root, _ := packageRootFixture(t)
	source := durablePackageFixture(t)
	want, err := inferencecatalog.LoadPackage(source)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := inferencecatalog.PackageDigest(source)
	if err != nil {
		t.Fatal(err)
	}
	digest, published, err := PutRootContext(t.Context(), root, source)
	if err != nil || !published || digest != expected {
		t.Fatalf("publish=%s/%v/%v", digest, published, err)
	}
	loaded, err := LoadRootContext(t.Context(), root, digest)
	if err != nil || !reflect.DeepEqual(loaded, want) {
		t.Fatalf("projection differs: %+v/%v", loaded, err)
	}
	before, err := root.Lstat(digest)
	if err != nil {
		t.Fatal(err)
	}
	again, published, err := PutRootContext(t.Context(), root, source)
	after, statErr := root.Lstat(digest)
	if err != nil || statErr != nil || published || again != digest || !os.SameFile(before, after) {
		t.Fatalf("noop=%s/%v/%v/%v", again, published, err, statErr)
	}
	if err := root.WriteFile(digest+"/models/org/model/README.en.md", []byte("other valid bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRootContext(t.Context(), root, digest); err == nil {
		t.Fatal("corrupt digest loaded")
	}
	if _, published, err := PutRootContext(t.Context(), root, source); err == nil || published {
		t.Fatalf("corrupt existing digest accepted: %v/%v", published, err)
	}
	if stage := packageRootStage(t, root); stage != "" {
		t.Fatal("noop/corrupt check staged", stage)
	}
}

func TestPackageRootContextRenameKeepsOriginalRoot(t *testing.T) {
	root, name := packageRootFixture(t)
	swapped := false
	ctx := packageRootObservedContext{Context: t.Context(), observe: func() {
		if swapped || packageRootStage(t, root) == "" {
			return
		}
		swapped = true
		if err := os.Rename(name, name+"-held"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(name, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(name, "foreign"), []byte("keep foreign bytes"), 0600); err != nil {
			t.Fatal(err)
		}
	}}
	digest, published, err := PutRootContext(ctx, root, durablePackageFixture(t))
	if err != nil || !published || !swapped {
		t.Fatalf("rename publish=%s/%v/%v swapped=%v", digest, published, err, swapped)
	}
	if _, err := os.Stat(filepath.Join(name+"-held", digest)); err != nil {
		t.Fatal("held root missing package", err)
	}
	entries, err := os.ReadDir(name)
	data, readErr := os.ReadFile(filepath.Join(name, "foreign"))
	if err != nil || readErr != nil || len(entries) != 1 || string(data) != "keep foreign bytes" {
		t.Fatalf("borrowed replacement=%v/%s/%v/%v", entries, data, err, readErr)
	}
	if _, err := LoadRootContext(t.Context(), root, digest); err != nil {
		t.Fatal("held root load", err)
	}
}

func TestPackageRootContextRejectsCompetingEmptyDigest(t *testing.T) {
	root, _ := packageRootFixture(t)
	source := durablePackageFixture(t)
	digest, err := inferencecatalog.PackageDigest(source)
	if err != nil {
		t.Fatal(err)
	}
	var foreign fs.FileInfo
	ctx := packageRootObservedContext{Context: t.Context(), observe: func() {
		if foreign != nil || packageRootStage(t, root) == "" {
			return
		}
		if err := root.Mkdir(digest, 0700); err != nil {
			t.Fatal(err)
		}
		var err error
		foreign, err = root.Lstat(digest)
		if err != nil {
			t.Fatal(err)
		}
	}}
	_, published, err := PutRootContext(ctx, root, source)
	after, statErr := root.Lstat(digest)
	if err == nil || published || foreign == nil || statErr != nil || !os.SameFile(foreign, after) {
		t.Fatalf("foreign directory overwritten: %v/%v/%v", published, err, statErr)
	}
	entries, err := fs.ReadDir(root.FS(), digest)
	if err != nil || len(entries) != 0 || packageRootStage(t, root) != "" {
		t.Fatalf("foreign/stage changed: %v/%v", entries, err)
	}
}

func TestPackageRootContextReplacedStagePreservesForeign(t *testing.T) {
	root, _ := packageRootFixture(t)
	stage := ""
	ctx := packageRootObservedContext{Context: t.Context(), observe: func() {
		if stage != "" {
			return
		}
		stage = packageRootStage(t, root)
		if stage == "" {
			return
		}
		if err := root.Rename(stage, "original-stage"); err != nil {
			t.Fatal(err)
		}
		if err := root.Mkdir(stage, 0700); err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile(stage+"/foreign", []byte("foreign sentinel"), 0600); err != nil {
			t.Fatal(err)
		}
	}}
	_, published, err := PutRootContext(ctx, root, durablePackageFixture(t))
	data, readErr := root.ReadFile(stage + "/foreign")
	if err == nil || published || stage == "" || readErr != nil || string(data) != "foreign sentinel" {
		t.Fatalf("replacement stage lost: %v/%v/%s/%v", published, err, data, readErr)
	}
	entries, listErr := fs.ReadDir(root.FS(), "original-stage")
	if listErr != nil || len(entries) != 0 {
		t.Fatalf("unreachable original stage mutated: %v/%v", entries, listErr)
	}
}

func TestPackageRootContextRejectsDigestAndNestedSymlinks(t *testing.T) {
	for _, kind := range []string{"digest", "nested"} {
		t.Run(kind, func(t *testing.T) {
			root, _ := packageRootFixture(t)
			source := durablePackageFixture(t)
			digest, _, err := PutRootContext(t.Context(), root, source)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "digest" {
				if err := root.Rename(digest, "other"); err != nil {
					t.Fatal(err)
				}
				if err := root.Symlink("other", digest); err != nil {
					t.Fatal(err)
				}
			} else {
				file := digest + "/models/org/model/README.en.md"
				if err := root.Rename(file, digest+"/models/org/model/other"); err != nil {
					t.Fatal(err)
				}
				if err := root.Symlink("other", file); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := LoadRootContext(t.Context(), root, digest); err == nil {
				t.Fatal("symlink loaded")
			}
			if _, published, err := PutRootContext(t.Context(), root, source); err == nil || published {
				t.Fatalf("symlink reused=%v/%v", published, err)
			}
			if packageRootStage(t, root) != "" {
				t.Fatal("symlink cache staged")
			}
		})
	}
}

func TestPackageRootContextCancellationCleansOwnedStageOnly(t *testing.T) {
	root, _ := packageRootFixture(t)
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := false
	ctx := packageRootObservedContext{Context: base, observe: func() {
		stage := packageRootStage(t, root)
		if observed || stage == "" {
			return
		}
		if _, err := root.Lstat(stage + "/manifest.yaml"); err != nil {
			return
		}
		observed = true
		cancel()
	}}
	_, published, err := PutRootContext(ctx, root, durablePackageFixture(t))
	if !observed || published || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v/%v/%v", observed, published, err)
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil || len(entries) != 0 {
		t.Fatalf("owned stage leaked: %v/%v", entries, err)
	}
	if _, err := LoadRootContext(base, root, strings.Repeat("a", 64)); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled load continued", err)
	}
}

func TestPackageRootContextExistingLeafReplacementDuringSync(t *testing.T) {
	root, _ := packageRootFixture(t)
	source := durablePackageFixture(t)
	digest, _, err := PutRootContext(t.Context(), root, source)
	if err != nil {
		t.Fatal(err)
	}
	leaf := digest + "/models/org/model/README.en.md"
	original, err := root.Lstat(leaf)
	if err != nil {
		t.Fatal(err)
	}
	data, err := root.ReadFile(leaf)
	if err != nil {
		t.Fatal(err)
	}
	replaced := false
	ctx := packageRootObservedContext{Context: t.Context(), observe: func() {
		if replaced {
			return
		}
		// The existing context observation seam holds the real sync traversal.
		// No production callback or forged package validation result is involved.
		callers := make([]uintptr, 32)
		count := runtime.Callers(2, callers)
		frames := runtime.CallersFrames(callers[:count])
		for {
			frame, more := frames.Next()
			if strings.HasSuffix(frame.Function, "packages.syncRootPackage") {
				replaced = true
				if err := root.Rename(leaf, "held-original-documentation"); err != nil {
					t.Fatal(err)
				}
				if err := root.WriteFile(leaf, data, 0644); err != nil {
					t.Fatal(err)
				}
				break
			}
			if !more {
				break
			}
		}
	}}
	_, published, err := PutRootContext(ctx, root, source)
	actual, statErr := root.Lstat(leaf)
	if !replaced || err == nil || published || statErr != nil || os.SameFile(original, actual) {
		t.Fatalf("changed leaf accepted: %v/%v/%v/%v", replaced, published, err, statErr)
	}
	after, readErr := root.ReadFile(leaf)
	saved, savedErr := root.ReadFile("held-original-documentation")
	if readErr != nil || savedErr != nil || string(after) != string(data) || string(saved) != string(data) {
		t.Fatalf("replacement/original bytes lost: %s/%s/%v/%v", after, saved, readErr, savedErr)
	}
	if packageRootStage(t, root) != "" {
		t.Fatal("known cache failure created stage")
	}
}

func TestPackageRootContextValidateHeldRootExpectedDigestAndCancellation(t *testing.T) {
	parent, _ := packageRootFixture(t)
	source := durablePackageFixture(t)
	digest, _, err := PutRootContext(t.Context(), parent, source)
	if err != nil {
		t.Fatal(err)
	}
	child, err := parent.OpenRoot(digest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := child.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := parent.Rename(digest, "held-original-package"); err != nil {
		t.Fatal(err)
	}
	if err := parent.Mkdir(digest, 0700); err != nil {
		t.Fatal(err)
	}
	if err := parent.WriteFile(digest+"/foreign", []byte("foreign sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	expected, err := inferencecatalog.LoadPackage(source)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := ValidateRootContext(t.Context(), child, digest)
	if err != nil || !reflect.DeepEqual(pkg, expected) {
		t.Fatalf("held original package not validated: %+v/%v", pkg, err)
	}
	pkg, err = ValidateRootContext(t.Context(), child, strings.Repeat("0", 64))
	if err == nil || !reflect.DeepEqual(pkg, inferencecatalog.Package{}) {
		t.Fatalf("wrong expected digest accepted: %+v/%v", pkg, err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	pkg, err = ValidateRootContext(cancelled, child, digest)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(pkg, inferencecatalog.Package{}) {
		t.Fatalf("cancelled validation returned projection: %+v/%v", pkg, err)
	}
	if _, err := child.Stat("manifest.yaml"); err != nil {
		t.Fatal("validator closed caller root", err)
	}
	sentinel, err := parent.ReadFile(digest + "/foreign")
	if err != nil || string(sentinel) != "foreign sentinel" {
		t.Fatalf("validator borrowed replacement: %s/%v", sentinel, err)
	}
	// Load uses the same validator while retaining its separate named-entry check.
	loaded, err := LoadRootContext(t.Context(), parent, "bad")
	if err == nil || !reflect.DeepEqual(loaded, inferencecatalog.Package{}) {
		t.Fatalf("invalid load returned projection: %+v/%v", loaded, err)
	}
}
