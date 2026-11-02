package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/store"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

func templateUpdateFixture(t *testing.T) ([]byte, string) {
	t.Helper()
	model := inferencecatalog.InferenceModel{
		ModelID: "acme/model", Source: "upstream",
		Specs: []inferencecatalog.InferenceSpec{{
			Name: "default", Engine: "vllm", Image: "vllm:test",
			Weights: inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: "acme/model"}}},
			GPUs:    inferencecatalog.GPUSelector{Vendor: "nvidia"},
		}},
	}
	data, err := inferencecatalog.MarshalModelYAML(model)
	if err != nil {
		t.Fatal(err)
	}
	tree := fstest.MapFS{
		"manifest.yaml":                {Data: []byte("schema_version: 1\npublished_at: 2026-11-02T00:00:00Z\nmodels: 1\nspecs: 1\n")},
		"models/acme/model/model.yaml": {Data: data},
	}
	var archive bytes.Buffer
	if err := inferencecatalog.PackPackage(&archive, tree); err != nil {
		t.Fatal(err)
	}
	digest, err := inferencecatalog.PackageDigest(tree)
	if err != nil {
		t.Fatal(err)
	}
	return archive.Bytes(), digest
}

func TestUpdatesTemplateCompatibilityCachesAndRefreshes(t *testing.T) {
	run := newImageManifestRun(t)
	channel := newTestChannel(t)
	archive, digest := templateUpdateFixture(t)
	channel.publishTemplateCatalog(archive, digest, inferencecatalog.PackageSchemaVersion)
	run.useChannel(channel)
	for range 2 {
		if c := getUpdateStatus(t, run).TemplateCatalog; !c.Compatible || !c.UpdateAvailable || c.Latest != channel.index.TemplateCatalog.Version {
			t.Fatalf("template catalog = %+v", c)
		}
	}
	if got := strings.Join(channel.requested(), ","); got != "nightly/index.json,"+channel.templatePath {
		t.Fatalf("polling must reuse the catalog check: %s", got)
	}
	// Checking is read-only, even for a valid package.
	if _, err := run.store.InferenceModelByModelID("acme/model"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("the update check imported the published model")
	}
	if got, err := run.store.UpstreamInferencePackageDigest(); err != nil || got != "" {
		t.Fatalf("the update check changed the catalog digest: %q, %v", got, err)
	}

	// A new index claiming a future schema is incompatible without a download.
	channel.publishTemplateCatalog(archive, digest, inferencecatalog.PackageSchemaVersion+1)
	rec := run.do(http.MethodGet, "/api/v1/updates?refresh=true", "", "")
	var status dto.UpdateStatus
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &status) != nil || status.TemplateCatalog.Compatible {
		t.Fatalf("future schema = %d %s", rec.Code, rec.Body.String())
	}
	if len(channel.requested()) != 3 {
		t.Fatalf("future schema downloaded a package: %v", channel.requested())
	}
	channel.publishTemplateCatalog(archive, digest, inferencecatalog.PackageSchemaVersion)
	rec = run.do(http.MethodGet, "/api/v1/updates?refresh=true", "", "")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &status) != nil || !status.TemplateCatalog.Compatible {
		t.Fatalf("refreshed catalog = %d %s", rec.Code, rec.Body.String())
	}
	if len(channel.requested()) != 5 {
		t.Fatalf("refresh did not recheck the package: %v", channel.requested())
	}
}

func TestUpdatesTemplateCompatibilityDownloadFailures(t *testing.T) {
	for _, fault := range []string{"missing", "altered"} {
		t.Run(fault, func(t *testing.T) {
			run := newImageManifestRun(t)
			channel := newTestChannel(t)
			archive, digest := templateUpdateFixture(t)
			channel.publishTemplateCatalog(archive, digest, inferencecatalog.PackageSchemaVersion)
			channel.publishImageCatalog([]byte(upstreamManifest))
			if fault == "missing" {
				channel.fail(channel.templatePath, http.StatusNotFound)
			} else {
				channel.replace(channel.templatePath, append([]byte(nil), archive[:len(archive)-1]...))
			}
			run.useChannel(channel)
			for range 2 {
				rec := run.do(http.MethodGet, "/api/v1/updates", "", "")
				var status dto.UpdateStatus
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &status) != nil {
					t.Fatalf("failed package download = %d %s", rec.Code, rec.Body.String())
				}
				c := status.TemplateCatalog
				if !status.Available || !status.Server.UpdateAvailable || !status.ImageCatalog.UpdateAvailable || c.Compatible || c.CheckError == nil || c.CheckError.Code != ErrorUpdateDownloadFailed || c.CheckError.Params["file"] != channel.templatePath || c.CheckError.Params["detail"] == "" {
					t.Fatalf("a template download failure must preserve other updates: %s", rec.Body.String())
				}
			}
			requests := len(channel.requested())
			if requests != 3 { // one index and two download attempts; polling retries neither
				t.Fatalf("a cached failed check was retried: %v", channel.requested())
			}
			channel.fail(channel.templatePath, 0)
			channel.replace(channel.templatePath, archive)
			rec := run.do(http.MethodGet, "/api/v1/updates?refresh=true", "", "")
			var status dto.UpdateStatus
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &status) != nil || !status.TemplateCatalog.Compatible || status.TemplateCatalog.CheckError != nil || len(channel.requested()) != requests+2 {
				t.Fatalf("retry after repair = %d %s, requests %v", rec.Code, rec.Body.String(), channel.requested())
			}
		})
	}
}

func TestUpdatesTemplateWithAutoCheckOffUsesManualResult(t *testing.T) {
	run := newImageManifestRun(t)
	channel := newTestChannel(t)
	archive, digest := templateUpdateFixture(t)
	channel.publishTemplateCatalog(archive, digest, inferencecatalog.PackageSchemaVersion)
	run.deps.Server = &autoCheckServer{manual: true}
	run.useChannel(channel)
	if status := getUpdateStatus(t, run); status.Available || len(channel.requested()) != 0 {
		t.Fatalf("a page load contacted the sources: %+v, %v", status, channel.requested())
	}
	rec := run.do(http.MethodGet, "/api/v1/updates?refresh=true", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("manual check = %d %s", rec.Code, rec.Body.String())
	}
	if c := getUpdateStatus(t, run).TemplateCatalog; !c.Compatible || len(channel.requested()) != 2 {
		t.Fatalf("manual check was not retained: %+v, requests %v", c, channel.requested())
	}
}

func TestTemplateCompatibilityConcurrentChecksAndCancellation(t *testing.T) {
	channel := newTestChannel(t)
	archive, digest := templateUpdateFixture(t)
	channel.publishTemplateCatalog(archive, digest, inferencecatalog.PackageSchemaVersion)
	client := channel.sources().channel()
	fetched, err := client.FetchIndex(context.Background(), "nightly")
	if err != nil {
		t.Fatal(err)
	}
	var check templateUpdateCheck
	if compatible, err := check.check(context.Background(), client, fetched, false); compatible || err != nil || len(channel.requested()) != 1 {
		t.Fatalf("unchecked index with automatic checks off: compatible=%v, err=%v, requests=%v", compatible, err, channel.requested())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := check.check(ctx, client, fetched, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled check = %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if compatible, err := check.check(context.Background(), client, fetched, true); err != nil || !compatible {
				t.Errorf("check after cancellation: compatible=%v, err=%v", compatible, err)
			}
		})
	}
	wg.Wait()
	if len(channel.requested()) != 2 {
		t.Fatalf("concurrent callers downloaded more than once: %v", channel.requested())
	}
}

func TestTemplateCompatibilityRetriesFailedDownloadAfterBackoff(t *testing.T) {
	channel := newTestChannel(t)
	archive, digest := templateUpdateFixture(t)
	channel.publishTemplateCatalog(archive, digest, inferencecatalog.PackageSchemaVersion)
	client := channel.sources().channel()
	fetched, err := client.FetchIndex(context.Background(), "nightly")
	if err != nil {
		t.Fatal(err)
	}
	channel.fail(channel.templatePath, http.StatusServiceUnavailable)
	var check templateUpdateCheck
	for range 2 {
		if compatible, err := check.check(context.Background(), client, fetched, true); compatible || err == nil {
			t.Fatalf("unavailable archive: compatible=%v err=%v", compatible, err)
		}
	}
	if len(channel.requested()) != 3 {
		t.Fatalf("failed check was not cached: %v", channel.requested())
	}
	check.at = time.Now().Add(-distribution.FailedCheckTTL - time.Second)
	channel.fail(channel.templatePath, 0)
	if compatible, err := check.check(context.Background(), client, fetched, false); compatible || err == nil || len(channel.requested()) != 3 {
		t.Fatalf("checks off retried an expired failure: compatible=%v err=%v requests=%v", compatible, err, channel.requested())
	}
	if compatible, err := check.check(context.Background(), client, fetched, true); !compatible || err != nil || len(channel.requested()) != 4 {
		t.Fatalf("expired failure did not recover: compatible=%v err=%v requests=%v", compatible, err, channel.requested())
	}
}

func TestCatalogUpdateCheckErrorWireContract(t *testing.T) {
	const wire = `{"current":"old","latest":"new","update_available":true,"compatible":false,"check_error":{"code":"UPDATE_DOWNLOAD_FAILED","params":{"detail":"missing","file":"catalog/templates/new.tar.gz"},"message":"Catalog check failed"}}`
	var catalog dto.CatalogUpdate
	if err := json.Unmarshal([]byte(wire), &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Compatible || catalog.CheckError == nil || catalog.CheckError.Code != ErrorUpdateDownloadFailed || catalog.CheckError.Params["file"] != "catalog/templates/new.tar.gz" {
		t.Fatalf("decoded catalog = %+v", catalog)
	}
	got, err := json.Marshal(catalog)
	if err != nil || string(got) != wire {
		t.Fatalf("catalog wire = %s, %v", got, err)
	}
}
