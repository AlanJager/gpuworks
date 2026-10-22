package inferencecatalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func packageFixture(t *testing.T) fstest.MapFS {
	t.Helper()
	model := flatFixture().Models[0]
	model.ModelID = "org/model"
	model.Specs[0].Assets = []Asset{{File: "assets/serve.py", SHA256: "e13cdff7de4a58aa927d0812d424a163af37d0619bebb12c0e68f042cd02a511", UnpackTo: "/opt/gpuworks/serve.py"}}
	data, err := MarshalModelYAML(model)
	if err != nil {
		t.Fatal(err)
	}
	return fstest.MapFS{
		"manifest.yaml":                    {Data: []byte("schema_version: 1\npublished_at: 2026-08-31T00:00:00Z\nmodels: 1\nspecs: 1\n")},
		"models/org/model/model.yaml":      {Data: data},
		"models/org/model/README.zh-CN.md": {Data: []byte("# 使用说明\n")},
		"models/org/model/assets/serve.py": {Data: []byte("print('ready')\n")},
	}
}

func TestLoadPackageValidatesModelOwnedLayoutAndSummary(t *testing.T) {
	loaded, err := LoadPackage(packageFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Manifest.Models != 1 || loaded.Catalog.Models[0].ModelID != "org/model" {
		t.Fatalf("loaded=%+v", loaded)
	}
	if got := loaded.Catalog.Models[0].Specs[0].Assets[0].SizeBytes; got != int64(len("print('ready')\n")) {
		t.Fatalf("asset size_bytes=%d", got)
	}

	wrongPath := packageFixture(t)
	wrongPath["models/wrong/model/model.yaml"] = wrongPath["models/org/model/model.yaml"]
	delete(wrongPath, "models/org/model/model.yaml")
	if _, err := LoadPackage(wrongPath); err == nil || !strings.Contains(err.Error(), "must be stored") {
		t.Fatalf("wrong path error=%v", err)
	}

	wrongCount := packageFixture(t)
	wrongCount["manifest.yaml"] = &fstest.MapFile{Data: []byte("schema_version: 1\npublished_at: 2026-08-31T00:00:00Z\nmodels: 2\nspecs: 1\n")}
	if _, err := LoadPackage(wrongCount); err == nil || !strings.Contains(err.Error(), "manifest says") {
		t.Fatalf("wrong count error=%v", err)
	}

	wrongSize := packageFixture(t)
	model, err := parseModelYAML(wrongSize["models/org/model/model.yaml"].Data)
	if err != nil {
		t.Fatal(err)
	}
	model.Specs[0].Assets[0].SizeBytes = 1
	wrongSize["models/org/model/model.yaml"].Data, err = MarshalModelYAML(model)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPackage(wrongSize); err == nil || !strings.Contains(err.Error(), "actual size") {
		t.Fatalf("wrong asset size error=%v", err)
	}
}

func TestLoadPackageRejectsUnknownFieldsAndEscapingIdentity(t *testing.T) {
	unknown := packageFixture(t)
	unknown["manifest.yaml"] = &fstest.MapFile{Data: []byte("schema_version: 1\npublished_at: 2026-08-31T00:00:00Z\nmodels: 1\nspecs: 1\nunknown: true\n")}
	if _, err := LoadPackage(unknown); err == nil {
		t.Fatal("unknown manifest field was accepted")
	}

	unexpected := packageFixture(t)
	unexpected["models/org/model/dependency.whl"] = &fstest.MapFile{Data: []byte("wheel")}
	if _, err := LoadPackage(unexpected); err == nil || !strings.Contains(err.Error(), "unexpected model file") {
		t.Fatalf("unexpected file error=%v", err)
	}

	catalog := flatFixture()
	catalog.Models[0].ModelID = "org/../escape"
	if err := ValidateFlatCatalog(&catalog); err == nil {
		t.Fatal("dot-segment model id was accepted")
	}
}

func TestPackPackageIsDeterministicAndContainsOnlyRegularFiles(t *testing.T) {
	fixture := packageFixture(t)
	var first, second bytes.Buffer
	if err := PackPackage(&first, fixture); err != nil {
		t.Fatal(err)
	}
	if err := PackPackage(&second, fixture); err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(first.Bytes()) != sha256.Sum256(second.Bytes()) {
		t.Fatal("deterministic package hashes differ")
	}

	gz, err := gzip.NewReader(bytes.NewReader(first.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	tarReader := tar.NewReader(gz)
	var names []string
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg || header.Mode != 0o644 || !header.ModTime.Equal(time.Unix(0, 0).UTC()) {
			t.Fatalf("non-canonical header=%+v", header)
		}
		names = append(names, header.Name)
	}
	if !strings.Contains(strings.Join(names, "\n"), "models/org/model/model.yaml") {
		t.Fatalf("archive names=%v", names)
	}

	withSymlink := packageFixture(t)
	withSymlink["models/org/model/assets/link.py"] = &fstest.MapFile{Mode: fs.ModeSymlink}
	if err := PackPackage(io.Discard, withSymlink); err == nil {
		t.Fatal("non-regular package file was accepted")
	}
}

func TestWritePackageDirectoryRoundTrips(t *testing.T) {
	root := t.TempDir() + "/catalog"
	if err := WritePackageDirectory(root, "2026-08-31T00:00:00Z", flatFixture()); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPackageDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Catalog.Models[0].ModelID != "org/model" || loaded.Manifest.Specs != 1 {
		t.Fatalf("loaded=%+v", loaded)
	}
	if err := WritePackageDirectory(root, "2026-08-31T00:00:00Z", flatFixture()); err == nil {
		t.Fatal("existing package directory was overwritten")
	}
}

func TestWritePackageDirectoryRoundTripsCrossModelComponentReferences(t *testing.T) {
	catalog := flatFixture()
	target := catalog.Models[0]
	target.ModelID = "org/target"
	target.Specs[0].Args = append(target.Specs[0].Args,
		"--speculative-config", `{"model":"/models/draft"}`)
	target.Specs[0].Components = []Component{
		{
			Name: "target", Role: "primary", Weights: target.Specs[0].Weights,
			WeightBytes: target.Specs[0].WeightBytes, MountPath: "/models/target",
		},
		{
			Name: "draft", Role: "speculative_draft", ModelID: "org/draft",
			Weights: target.Specs[0].Weights, WeightBytes: target.Specs[0].WeightBytes,
			MountPath: "/models/draft",
		},
	}
	draft := target
	draft.ModelID = "org/draft"
	draft.Specs = nil
	catalog.Models = []InferenceModel{target, draft}

	root := t.TempDir() + "/catalog"
	if err := WritePackageDirectory(root, "2026-08-31T00:00:00Z", catalog); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPackageDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Catalog.Models) != 2 || loaded.Manifest.Specs != 1 {
		t.Fatalf("loaded=%+v", loaded)
	}
}

func TestWritePackageDirectoryCopiesOnlyDeclaredModelFiles(t *testing.T) {
	fixture := packageFixture(t)
	model, err := parseModelYAML(fixture["models/org/model/model.yaml"].Data)
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	for _, name := range []string{"models/org/model/assets/serve.py", "models/org/model/README.zh-CN.md"} {
		file := fixture[name]
		target := source + "/" + name
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, file.Data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir() + "/catalog"
	if err := WritePackageDirectoryWithFiles(root, "2026-08-31T00:00:00Z", FlatCatalog{Models: []InferenceModel{model}}, os.DirFS(source)); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPackageDirectory(root)
	if err != nil || loaded.Catalog.Models[0].ReadmeZHCN != "README.zh-CN.md" {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	if _, err := os.Stat(root + "/models/org/model/assets/serve.py"); err != nil {
		t.Fatal(err)
	}
	if err := WritePackageDirectory(t.TempDir()+"/missing", "2026-08-31T00:00:00Z", FlatCatalog{Models: []InferenceModel{model}}); err == nil || !strings.Contains(err.Error(), "requires a package files source") {
		t.Fatalf("missing files source error=%v", err)
	}
}

func TestReadPackageArchiveRoundTripsAndRejectsLinks(t *testing.T) {
	var packed bytes.Buffer
	if err := PackPackage(&packed, packageFixture(t)); err != nil {
		t.Fatal(err)
	}
	archive, err := ReadPackageArchive(bytes.NewReader(packed.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if archive.Package.Manifest.Models != 1 || archive.Package.Catalog.Models[0].ModelID != "org/model" {
		t.Fatalf("archive=%+v", archive.Package)
	}

	var malicious bytes.Buffer
	gz := gzip.NewWriter(&malicious)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.yaml", Typeflag: tar.TypeSymlink, Linkname: "outside"}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPackageArchive(bytes.NewReader(malicious.Bytes())); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink archive error=%v", err)
	}
}
