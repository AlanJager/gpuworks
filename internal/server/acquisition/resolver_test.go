package acquisition

import (
	"strings"
	"testing"
)

func TestResolveHasSingleProviderRecommendation(t *testing.T) {
	r := Resolve(WeightFacts{Providers: []WeightSource{{Provider: "huggingface", RepoID: "a", Suggested: true}, {Provider: "modelscope", RepoID: "a", Suggested: true}}}, ImageFacts{})
	count := 0
	for _, c := range r.Providers {
		if c.Recommended {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("recommended providers=%d", count)
	}
}

func TestResolveFallsBackToFirstProviderWhenNoneSuggested(t *testing.T) {
	r := Resolve(WeightFacts{Providers: []WeightSource{
		{Provider: "huggingface", RepoID: "org/model"},
		{Provider: "modelscope", RepoID: "org/model"},
	}}, ImageFacts{})
	if len(r.Providers) != 2 || !r.Providers[0].Recommended || r.Providers[1].Recommended {
		t.Fatalf("providers=%+v", r.Providers)
	}
}

func TestResolveDeduplicatesProviderCandidates(t *testing.T) {
	r := Resolve(WeightFacts{Providers: []WeightSource{
		{Provider: "huggingface", RepoID: "org/model", Revision: "main", Suggested: true},
		{Provider: "huggingface", RepoID: "org/model", Revision: "main"},
	}}, ImageFacts{})
	if len(r.Providers) != 1 || !r.Providers[0].Recommended {
		t.Fatalf("providers=%+v", r.Providers)
	}
}

func TestResolvePresentPrefersFilesOnTheNode(t *testing.T) {
	r := Resolve(WeightFacts{
		Present: []StoragePresence{
			{StorageID: 3, StorageName: "archive", StorageType: "nfs_cache", CacheBytes: 15},
			{StorageID: 2, StorageName: "model-archive", StorageType: "nfs"},
			{StorageID: 1, StorageName: "default", StorageType: "local", OnNode: true},
			{StorageID: 4, StorageName: "broken", StorageType: "nfs", Unavailable: "storage_unavailable"},
		},
		Providers: []WeightSource{{Provider: "huggingface", RepoID: "r", Suggested: true}},
		Targets:   []StorageTarget{{StorageID: 1, StorageName: "default", StorageType: "local"}, {StorageID: 2, StorageName: "model-archive", StorageType: "nfs", Default: true}},
	}, ImageFacts{})
	var keys []string
	for _, c := range r.Present {
		keys = append(keys, c.Key)
	}
	if strings.Join(keys, ",") != "present:1,present:2,present:3,present:4" {
		t.Fatalf("present order = %v", keys)
	}
	if !r.Present[0].Recommended || r.Present[1].Recommended || r.Present[3].Recommended {
		t.Fatalf("only the on-node local storage may be recommended: %+v", r.Present)
	}
	// The provider says which weights run, not where a node fetches them,
	// so the suggested one stays recommended whatever storages hold a copy.
	if !r.Providers[0].Recommended {
		t.Fatal("the suggested provider must stay recommended")
	}
	if len(r.Targets) != 2 || r.Targets[0].StorageID != 2 || !r.Targets[0].Recommended || !r.Targets[0].Default {
		t.Fatalf("targets = %+v, want the default storage first and recommended", r.Targets)
	}
}

func TestResolvePresentRecommendsNothingWhenEveryStorageIsUnavailable(t *testing.T) {
	r := Resolve(WeightFacts{
		Present:   []StoragePresence{{StorageID: 2, StorageName: "model-archive", StorageType: "nfs", Unavailable: "storage_unavailable"}},
		Providers: []WeightSource{{Provider: "modelscope", RepoID: "r", Suggested: true}},
	}, ImageFacts{})
	if r.Present[0].Recommended {
		t.Fatalf("unavailable storage recommended: %+v", r.Present)
	}
	if !r.Providers[0].Recommended {
		t.Fatal("the suggested provider must be recommended")
	}
}

func TestResolveCarriesTheImageFactsThrough(t *testing.T) {
	cached := true
	r := Resolve(WeightFacts{}, ImageFacts{Canonical: "gpuworks/runner:r1", Cached: &cached})
	if r.Image.Canonical != "gpuworks/runner:r1" || r.Image.Cached == nil || !*r.Image.Cached {
		t.Fatalf("image = %+v", r.Image)
	}
}
