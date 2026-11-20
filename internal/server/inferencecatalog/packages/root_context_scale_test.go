package packages

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

// manyModelPackageFixture is a valid package of n models, two entries each.
func manyModelPackageFixture(t *testing.T, n int) fstest.MapFS {
	t.Helper()
	files := fstest.MapFS{"manifest.yaml": {Data: []byte(fmt.Sprintf("schema_version: 1\npublished_at: 2026-09-21T00:00:00Z\nmodels: %d\nspecs: %d\n", n, n))}}
	for i := range n {
		id := fmt.Sprintf("org/model-%04d", i)
		model := inferencecatalog.InferenceModel{ModelID: id, DisplayTitle: "Model", Specs: []inferencecatalog.InferenceSpec{{Name: "default", Engine: "vllm", Image: "vllm/vllm-openai:latest", Weights: inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: id}}}, GPUs: inferencecatalog.GPUSelector{Vendor: "nvidia", CC: ">=8.0"}}}}
		data, err := inferencecatalog.MarshalModelYAML(model)
		if err != nil {
			t.Fatal(err)
		}
		files["models/"+id+"/model.yaml"] = &fstest.MapFile{Data: data}
	}
	return files
}

// The copy compared the whole staged tree after every entry, so a package of
// a thousand entries took most of a minute to publish and a joining member
// spent that inside its join. Four times the entries must cost about four
// times the work, not sixteen.
func TestPackageRootContextCopyScalesWithThePackage(t *testing.T) {
	publish := func(n int) time.Duration {
		best := time.Duration(0)
		for range 2 {
			root, _ := packageRootFixture(t)
			source := manyModelPackageFixture(t, n)
			started := time.Now()
			if _, published, err := PutRootContext(t.Context(), root, source); err != nil || !published {
				t.Fatalf("publishing %d models: published=%v err=%v", n, published, err)
			}
			if took := time.Since(started); best == 0 || took < best {
				best = took
			}
		}
		return best
	}
	small, large := publish(150), publish(600)
	if large > 9*small {
		t.Fatalf("600 models took %s and 150 took %s: the copy grows faster than the package", large, small)
	}
}

// An entry replaced after it was copied is no longer noticed at the next
// entry. It must still stop the publication.
func TestPackageRootContextRefusesAnEarlierEntryReplacedDuringTheCopy(t *testing.T) {
	for _, mode := range []string{"same-bytes-new-file", "removed", "added"} {
		t.Run(mode, func(t *testing.T) {
			root, _ := packageRootFixture(t)
			source := manyModelPackageFixture(t, 12)
			expected, err := inferencecatalog.PackageDigest(source)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			_, published, err := PutRootContextWithProgress(t.Context(), root, source, func() error {
				calls++
				if calls != 3 {
					return nil
				}
				stage := packageRootStage(t, root)
				first := ""
				walkErr := fs.WalkDir(root.FS(), stage, func(name string, entry fs.DirEntry, e error) error {
					if e == nil && !entry.IsDir() && strings.HasSuffix(name, "model.yaml") && first == "" {
						first = name
					}
					return e
				})
				if walkErr != nil || first == "" {
					return errors.Join(walkErr, errors.New("no copied model to replace"))
				}
				switch mode {
				case "same-bytes-new-file":
					data, e := root.ReadFile(first)
					if e != nil {
						return e
					}
					// Hold the copied file open across the replacement: a file
					// system may otherwise hand its inode to the new file within
					// the same timestamp tick, and no stat could tell them apart.
					held, e := root.Open(first)
					if e != nil {
						return e
					}
					t.Cleanup(func() { _ = held.Close() })
					if e := root.Remove(first); e != nil {
						return e
					}
					return root.WriteFile(first, data, 0644)
				case "removed":
					return root.Remove(first)
				default:
					return root.WriteFile(stage+"/models/foreign", []byte("foreign"), 0644)
				}
			})
			if err == nil || published {
				t.Fatalf("publication with a changed earlier entry: published=%v err=%v", published, err)
			}
			if _, statErr := root.Lstat(expected); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("the digest directory exists after a refused publication: %v", statErr)
			}
		})
	}
}
