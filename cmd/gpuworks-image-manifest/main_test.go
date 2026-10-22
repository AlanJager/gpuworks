package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/manifest"
)

const (
	matrixPath = "../../build/runner-images/matrix.yaml"
	seedPath   = "../../internal/server/imagecatalog/catalog-seed.json"
)

// The checked-in seed is the generator's output for the checked-in matrix,
// byte for byte. A matrix change without `make image-manifest` fails here
// instead of shipping a catalog that no longer matches the images built.
func TestSeedMatchesMatrix(t *testing.T) {
	matrix, err := os.ReadFile(matrixPath)
	if err != nil {
		t.Fatalf("read matrix (update matrixPath and the Makefile's IMAGE_MATRIX if the topic moved): %v", err)
	}
	seed, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	var current manifest.Manifest
	if err := json.Unmarshal(seed, &current); err != nil {
		t.Fatal(err)
	}
	date, _, _ := strings.Cut(current.CatalogVersion, ".")
	got, err := generate(matrix, options{Registry: DefaultRegistry, Date: date})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, seed) {
		t.Fatalf("%s is stale; run `make image-manifest`", filepath.Base(seedPath))
	}
}

func TestGenerateMapsMatrixItems(t *testing.T) {
	matrix, err := os.ReadFile(matrixPath)
	if err != nil {
		t.Fatal(err)
	}
	out, err := generate(matrix, options{Registry: DefaultRegistry, Date: "2026-10-08"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(out)
	if err != nil {
		t.Fatalf("generated manifest does not parse: %v", err)
	}
	if m.SchemaVersion != 3 || !strings.HasPrefix(m.CatalogVersion, "2026-10-08.") || len(m.CatalogVersion) != len("2026-10-08.")+8 {
		t.Fatalf("schema/catalog version = %d %q", m.SchemaVersion, m.CatalogVersion)
	}
	versions := map[string]manifest.ManifestVersion{}
	seriesOf := map[string]manifest.ManifestSeries{}
	var names []string
	for _, series := range m.Series {
		names = append(names, series.Name)
		if series.Purpose != "inference" || len(series.CommonArgs) != 0 || len(series.CommonEnv) != 0 {
			t.Fatalf("series %q = %+v", series.Name, series)
		}
		for _, version := range series.Versions {
			versions[version.Name] = version
			seriesOf[version.Name] = series
		}
	}
	wantSeries := "MLX Audio · Metal,MLX-VLM · Metal,SGLang · CANN,SGLang · CUDA,SGLang · DTK,SGLang · HGGC,llama.cpp · Metal,vLLM Omni · CANN,vLLM Omni · CUDA,vLLM · CANN,vLLM · CUDA,vLLM · DTK,vLLM · HGGC,vLLM · Metal"
	if strings.Join(names, ",") != wantSeries {
		t.Fatalf("series = %v", names)
	}
	if len(versions) != 29 {
		t.Fatalf("versions = %d, want 29", len(versions))
	}
	// The Apple silicon environment package: framework metal, its version the
	// minimum macOS, its arch the CPU's.
	if metal := versions["metal15.0-vllm0.31.0-r2"]; metal.FrameworkVersion != "15.0" || metal.Arch != "arm64" || metal.EngineVersion != "0.31.0" || seriesOf["metal15.0-vllm0.31.0-r2"].Framework != "metal" {
		t.Fatalf("metal version = %+v in %+v", metal, seriesOf["metal15.0-vllm0.31.0-r2"])
	}
	// The servers the callable templates start are runners of engine callable.
	if llama := seriesOf["metal15.0-llama.cpp-b11617-r1"]; llama.Engine != "callable" || llama.Framework != "metal" || llama.Name != "llama.cpp · Metal" || versions["metal15.0-llama.cpp-b11617-r1"].EngineVersion != "b11617" {
		t.Fatalf("llama.cpp series = %+v, version %+v", llama, versions["metal15.0-llama.cpp-b11617-r1"])
	}
	if audio := seriesOf["metal15.0-mlx-audio0.5.9-r1"]; audio.Engine != "callable" || audio.Framework != "metal" || versions["metal15.0-mlx-audio0.5.9-r1"].EngineVersion != "0.5.9" {
		t.Fatalf("mlx-audio series = %+v", audio)
	}

	omni := versions["cann9.1-910b-vllm-omni0.30.0-r3"]
	if omni.ImageRef != "gpuworks/gpuworks-runner:cann9.1-910b-vllm-omni0.30.0-r3" || omni.EngineVersion != "0.30.0" || omni.FrameworkVersion != "9.1" || omni.Arch != "multi" {
		t.Fatalf("omni version = %+v", omni)
	}
	if s := seriesOf[omni.Name]; s.Engine != "vllm-omni" || s.Framework != "cann" || s.Name != "vLLM Omni · CANN" {
		t.Fatalf("omni series = %+v (the matrix says engine=vllm; the id decides)", s)
	}
	if !strings.Contains(omni.Notes, "requires: engine, omni, omni_no_mooncake, omni_patched, overlays") || !strings.HasSuffix(omni.Notes, "model: 910b") {
		t.Fatalf("omni notes = %q", omni.Notes)
	}
	wantSources := []string{
		"quay.io/gpuworks/gpuworks-runner:cann9.1-910b-vllm-omni0.30.0-r3",
		"swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:cann9.1-910b-vllm-omni0.30.0-r3",
	}
	if strings.Join(omni.RegistrySources, " ") != strings.Join(wantSources, " ") {
		t.Fatalf("registry sources = %v", omni.RegistrySources)
	}
	if dtk := versions["dtk26.04-vllm0.18.1-r2"]; dtk.Arch != "amd64" || dtk.FrameworkVersion != "26.04" || seriesOf[dtk.Name].Framework != "dtk" {
		t.Fatalf("dtk version = %+v", dtk)
	}
	if cuda := versions["cuda12.9-vllm0.31.0-r2"]; cuda.EngineVersion != "0.31.0" || cuda.FrameworkVersion != "12.9" || seriesOf[cuda.Name].Name != "vLLM · CUDA" {
		t.Fatalf("cuda12.9 version = %+v", cuda)
	}
	if baseline := versions["cann9.0-910b-vllm-omni0.23.0rc1-r1"]; baseline.EngineVersion != "0.23.0rc1" {
		t.Fatalf("revision r1 / rc engine version = %+v", baseline)
	}
	if sglang := versions["cann8.5-910b-sglang0.5.12.post1-r2"]; sglang.EngineVersion != "0.5.12.post1" || seriesOf[sglang.Name].Engine != "sglang" {
		t.Fatalf("post-release engine version = %+v", sglang)
	}
	for name, version := range versions {
		if version.RollingTag || version.Deprecated || version.BlobSHA256 != "" || version.SizeBytes != 0 {
			t.Fatalf("version %s carries a field the generator must not produce: %+v", name, version)
		}
	}
	again, err := generate(matrix, options{Registry: DefaultRegistry, Date: "2026-10-08"})
	if err != nil || !bytes.Equal(again, out) {
		t.Fatalf("generation is not reproducible: err=%v", err)
	}
}

// A repository gpuworks does not publish copies of has none to list.
func TestGenerateRequiresAPublishedRepository(t *testing.T) {
	_, err := generate([]byte("defaults:\n  platforms: [linux/amd64]\nitems:\n  - id: cuda13.0-vllm0.30.0\n    revision: r2\n"), options{Registry: "example/unmirrored", Date: "2026-10-08"})
	if err == nil || !strings.Contains(err.Error(), "no published copies") {
		t.Fatalf("err = %v", err)
	}
}

func TestGenerateRejectsInconsistentMatrix(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"id grammar": {
			yaml: "items:\n  - id: rocm6.4-vllm0.30.0\n    revision: r1\n",
			want: "rocm6.4-vllm0.30.0",
		},
		"engine version arg": {
			yaml: "items:\n  - id: cuda13.0-vllm0.30.0\n    revision: r2\n    args:\n      ENGINE_VERSION: \"0.29.0\"\n",
			want: "cuda13.0-vllm0.30.0",
		},
		"sglang version arg": {
			yaml: "items:\n  - id: cann9.0-910b-sglang0.5.18\n    revision: r2\n    args:\n      SGLANG_VERSION: \"0.5.17\"\n",
			want: "cann9.0-910b-sglang0.5.18",
		},
		"framework version arg": {
			yaml: "items:\n  - id: cuda12.9-vllm0.31.0\n    revision: r2\n    args:\n      FRAMEWORK_VERSION: \"12.8\"\n",
			want: "cuda12.9-vllm0.31.0",
		},
		"platform": {
			yaml: "defaults:\n  platforms: [linux/amd64]\nitems:\n  - id: cuda13.0-vllm0.30.0\n    revision: r2\n    platforms: [linux/amd64, linux/ppc64le]\n",
			want: "cuda13.0-vllm0.30.0",
		},
		"revision": {
			yaml: "items:\n  - id: cuda13.0-vllm0.30.0\n",
			want: "cuda13.0-vllm0.30.0",
		},
		"backend": {
			yaml: "items:\n  - id: cuda13.0-vllm0.30.0\n    revision: r2\n    backend: cann\n",
			want: "cuda13.0-vllm0.30.0",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := generate([]byte("defaults:\n  platforms: [linux/amd64]\n"+strings.TrimPrefix(tc.yaml, "defaults:\n  platforms: [linux/amd64]\n")), options{Date: "2026-10-08"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// The omni engine must win over plain vllm, or omni ids would split into
// engine=vllm with an engine version of "-omni…".
// llama.cpp numbers its releases b<build>; the id writes them llama.cpp-b<build>.
func TestParseTagReadsALlamaCppBuild(t *testing.T) {
	tag, err := parseTag("metal15.0-llama.cpp-b11617")
	if err != nil || tag.Engine != "llama.cpp" || tag.EngineVersion != "b11617" || tag.Backend != "metal" || tag.FrameworkVersion != "15.0" {
		t.Fatalf("tag = %+v, %v", tag, err)
	}
	for _, bad := range []string{"metal15.0-llama.cpp11617", "metal15.0-llama.cpp-11617", "metal15.0-llama.cppb11617"} {
		if _, err := parseTag(bad); err == nil {
			t.Errorf("parseTag(%q) accepted a malformed llama.cpp id", bad)
		}
	}
}

func TestParseTagPrefersOmni(t *testing.T) {
	tag, err := parseTag("cann9.1-910b-vllm-omni0.30.0")
	if err != nil {
		t.Fatal(err)
	}
	if tag != (parsedTag{Backend: "cann", FrameworkVersion: "9.1", Model: "910b", Engine: "vllm-omni", EngineVersion: "0.30.0"}) {
		t.Fatalf("tag = %+v", tag)
	}
}

// The metal framework version of an environment package is the minimum macOS
// it runs on, which the agent's doctor and the install refuse below
// (accelerator.MinimumMacOSMajor), and build.sh builds for (MACOS_TARGET).
// The three cannot drift apart.
func TestMetalFrameworkVersionIsTheMinimumMacOS(t *testing.T) {
	data, err := os.ReadFile(matrixPath)
	if err != nil {
		t.Fatal(err)
	}
	var matrix matrixFile
	if err := yaml.Unmarshal(data, &matrix); err != nil {
		t.Fatal(err)
	}
	want := strconv.Itoa(accelerator.MinimumMacOSMajor) + ".0"
	metal := 0
	for _, item := range matrix.Items {
		tag, err := parseTag(item.ID)
		if err != nil || tag.Backend != "metal" {
			continue
		}
		metal++
		if tag.FrameworkVersion != want {
			t.Errorf("%s: framework version %s, want the minimum macOS %s", item.ID, tag.FrameworkVersion, want)
		}
	}
	if metal == 0 {
		t.Fatal("no metal items in the matrix")
	}
	script, err := os.ReadFile("../../build/host-envs/build.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^MACOS_TARGET=(\S+)`).FindSubmatch(script)
	if m == nil || string(m[1]) != want {
		t.Fatalf("build.sh MACOS_TARGET = %q, want %s", m, want)
	}
}
