package templates_test

import (
	"gpuworks.ai/gpuworks/internal/testutil/catalogfixture"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

// This Go lane validates source overlays and the current seed through the
// catalog/launch APIs without requiring Python packages. The materializer's
// runtime inspection and failure paths run in test-amd-import-tools.
func TestAMDOverlayLaunchContracts(t *testing.T) {
	output := t.TempDir()
	for _, name := range []string{"amd395", "rocm"} {
		catalogfixture.WriteLaunchOverlay(t, "../../internal/server/inferencecatalog/seed/catalog", filepath.Join(name, "catalog"), filepath.Join(output, name, "catalog"))
	}
	for _, name := range []string{"amd395", "rocm"} {
		t.Run(name, func(t *testing.T) {
			pkg, err := inferencecatalog.LoadPackage(os.DirFS(filepath.Join(output, name, "catalog")))
			if err != nil {
				t.Fatal(err)
			}
			wantModels := 1
			arch := "gfx942"
			gpuName := "Instinct MI300X"
			if name == "amd395" {
				wantModels = 2
				arch = "gfx1151"
				gpuName = "Radeon 8060S"
			}
			if len(pkg.Catalog.Models) != wantModels {
				t.Fatalf("models=%d", len(pkg.Catalog.Models))
			}
			for _, model := range pkg.Catalog.Models {
				if len(model.Specs) != 2 {
					t.Fatalf("%s specs=%d", model.ModelID, len(model.Specs))
				}
				for _, spec := range model.Specs {
					if spec.GPUs.ArchID != arch || spec.WeightBytes <= 0 || !strings.HasPrefix(spec.Image, "verified.example/") {
						t.Fatalf("invalid generated AMD spec %s", spec.Name)
					}
					selector := chipmatch.Spec{Name: spec.Name, Quantization: spec.Weights.Quantization, GPUs: chipmatch.Predicate{Vendor: spec.GPUs.Vendor, Name: spec.GPUs.Name, ArchID: spec.GPUs.ArchID}}
					if err := chipmatch.ValidateSpec(chipmatch.ReportedGPU(gpuName, "amd", arch, ""), selector, nil); err != nil {
						t.Fatal(err)
					}
					if err := chipmatch.ValidateSpec(chipmatch.ReportedGPU(gpuName, "amd", "wrong", ""), selector, nil); err == nil {
						t.Fatal("architecture mismatch accepted")
					}
					engine, err := deploy.SelectEngine(spec.Engine)
					if err != nil {
						t.Fatal(err)
					}
					launch, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{Engine: engine, Params: deploy.Params{Vendor: "amd", ModelPath: "/model", APIPort: 30000, TensorParallel: 1, AssignedGPUs: 1, TemplateArgs: spec.Args, TemplateEnv: spec.Env}, Entrypoint: spec.Entrypoint})
					if err != nil {
						t.Fatal(err)
					}
					command := strings.Join(append(launch.Entrypoint, launch.Argv...), " ")
					if !strings.Contains(command, "/model") || !strings.Contains(command, "30000") || strings.Contains(command, "${") {
						t.Fatalf("incomplete launch: %s", command)
					}
				}
			}
		})
	}
}
