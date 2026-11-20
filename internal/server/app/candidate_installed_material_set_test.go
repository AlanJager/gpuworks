//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

func candidateInstalledPackageFixture(t *testing.T) (string, *materialReceiver, map[uint64]materialManifest) {
	t.Helper()
	dir, receiver, _, files, _ := receivedPackageRootFixture(t)
	pkg, err := inferencecatalog.LoadPackage(fsFixture(files))
	if err != nil {
		t.Fatal(err)
	}
	asset := []byte("print('shared installed asset')\n")
	model := pkg.Catalog.Models[0]
	model.Specs[0].Assets = []inferencecatalog.Asset{{File: "assets/serve.py", SHA256: materialTestDigest(asset), UnpackTo: "/opt/gpuworks/serve.py"}}
	files["models/"+model.ModelID+"/model.yaml"], err = inferencecatalog.MarshalModelYAML(model)
	if err != nil {
		t.Fatal(err)
	}
	files["models/"+model.ModelID+"/assets/serve.py"] = asset
	digest, err := inferencecatalog.PackageDigest(fsFixture(files))
	if err != nil {
		t.Fatal(err)
	}
	manifest := materialManifest{SourceMemberID: 1}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		file := manifestFile(MaterialCatalogPackage, "catalog-packages/"+digest+"/"+name, files[name])
		if _, err := receiver.receive(t.Context(), file.SHA256, bytes.NewReader(files[name])); err != nil {
			t.Fatal(err)
		}
		manifest.Files = append(manifest.Files, file)
		if strings.Contains(name, "/assets/") {
			file.Class = MaterialRuntimeAsset
			manifest.Files = append(manifest.Files, file)
		}
	}
	master := []byte("shared master remains a consumer requirement")
	if err := os.WriteFile(filepath.Join(dir, "master.key"), master, 0600); err != nil {
		t.Fatal(err)
	}
	manifest.Files = append(manifest.Files, manifestFile(MaterialMasterSecret, "master.key", master), manifestFile(MaterialMemberIdentity, "gpuworks.db.join/member.key", []byte("first source member key")))
	if err := materializeReceivedManifest(t.Context(), receiver, manifest); err != nil {
		t.Fatal(err)
	}
	second := manifest
	second.SourceMemberID = 3
	second.Files = slices.Clone(manifest.Files)
	second.Files[len(second.Files)-1] = manifestFile(MaterialMemberIdentity, "gpuworks.db.join/member.key", []byte("different second source key"))
	return dir, receiver, map[uint64]materialManifest{1: manifest, 3: second}
}

func TestCandidateInstalledMaterialSetsShareConsumerProof(t *testing.T) {
	dir, receiver, manifests := candidateInstalledPackageFixture(t)
	recorder := &candidateProofRecorder{}
	ctx := context.WithValue(t.Context(), candidateProofMetricsKey{}, recorder)
	if err := checkCandidateInstalledMaterialSets(ctx, receiver, manifests); err != nil {
		t.Fatal(err)
	}
	var count, size int64
	for _, file := range manifests[1].Files {
		if file.Class != MaterialMemberIdentity {
			count++
			size += file.Bytes
		}
	}
	if got := recorder.snapshot().LocalHash; got.Calls != count || got.VerifiedBytes != size {
		t.Fatalf("shared consumer bytes checked repeatedly: got=%+v want calls=%d bytes=%d", got, count, size)
	}
	if _, err := os.Lstat(filepath.Join(dir, "gpuworks.db.join/member.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source identity was required or installed at a consumer path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "master.key"), []byte("wrong master"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkCandidateInstalledMaterialSets(t.Context(), receiver, manifests); err == nil {
		t.Fatal("shared master was excluded from consumer proof")
	}
}

func TestCandidateInstalledMaterialSetsKeepUniqueConsumerPaths(t *testing.T) {
	for _, sameDigest := range []bool{false, true} {
		t.Run(map[bool]string{false: "second-source-file", true: "same-digest-two-paths"}[sameDigest], func(t *testing.T) {
			dir, receiver, manifests := candidateInstalledPackageFixture(t)
			body := []byte("unique second consumer")
			file := manifestFile(MaterialAgentOverride, "dl/second-agent", body)
			if sameDigest {
				if err := os.MkdirAll(filepath.Join(dir, "dl"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "dl/first-agent"), body, 0600); err != nil {
					t.Fatal(err)
				}
				first := manifests[1]
				first.Files = append(first.Files, manifestFile(MaterialAgentOverride, "dl/first-agent", body))
				manifests[1] = first
			}
			second := manifests[3]
			second.Files = append(second.Files, file)
			manifests[3] = second
			if err := checkCandidateInstalledMaterialSets(t.Context(), receiver, manifests); err == nil {
				t.Fatal("absent consumer file from second source was accepted")
			}
			if err := os.MkdirAll(filepath.Join(dir, "dl"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, file.Path), body, 0600); err != nil {
				t.Fatal(err)
			}
			if err := checkCandidateInstalledMaterialSets(t.Context(), receiver, manifests); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCandidateInstalledMaterialSetsRejectConflictsBeforeConsumerRead(t *testing.T) {
	for _, changeSize := range []bool{false, true} {
		t.Run(map[bool]string{false: "digest", true: "bytes"}[changeSize], func(t *testing.T) {
			_, receiver, manifests := candidateInstalledPackageFixture(t)
			second := manifests[3]
			if changeSize {
				second.Files[0].Bytes++
			} else {
				second.Files[0].SHA256 = materialTestDigest([]byte("contradictory source bytes"))
			}
			manifests[3] = second
			recorder := &candidateProofRecorder{}
			ctx := context.WithValue(t.Context(), candidateProofMetricsKey{}, recorder)
			if err := checkCandidateInstalledMaterialSets(ctx, receiver, manifests); err == nil {
				t.Fatal("source consumer path conflict was accepted")
			}
			if got := recorder.snapshot().LocalHash.Calls; got != 0 {
				t.Fatalf("conflicting source union read %d consumer files before rejection", got)
			}
		})
	}
}

func TestCandidateInstalledMaterialSetsValidateIdentityMetadata(t *testing.T) {
	for _, mode := range []string{"path", "digest", "size", "limit"} {
		t.Run(mode, func(t *testing.T) {
			_, receiver, manifests := candidateInstalledPackageFixture(t)
			second := manifests[3]
			identity := &second.Files[len(second.Files)-1]
			switch mode {
			case "path":
				identity.Path = "dl/member.key"
			case "digest":
				identity.SHA256 = "bad"
			case "size":
				identity.Bytes = 0
			case "limit":
				identity.Bytes = maxMaterialReplicaBytes + 1
			}
			manifests[3] = second
			if err := checkCandidateInstalledMaterialSets(t.Context(), receiver, manifests); err == nil || !strings.Contains(err.Error(), "invalid manifest entry") {
				t.Fatalf("identity metadata bypassed original entry validation: %v", err)
			}
		})
	}
}

func TestCandidateInstalledMaterialSetsFailClosedDuringRealConsumerProof(t *testing.T) {
	for _, mode := range []string{"corrupt", "cancel", "replace-root"} {
		t.Run(mode, func(t *testing.T) {
			dir, receiver, manifests := candidateInstalledPackageFixture(t)
			recorder := &candidateProofRecorder{}
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := context.WithValue(base, candidateProofMetricsKey{}, recorder)
			if mode == "corrupt" {
				file := manifests[1].Files[0]
				if err := os.WriteFile(filepath.Join(dir, file.Path), bytes.Repeat([]byte("x"), int(file.Bytes)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var swapped bool
			if mode == "cancel" || mode == "replace-root" {
				mutate := cancel
				if mode == "replace-root" {
					moved := dir + ".original"
					mutate = func() {
						if err := os.Rename(dir, moved); err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { _ = os.RemoveAll(dir); _ = os.Rename(moved, dir) })
						if err := os.Mkdir(dir, 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(dir, "foreign-keep"), []byte("foreign root untouched"), 0600); err != nil {
							t.Fatal(err)
						}
						swapped = true
					}
				}
				ctx = &observedMaterialContext{Context: ctx, cancel: mutate, trigger: 1, ready: func() bool { return recorder.snapshot().LocalHash.Calls > 0 }}
			}
			err := checkCandidateInstalledMaterialSets(ctx, receiver, manifests)
			if err == nil {
				t.Fatal("consumer mutation/cancellation was accepted")
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("consumer hash cancellation was not propagated: %v", err)
			}
			if mode == "replace-root" {
				if !swapped || !strings.Contains(err.Error(), "data root identity changed") {
					t.Fatalf("held consumer root replacement was not rejected: swapped=%v err=%v", swapped, err)
				}
				body, readErr := os.ReadFile(filepath.Join(dir, "foreign-keep"))
				entries, listErr := os.ReadDir(dir)
				if readErr != nil || listErr != nil || string(body) != "foreign root untouched" || len(entries) != 1 {
					t.Fatalf("proof changed foreign consumer root: body=%q read=%v entries=%v list=%v", body, readErr, entries, listErr)
				}
			}
		})
	}
}
