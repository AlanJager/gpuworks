//go:build multiproc

package l2

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// l2AutoindexSite is a plain HTTP file server of the kind an internal mirror
// runs: an Apache mod_autoindex listing whose size column is rounded for
// people, with HEAD, Content-Length, Accept-Ranges and 206 on the files. The
// gap between the listed sizes and the real bodies is what the agent has to
// correct as it downloads.
type l2AutoindexSite struct {
	root  string
	files map[string]string
	sizes map[string]string

	mu       sync.Mutex
	requests []string
}

func (s *l2AutoindexSite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	s.mu.Unlock()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(r.URL.Path, s.root) {
		http.NotFound(w, r)
		return
	}
	rel := strings.Trim(strings.TrimPrefix(r.URL.Path, s.root), "/")
	if rel == "" {
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		w.Header().Set("Content-Type", "text/html;charset=UTF-8")
		var out strings.Builder
		out.WriteString("<html><head><title>Index of " + r.URL.Path + "</title></head><body>\n<table>\n")
		out.WriteString(`<tr><th><a href="?C=N;O=D">Name</a></th><th><a href="?C=S;O=A">Size</a></th></tr>` + "\n")
		out.WriteString(`<tr><td><a href="/">Parent Directory</a></td><td align="right">  - </td></tr>` + "\n")
		for _, name := range slices.Sorted(maps.Keys(s.files)) {
			size := s.sizes[name]
			if size == "" {
				size = fmt.Sprint(len(s.files[name]))
			}
			out.WriteString(`<tr><td><a href="` + name + `">` + name + `</a></td><td align="right">2026-09-01 14:43  </td><td align="right">` + size + `  </td></tr>` + "\n")
		}
		out.WriteString("</table>\n<address>Apache/2.4.6 (CentOS)</address>\n</body></html>\n")
		_, _ = io.WriteString(w, out.String())
		return
	}
	body, ok := s.files[rel]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", `"`+rel+`"`)
	http.ServeContent(w, r, rel, time.Unix(1788000000, 0).UTC(), strings.NewReader(body))
}

func (s *l2AutoindexSite) fetchCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, entry := range s.requests {
		if entry == "GET "+s.root+"/"+path {
			count++
		}
	}
	return count
}

// TestHTTPModelImportOneNode is l2-http-model-import-onenode: a model served
// only by a plain HTTP directory is previewed, registered, downloaded onto a
// real agent process and then left alone by the next reconcile.
func TestHTTPModelImportOneNode(t *testing.T) {
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	serverBin := buildBinary(t, "./cmd/gpuworks")
	agentBin := buildBinary(t, "./cmd/gpuworks-agent")

	site := &l2AutoindexSite{
		root: "/Qwen/Qwen3-0.6B",
		files: map[string]string{
			"config.json":           `{"model_type":"fake","architectures":["FakeForCausalLM"],"max_position_embeddings":2048,"torch_dtype":"bfloat16"}`,
			"tokenizer_config.json": `{"model_type":"fake"}`,
			"model.safetensors":     strings.Repeat("w", 9973),
		},
		// The listing rounds the shard down, exactly as mod_autoindex does.
		sizes: map[string]string{"model.safetensors": "9.7K"},
	}
	mirror := httptest.NewServer(site)
	defer mirror.Close()
	directoryURL := mirror.URL + site.root

	port := freePort(t)
	base := "http://127.0.0.1:" + port
	serverLogPath := filepath.Join(t.TempDir(), "server.log")
	serverLog, err := os.Create(serverLogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverLog.Close() })
	server := serverCommand(t, "multiproc-password", serverBin, "serve", "--config", loopbackOutboundConfig(t), "--addr", "127.0.0.1:"+port, "--data", t.TempDir())
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(server) })
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(response *http.Response) bool {
		return response.StatusCode == http.StatusUnauthorized
	}, serverLogPath)
	adminToken := login(t, base)
	seed, nodeToken := createNode(t, base, adminToken, "http-model-import")

	agentState := filepath.Join(t.TempDir(), "agent-state")
	recordPath := filepath.Join(t.TempDir(), "workload.json")
	agentLogPath := filepath.Join(t.TempDir(), "agent.log")
	agentLog, err := os.Create(agentLogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentLog.Close() })
	startAgent := func() *exec.Cmd {
		command := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken, "--state", agentState)
		command.Env = append(os.Environ(),
			"GPUWORKS_FAKE_GPU=A100x1,81920",
			"GPUWORKS_FAKE_RUNTIME_RECORD="+recordPath,
			"GPUWORKS_LOG_LEVEL=warn",
		)
		command.Stdout, command.Stderr = agentLog, agentLog
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		return command
	}
	agent := startAgent()
	t.Cleanup(func() {
		if agent != nil {
			stopProcess(agent)
		}
	})
	node := waitNode(t, base, adminToken, func(candidate processNode) bool {
		return candidate.UUID == seed.UUID && candidate.State == "online" && candidate.GPUCount == 1
	})

	// Preview: the server reads the directory and proposes the identity.
	previewBody, _ := json.Marshal(map[string]string{"url": directoryURL + "/"})
	preview := struct {
		SourceURL     string `json:"source_url"`
		SourceID      string `json:"source_id"`
		Name          string `json:"name"`
		Arch          string `json:"arch"`
		ContextLength int64  `json:"context_length"`
		WeightBytes   int64  `json:"weight_bytes"`
		FilesCount    int    `json:"files_count"`
	}{}
	decodeAuthedPost(t, base, adminToken, "/api/v1/models/preview-url", previewBody, http.StatusOK, &preview)
	if preview.SourceURL != directoryURL || preview.SourceID != "Qwen/Qwen3-0.6B" || preview.Name != "Qwen3-0.6B" {
		t.Fatalf("preview identity = %+v", preview)
	}
	if preview.Arch != "FakeForCausalLM" || preview.ContextLength != 2048 || preview.FilesCount != 3 {
		t.Fatalf("preview facts = %+v", preview)
	}
	// The listing rounds the shard to 9932 bytes; the preview HEADs it once for
	// the real 9973.
	if preview.WeightBytes != int64(len(site.files["model.safetensors"])) {
		t.Fatalf("preview weight_bytes = %d, want the exact %d", preview.WeightBytes, len(site.files["model.safetensors"]))
	}

	registerBody, _ := json.Marshal(map[string]any{"source_url": directoryURL, "source_id": preview.SourceID})
	model := struct {
		ID        uint   `json:"id"`
		Source    string `json:"source"`
		SourceURL string `json:"source_url"`
		Revision  string `json:"revision"`
	}{}
	decodeAuthedPost(t, base, adminToken, "/api/v1/models/register-url", registerBody, http.StatusCreated, &model)
	if model.ID == 0 || model.Source != "url" || model.SourceURL != directoryURL || model.Revision != "" {
		t.Fatalf("registered model = %+v", model)
	}

	downloadBody, _ := json.Marshal(map[string]string{"node_uuid": node.UUID})
	decodeAuthedPost(t, base, adminToken, fmt.Sprintf("/api/v1/models/%d/download", model.ID), downloadBody, http.StatusAccepted, nil)

	presence := waitModelPresence(t, base, adminToken, model.ID, node.UUID, 60*time.Second)
	exact := int64(0)
	for _, body := range site.files {
		exact += int64(len(body))
	}
	if presence.State != "present" {
		t.Fatalf("model presence = %+v", presence)
	}
	// The listing's rounded total would have been 9932 + 105 + 22; the agent
	// corrects it from the responses, so the recorded size is the real one.
	if presence.SizeBytes != exact || presence.Downloaded != exact {
		t.Fatalf("presence bytes = %d/%d, want the measured %d", presence.Downloaded, presence.SizeBytes, exact)
	}
	// The downloader opens several range connections per file, so the exact
	// count is an implementation detail; what matters is that the shard was
	// fetched at all now and not at all later.
	shardFetches := site.fetchCount("model.safetensors")
	if shardFetches == 0 {
		t.Fatal("the shard was never fetched")
	}
	downloaded := filepath.Join(agentState, "models", "url", "Qwen_Qwen3-0.6B-main", "model.safetensors")
	if body, err := os.ReadFile(downloaded); err != nil || len(body) != len(site.files["model.safetensors"]) {
		t.Fatalf("downloaded shard = %d bytes (err %v), want %d", len(body), err, len(site.files["model.safetensors"]))
	}

	// A second reconcile: the agent restarts, replays desired state and must
	// recognize the completion marker instead of downloading the shard again.
	if err := agent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = agent.Process.Wait()
	agent = nil
	waitNode(t, base, adminToken, func(candidate processNode) bool {
		return candidate.UUID == node.UUID && candidate.State == "offline"
	})
	agent = startAgent()
	if !waitNodeUntil(t, base, adminToken, 30*time.Second, func(candidate processNode) bool {
		return candidate.UUID == node.UUID && candidate.State != "offline"
	}) {
		agentOutput, _ := os.ReadFile(agentLogPath)
		t.Fatalf("agent did not reconnect\n%s", agentOutput)
	}
	again := waitModelPresence(t, base, adminToken, model.ID, node.UUID, 30*time.Second)
	if again.State != "present" || again.SizeBytes != exact {
		t.Fatalf("presence after reconnect = %+v", again)
	}
	if fetched := site.fetchCount("model.safetensors"); fetched != shardFetches {
		t.Fatalf("the shard was fetched %d more times; the second reconcile re-downloaded it", fetched-shardFetches)
	}
}

type l2ModelPresence struct {
	NodeUUID   string `json:"node_uuid"`
	State      string `json:"state"`
	SizeBytes  int64  `json:"size_bytes"`
	Downloaded int64  `json:"downloaded"`
	Message    string `json:"message"`
}

func waitModelPresence(t *testing.T, base, token string, modelID uint, nodeUUID string, timeout time.Duration) l2ModelPresence {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last l2ModelPresence
	for time.Now().Before(deadline) {
		request, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/models/%d", base, modelID), nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := processHTTPClient.Do(request)
		if err == nil {
			var model struct {
				Nodes []l2ModelPresence `json:"nodes"`
			}
			_ = json.NewDecoder(response.Body).Decode(&model)
			response.Body.Close()
			for _, presence := range model.Nodes {
				if presence.NodeUUID != nodeUUID {
					continue
				}
				last = presence
				if presence.State == "present" {
					return presence
				}
				if presence.State == "failed" || presence.State == "insufficient_disk_space" {
					t.Fatalf("model download failed: %+v", presence)
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("model %d did not become present on %s; last presence %+v", modelID, nodeUUID, last)
	return last
}

func decodeAuthedPost(t *testing.T, base, token, path string, body []byte, wantStatus int, out any) {
	t.Helper()
	response, err := authedPost(t, base, token, path, body)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != wantStatus {
		t.Fatalf("POST %s status=%d body=%s", path, response.StatusCode, raw)
	}
	if out == nil {
		return
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("POST %s body=%s: %v", path, raw, err)
	}
}
