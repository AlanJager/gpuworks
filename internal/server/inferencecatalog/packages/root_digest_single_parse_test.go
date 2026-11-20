//go:build linux

package packages

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

func TestPackageRootDigestRejectsSameInodeLengthAndMtimeRewrite(t *testing.T) {
	for _, leaf := range []string{"manifest.yaml", "models/org/model/model.yaml", "models/org/model/README.en.md", "models/org/model/assets/serve.py"} {
		t.Run(leaf, func(t *testing.T) {
			parent, dir := packageRootFixture(t)
			fixture := durablePackageFixture(t)
			pkg, err := inferencecatalog.LoadPackage(fixture)
			if err != nil {
				t.Fatal(err)
			}
			asset := []byte("print('ready')\n")
			assetSHA := sha256.Sum256(asset)
			model := pkg.Catalog.Models[0]
			model.Specs[0].Assets = []inferencecatalog.Asset{{File: "assets/serve.py", SHA256: hex.EncodeToString(assetSHA[:]), UnpackTo: "/opt/gpuworks/serve.py"}}
			fixture["models/org/model/model.yaml"].Data, err = inferencecatalog.MarshalModelYAML(model)
			if err != nil {
				t.Fatal(err)
			}
			fixture["models/org/model/assets/serve.py"] = &fstest.MapFile{Data: asset}
			digest, _, err := PutRootContext(t.Context(), parent, fixture)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := LoadRootContext(t.Context(), parent, digest); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(dir, digest, leaf)
			before, err := os.Stat(name)
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			replacement := bytes.Clone(original)
			// Keep semantic YAML valid so content hashing must detect the mutation.
			if leaf == "manifest.yaml" {
				replacement = bytes.Replace(replacement, []byte("2026-09-21"), []byte("2026-09-22"), 1)
			} else if leaf == "models/org/model/model.yaml" {
				replacement = bytes.Replace(replacement, []byte("Model"), []byte("Other"), 1)
			} else {
				replacement[0] ^= 1
			}
			if bytes.Equal(original, replacement) || len(original) != len(replacement) {
				t.Fatal("fixture did not preserve length")
			}
			if err := os.WriteFile(name, replacement, before.Mode().Perm()); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(name, before.ModTime(), before.ModTime()); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(name)
			if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
				t.Fatalf("fixture identity changed: %v", err)
			}
			got, err := LoadRootContext(t.Context(), parent, digest)
			if err == nil || !reflect.DeepEqual(got, inferencecatalog.Package{}) {
				t.Fatalf("same-metadata corruption accepted: %+v/%v", got, err)
			}
			retained, err := os.ReadFile(name)
			if err != nil || !bytes.Equal(retained, replacement) {
				t.Fatalf("proof rewrote corruption: %v", err)
			}
		})
	}
}

func TestPackageRootDigestCancellationDuringArchiveReturnsNoProjection(t *testing.T) {
	parent, _ := packageRootFixture(t)
	digest, _, err := PutRootContext(t.Context(), parent, durablePackageFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	child, err := parent.OpenRoot(digest)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	archiveChecks := 0
	ctx := packageRootObservedContext{Context: base, observe: func() {
		// The cancellation adapter queries context before every operation. Detect
		// the actual archive caller, after semantic validation has completed.
		if packageDigestArchiveReading() {
			archiveChecks++
			cancel()
		}
	}}
	got, err := ValidateRootContext(ctx, child, digest)
	if archiveChecks == 0 || !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, inferencecatalog.Package{}) {
		t.Fatalf("archive cancellation=%d/%+v/%v", archiveChecks, got, err)
	}
	if _, err := child.Stat("manifest.yaml"); err != nil {
		t.Fatal("caller root closed", err)
	}
}

func packageDigestArchiveReading() bool {
	pcs := make([]uintptr, 48)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	packing := false
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, "inferencecatalog.LoadPackage") {
			return false
		}
		if strings.HasSuffix(frame.Function, "inferencecatalog.packPackage") {
			packing = true
		}
		if !more {
			return packing
		}
	}
}
