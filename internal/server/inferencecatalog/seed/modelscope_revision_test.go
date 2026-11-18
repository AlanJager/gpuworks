package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// Provider metadata captured 2026-11-18, not a rule that two providers can
// never share a commit. Each old revision was independently confirmed 404
// while the repository's own immutable commit returned a populated listing.
func TestEmbeddedSourcesUseVerifiedModelScopeRevisions(t *testing.T) {
	var snapshots []struct {
		Repo            string          `json:"repo"`
		InvalidRevision string          `json:"invalid_revision"`
		ValidRevisions  []string        `json:"valid_revisions"`
		File            json.RawMessage `json:"file"`
		LatestCommitter json.RawMessage `json:"latest_committer"`
	}
	data, err := os.ReadFile("testdata/modelscope-revisions.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &snapshots); err != nil {
		t.Fatal(err)
	}
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range snapshots {
		t.Run(snapshot.Repo, func(t *testing.T) {
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/models/"+snapshot.Repo+"/repo/files" || r.URL.Query().Get("Recursive") != "true" {
					http.Error(w, "unexpected provider request", 400)
					return
				}
				revision := r.URL.Query().Get("Revision")
				if revision == snapshot.InvalidRevision {
					w.WriteHeader(http.StatusNotFound)
					_, _ = fmt.Fprint(w, `{"Code":404,"Message":"revision does not exist","Data":null}`)
					return
				}
				if revision != "" && !slices.Contains(snapshot.ValidRevisions, revision) {
					t.Errorf("unverified revision %q: refresh provider snapshot when re-pinning", revision)
					http.Error(w, "unknown revision", 404)
					return
				}
				_, _ = fmt.Fprintf(w, `{"Code":200,"Data":{"LatestCommitter":%s,"Files":[%s]}}`, snapshot.LatestCommitter, snapshot.File)
			}))
			defer hub.Close()
			found := false
			for _, model := range catalog.Models {
				for _, spec := range model.Specs {
					for _, source := range spec.Weights.Sources {
						if source.Provider != modelrepo.SourceMS || source.RepoID != snapshot.Repo {
							continue
						}
						found = true
						files, err := modelrepo.ListFiles(context.Background(), source.Provider, source.RepoID, source.Revision, hub.URL, nil)
						if err != nil || len(files) == 0 {
							t.Errorf("%s/%s ModelScope listing failed at declared pin: %v", model.ModelID, spec.Name, err)
						}
					}
				}
			}
			if !found {
				t.Fatal("snapshot no longer corresponds to a catalog source")
			}
		})
	}
}

// Explicit opt-in network sentinel for catalog maintainers. Default tests
// remain offline; no weight file is downloaded by either verification path.
func TestEmbeddedModelScopePinsPublicContract(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_NET") != "1" {
		t.Skip("set GPUWORKS_TEST_NET=1 to verify provider metadata")
	}
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			for _, source := range spec.Weights.Sources {
				if source.Provider != modelrepo.SourceMS {
					continue
				}
				key := source.RepoID + "@" + source.Revision
				if seen[key] {
					continue
				}
				seen[key] = true
				t.Run(strings.ReplaceAll(key, "/", "_"), func(t *testing.T) {
					_, err := modelrepo.ListFiles(context.Background(), source.Provider, source.RepoID, source.Revision, "", nil)
					if err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}
