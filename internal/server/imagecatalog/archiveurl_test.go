package imagecatalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/test/ocifixture"
)

func TestNormalizeArchiveURLSpellsOneAddressOneWay(t *testing.T) {
	for raw, want := range map[string]string{
		"  https://mirror.example/img.tar  ":       "https://mirror.example/img.tar",
		"https://mirror.example/img.tar#sha":       "https://mirror.example/img.tar",
		"http://10.0.0.5:8080/a/b/img.tar":         "http://10.0.0.5:8080/a/b/img.tar",
		"https://mirror.example/img.tar?token=abc": "https://mirror.example/img.tar?token=abc",
		"HTTPS://Mirror.Example/img.tar":           "https://Mirror.Example/img.tar",
	} {
		got, err := NormalizeArchiveURL(raw)
		if err != nil {
			t.Fatalf("NormalizeArchiveURL(%q) error: %v", raw, err)
		}
		if got != want {
			t.Errorf("NormalizeArchiveURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestNormalizeArchiveURLRejectsUnusableAddresses(t *testing.T) {
	for _, raw := range []string{
		"",
		"   ",
		"ftp://mirror.example/img.tar",
		"file:///srv/img.tar",
		"https://user:secret@mirror.example/img.tar",
		"https://mirror.example",
		"https://mirror.example/",
		"quay.io/gpustack/runner:v1",
		"://broken",
	} {
		if got, err := NormalizeArchiveURL(raw); err == nil {
			t.Errorf("NormalizeArchiveURL(%q) = %q, want an error", raw, got)
		}
	}
}

func TestIsArchiveURLSeparatesArchivesFromRegistryRefs(t *testing.T) {
	for entry, want := range map[string]bool{
		"https://mirror.example/img.tar": true,
		"http://mirror.example/img.tar":  true,
		"  HTTP://mirror/img.tar":        true,
		"quay.io/gpustack/runner:v1":     false,
		"vllm/vllm-openai:v0.8":          false,
		"":                               false,
	} {
		if got := IsArchiveURL(entry); got != want {
			t.Errorf("IsArchiveURL(%q) = %v, want %v", entry, got, want)
		}
	}
}

func TestArchiveURLHostIsTheOperatorFacingName(t *testing.T) {
	if got := ArchiveURLHost("https://containerimages.example:8443/a/b.tar"); got != "containerimages.example:8443" {
		t.Errorf("ArchiveURLHost = %q", got)
	}
}

func TestFetchRequestRecognisesURLSource(t *testing.T) {
	if !(FetchRequest{Source: "url"}).IsURLFetch() || !(FetchRequest{Source: " URL "}).IsURLFetch() {
		t.Error("a url source must be recognised regardless of case and spacing")
	}
	if (FetchRequest{Source: "quay"}).IsURLFetch() || (FetchRequest{}).IsURLFetch() {
		t.Error("a registry source must not be treated as a url fetch")
	}
}

func TestPeekArchiveURLReadsTagsAndSizeWithoutDownloading(t *testing.T) {
	archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{
		RepoTags: []string{"gpustack/runner:cuda13.0-vllm0.25.1"}, LayerBytes: 512 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture, srv := ocifixture.StartArchiveServer(archive)
	defer srv.Close()

	peek, err := PeekArchiveURL(context.Background(), ArchiveHTTPClient(), srv.URL+"/image.tar")
	if err != nil {
		t.Fatalf("PeekArchiveURL: %v", err)
	}
	if !peek.Previewable || len(peek.RepoTags) != 1 || peek.RepoTags[0] != "gpustack/runner:cuda13.0-vllm0.25.1" {
		t.Fatalf("peek = %+v", peek)
	}
	if peek.SizeBytes != int64(len(archive)) {
		t.Fatalf("size = %d, want %d", peek.SizeBytes, len(archive))
	}
	// The whole point is not paying for the archive: only a HEAD and bounded
	// range requests may be made.
	for _, request := range fixture.Requests() {
		if strings.HasPrefix(request, "GET") && !strings.Contains(request, "bytes=") {
			t.Fatalf("previewer issued an unranged GET: %q", request)
		}
	}
}

func TestPeekArchiveURLReportsNotPreviewableWithoutRangeSupport(t *testing.T) {
	archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{RepoTags: []string{"example/app:v1"}, LayerBytes: 256 << 10})
	if err != nil {
		t.Fatal(err)
	}
	_, srv := ocifixture.StartArchiveServer(archive, ocifixture.WithoutRangeSupport())
	defer srv.Close()

	peek, err := PeekArchiveURL(context.Background(), ArchiveHTTPClient(), srv.URL+"/image.tar")
	if err != nil {
		t.Fatalf("PeekArchiveURL: %v", err)
	}
	if peek.Previewable || len(peek.RepoTags) != 0 {
		t.Fatalf("peek = %+v, want not previewable", peek)
	}
	if peek.SizeBytes != int64(len(archive)) {
		t.Fatalf("size = %d, want %d", peek.SizeBytes, len(archive))
	}
}

func TestArchiveURLSizeUsesOneHeadRequest(t *testing.T) {
	archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{RepoTags: []string{"example/app:v1"}})
	if err != nil {
		t.Fatal(err)
	}
	fixture, srv := ocifixture.StartArchiveServer(archive)
	defer srv.Close()

	size, err := ArchiveURLSize(context.Background(), ArchiveHTTPClient(), srv.URL+"/image.tar")
	if err != nil {
		t.Fatalf("ArchiveURLSize: %v", err)
	}
	if size != int64(len(archive)) {
		t.Fatalf("size = %d, want %d", size, len(archive))
	}
	if requests := fixture.Requests(); len(requests) != 1 || !strings.HasPrefix(requests[0], "HEAD") {
		t.Fatalf("requests = %v, want one HEAD", requests)
	}
}

func TestArchiveURLSizeReportsMissingArchive(t *testing.T) {
	_, srv := ocifixture.StartArchiveServer([]byte("ignored"))
	defer srv.Close()
	if _, err := ArchiveURLSize(context.Background(), ArchiveHTTPClient(), srv.URL+"/missing.tar"); err == nil {
		t.Fatal("a 404 archive must be reported as an error")
	}
}

func TestArchiveURLInvalidSeparatesInputFromNetworkFailures(t *testing.T) {
	_, err := PeekArchiveURL(context.Background(), ArchiveHTTPClient(), "ftp://mirror.example/img.tar")
	if err == nil || !ArchiveURLInvalid(err) {
		t.Fatalf("scheme rejection = %v, want an invalid-url error", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err = PeekArchiveURL(context.Background(), ArchiveHTTPClient(), srv.URL+"/image.tar")
	if err == nil || ArchiveURLInvalid(err) {
		t.Fatalf("server failure = %v, want a non-invalid-url error", err)
	}
}

func TestArchiveHTTPClientStopsAfterFiveRedirects(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()
	_, err := ArchiveURLSize(context.Background(), ArchiveHTTPClient(), srv.URL+"/image.tar")
	if err == nil || !strings.Contains(err.Error(), "stopped after 5 redirects") {
		t.Fatalf("redirect loop error = %v, want the 5-redirect bound", err)
	}
}

func TestArchiveHTTPClientRefusesRedirectToAnotherScheme(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ftp://elsewhere.example/img.tar", http.StatusFound)
	}))
	defer srv.Close()
	_, err := ArchiveURLSize(context.Background(), ArchiveHTTPClient(), srv.URL+"/image.tar")
	if err == nil || !strings.Contains(err.Error(), "not http or https") {
		t.Fatalf("redirect scheme error = %v, want the scheme rule re-applied", err)
	}
}
