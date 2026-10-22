package app

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/registryprobe"
	"gpuworks.ai/gpuworks/internal/proto"
)

// A task that carries candidates is fetched from them in order: an address
// that refuses connections is left for the next, the progress message says
// so, the failed address is remembered by the node's prober, and the node's
// URL rewrites - the old way of naming a mirror - no longer apply.
func TestDownloadRepoWalksTheTasksCandidates(t *testing.T) {
	config := []byte(`{"model_type":"test"}`)
	weights := []byte("fixture-weights")
	vocab := bytes.Repeat([]byte("vocabulary\n"), 512)
	var rewritten atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/models/org/model/repo/files":
			writeModelScopeRetryManifest(t, w, r, config, weights, vocab)
		case strings.HasSuffix(r.URL.Path, "/vocab.json"):
			w.Header().Set("Content-Length", fmt.Sprint(len(vocab)))
			_, _ = w.Write(vocab)
		default:
			http.Error(w, "unexpected fixture request "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rewritten.Add(1)
		http.Error(w, "a rewrite target must not be used", http.StatusGone)
	}))
	defer mirror.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + listener.Addr().String()
	_ = listener.Close()

	n, task, target, statuses := newModelDownloadRetryFixture(t, "", config, weights)
	n.registries = registryprobe.Offline()
	n.setNetwork(proto.NetworkConfig{Rewrites: []proto.URLRewrite{{From: server.URL, To: mirror.URL}, {From: dead, To: mirror.URL}}})
	task.Candidates = []proto.ModelFetchCandidate{{Base: dead}, {Base: server.URL, Upstream: true}}
	task.Endpoint = dead
	n.downloadRepo(context.Background(), task, target, target)

	got := lastModelFileStatus(t, statuses)
	if got.State != proto.MFPresent {
		t.Fatalf("final status = %+v", got)
	}
	downloaded, err := os.ReadFile(filepath.Join(target, "vocab.json"))
	if err != nil || !bytes.Equal(downloaded, vocab) {
		t.Fatalf("vocab.json = %d bytes, err=%v", len(downloaded), err)
	}
	if rewritten.Load() != 0 {
		t.Fatalf("a rewrite redirected %d requests although the task named its candidates", rewritten.Load())
	}
	deadHost, liveHost := strings.TrimPrefix(dead, "http://"), strings.TrimPrefix(server.URL, "http://")
	var messages []string
	for _, status := range *statuses {
		if status.Message != "" && (len(messages) == 0 || messages[len(messages)-1] != status.Message) {
			messages = append(messages, status.Message)
		}
	}
	if len(messages) < 2 || messages[0] != "downloading from "+deadHost ||
		!strings.HasPrefix(messages[1], "downloading from "+liveHost+" (switched: "+deadHost+" failed") {
		t.Fatalf("progress messages = %q", messages)
	}
	if !n.registries.Cooled(deadHost) || n.registries.Cooled(liveHost) {
		t.Fatal("the node's prober must remember the address that failed, and only that one")
	}
}

// A server that predates candidates sends at most an endpoint; the node's
// rewrites still redirect it, as they always did.
func TestFetchCandidatesFromAnOlderServer(t *testing.T) {
	net := proto.NetworkConfig{Rewrites: []proto.URLRewrite{{From: "https://huggingface.co/", To: "https://hf-mirror.com/"}}}
	for name, tc := range map[string]struct {
		task proto.ModelFileTask
		want []proto.ModelFetchCandidate
	}{
		"site rewritten": {proto.ModelFileTask{Source: "huggingface"}, []proto.ModelFetchCandidate{{Base: "https://hf-mirror.com"}}},
		"site":           {proto.ModelFileTask{Source: "modelscope"}, []proto.ModelFetchCandidate{{Base: "https://modelscope.cn", Upstream: true}}},
		"endpoint":       {proto.ModelFileTask{Source: "huggingface", Endpoint: "https://hf.corp/"}, []proto.ModelFetchCandidate{{Base: "https://hf.corp"}}},
		"directory":      {proto.ModelFileTask{Source: "url", Endpoint: "http://files.example/m/"}, []proto.ModelFetchCandidate{{Base: "http://files.example/m", Upstream: true}}},
		"planned": {proto.ModelFileTask{Source: "huggingface", Candidates: []proto.ModelFetchCandidate{{Base: "https://huggingface.co", Upstream: true}}},
			[]proto.ModelFetchCandidate{{Base: "https://huggingface.co", Upstream: true}}},
		"unknown source": {proto.ModelFileTask{Source: "local"}, nil},
	} {
		got := fetchCandidates(tc.task, net)
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s: candidates = %+v, want %+v", name, got, tc.want)
		}
	}
}
