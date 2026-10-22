package modelmeta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/modelconfig"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

func TestWeightListingTotalPrefersConsolidatedLayout(t *testing.T) {
	files := []modelrepo.File{
		{Path: "consolidated.safetensors", Size: 100},
		{Path: "model-00001-of-00002.safetensors", Size: 60},
		{Path: "model-00002-of-00002.safetensors", Size: 70},
	}
	if got, count := weightListingTotal(files); got != 100 || count != 1 {
		t.Fatalf("weight total=%d count=%d, want 100/1", got, count)
	}
}

func TestInputModalitiesComeFromConfigTowers(t *testing.T) {
	cfg := &modelconfig.Config{VisionConfig: &modelconfig.TowerConfig{}, AudioConfig: &modelconfig.TowerConfig{}}
	if got, want := InputModalities(cfg), []string{"text", "image", "audio"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("InputModalities = %v, want %v", got, want)
	}
	if got := InputModalities(nil); got != nil {
		t.Fatalf("nil config modalities = %v, want nil", got)
	}
}

func TestContextLengthAndPrecisionUseExtendedConfigMetadata(t *testing.T) {
	raw := `{"torch_dtype":"bfloat16","max_position_embeddings":131072,"model_max_length":32768,"quantization_config":{"quant_method":"gptq","bits":4}}`
	cfg, err := modelconfig.Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := ContextLength(cfg, raw); got != 32768 {
		t.Fatalf("ContextLength = %d, want 32768", got)
	}
	if got := Precision(cfg, raw); got != "bfloat16 / gptq 4bit" {
		t.Fatalf("Precision = %q", got)
	}
}

func TestObjectIDTimeFallback(t *testing.T) {
	got := objectIDTime("680da79d48c19d19cf82ab5e")
	if got == nil || got.Unix() != 0x680da79d {
		t.Fatalf("objectIDTime = %v", got)
	}
}

func TestFetchReadsRepositoryDatesAndLogoHints(t *testing.T) {
	testCases := []struct {
		name   string
		source string
		check  func(*Meta) bool
	}{
		{name: "huggingface", source: "huggingface", check: func(meta *Meta) bool {
			return meta.OriginCreatedAt != nil && meta.OriginCreatedAt.Equal(time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)) &&
				meta.OriginUpdatedAt != nil && meta.OriginUpdatedAt.Equal(time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)) &&
				strings.HasSuffix(meta.LogoURL, "/avatars/acme.png")
		}},
		{name: "modelscope", source: "modelscope", check: func(meta *Meta) bool {
			return meta.OriginCreatedAt != nil && meta.OriginCreatedAt.Equal(time.Unix(1746153600, 0).UTC()) &&
				meta.OriginUpdatedAt != nil && meta.OriginUpdatedAt.Equal(time.Unix(1754006400, 0).UTC()) &&
				strings.HasSuffix(meta.LogoURL, "/avatar.png")
		}},
	}
	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/api/models/acme/model":
					_, _ = w.Write([]byte(`{"author":"acme","createdAt":"2025-01-02T03:04:05Z","lastModified":"2025-02-03T04:05:06Z"}`))
				case r.URL.Path == "/api/v1/models/acme/model":
					_, _ = w.Write([]byte(`{"Code":200,"Data":{"Avatar":"http://example.test/avatar.png","CreatedTime":1746153600,"LastUpdatedTime":1754006400}}`))
				case r.URL.Path == "/acme/model/resolve/main/config.json" || r.URL.Path == "/models/acme/model/resolve/main/config.json":
					_, _ = w.Write([]byte(`{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"torch_dtype":"bfloat16","max_position_embeddings":32768}`))
				case r.URL.Path == "/acme/model/resolve/main/README.md" || r.URL.Path == "/models/acme/model/resolve/main/README.md":
					w.Header().Set("Content-Type", "text/plain")
					_, _ = w.Write([]byte("# Model"))
				case r.URL.Path == "/acme/model/resolve/main/model.safetensors.index.json" || r.URL.Path == "/models/acme/model/resolve/main/model.safetensors.index.json":
					_, _ = w.Write([]byte(`{"metadata":{"total_size":1234}}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			meta, err := Fetch(context.Background(), tt.source, "acme/model", "main", "", srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			if !tt.check(meta) || meta.WeightBytes != 1234 || meta.Arch != "Qwen3ForCausalLM" {
				t.Fatalf("metadata = %+v", meta)
			}
		})
	}
}

func TestFetchPinsTheDefaultBranchCommitOnlyWhenNoRevisionWasRequested(t *testing.T) {
	const hfCommit = "2fc0636424b8a1e0c1d1f0d3d1c0a9b8f7e6d5c4"
	const msCommit = "4d58a7b524cd0a41f80a7fbb6a5b22f2fca1e3c9"
	for _, tt := range []struct {
		name, source, revision, want string
		resolveCalls                 int
	}{
		{name: "huggingface default branch", source: "huggingface", revision: "", want: hfCommit, resolveCalls: 1},
		{name: "modelscope default branch", source: "modelscope", revision: "", want: msCommit, resolveCalls: 1},
		{name: "explicit revision is kept", source: "huggingface", revision: "main", want: "", resolveCalls: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/models/acme/model/revision/main":
					calls++
					_, _ = w.Write([]byte(`{"id":"acme/model","sha":"` + hfCommit + `","siblings":[{"rfilename":"config.json"}]}`))
				case "/api/v1/models/acme/model/repo/files":
					calls++
					_, _ = w.Write([]byte(`{"Code":200,"Success":true,"Data":{"LatestCommitter":{"CommittedDate":1745832383,"ShortId":"4d58a7b5"},` +
						`"Files":[{"Path":"config.json","Size":812,"Type":"blob","Revision":"` + msCommit + `","CommittedDate":1745832383}]}}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			meta, err := Fetch(context.Background(), tt.source, "acme/model", tt.revision, "", srv.URL)
			if err != nil {
				t.Fatalf("a metadata fetch must survive an unreachable repository: %v", err)
			}
			if meta.ResolvedRevision != tt.want {
				t.Fatalf("ResolvedRevision = %q, want %q", meta.ResolvedRevision, tt.want)
			}
			if tt.resolveCalls == 0 && calls != 0 {
				t.Fatalf("an explicitly requested revision triggered %d resolution requests", calls)
			}
		})
	}
}

func TestFetchKeepsRegistrationUsableWhenTheRevisionCannotBeResolved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"upstream unavailable"}`))
	}))
	defer srv.Close()
	meta, err := Fetch(context.Background(), "huggingface", "acme/model", "", "", srv.URL)
	if err != nil || meta == nil {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}
	if meta.ResolvedRevision != "" {
		t.Fatalf("ResolvedRevision = %q, want empty on an unreachable repository", meta.ResolvedRevision)
	}
}

func TestSafeMetadataURLRemovesCredentialsAndQuery(t *testing.T) {
	got := safeMetadataURL("https://user:secret@example.test/model?token=secret#fragment")
	if got != "https://example.test/model" {
		t.Fatalf("safeMetadataURL = %q", got)
	}
}
