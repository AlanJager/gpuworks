package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func TestPullImageRejectsProxyChoiceThatDaemonCannotHonor(t *testing.T) {
	log := filepath.Join(t.TempDir(), "pull")
	installFakeDocker(t, "case \"$*\" in 'info --format {{json .}}') printf '%s\\n' '{\"HttpProxy\":\"\",\"HttpsProxy\":\"\",\"NoProxy\":\"\"}';; 'pull registry.example/model:v1') touch '"+log+"';; *) exit 42;; esac")
	ctx := runtime.WithPullProxy(context.Background(), "http://user:secret@selected.example:3128")
	err := New().PullImage(ctx, "registry.example/model:v1", nil)
	if err == nil || !strings.Contains(err.Error(), "Docker daemon") {
		t.Fatalf("unsupported scoped proxy must be rejected: %v", err)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "user") {
		t.Fatalf("error leaked proxy credentials: %v", err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("docker pull ran with an unsupported network choice")
	}
}

func TestPullImageHonorsObservedDaemonPolicy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		info      string
		proxy     string
		explicit  bool
		wantError bool
	}{
		{name: "inherited skips inspection", info: "invalid", explicit: false},
		{name: "direct without daemon proxies", info: `{"HttpProxy":"","HttpsProxy":"","NoProxy":""}`, explicit: true},
		{name: "direct rejects daemon proxy", info: `{"HttpProxy":"http://proxy.example:3128","HttpsProxy":"http://proxy.example:3128","NoProxy":"*"}`, explicit: true, wantError: true},
		{name: "matching proxy", info: `{"HttpProxy":"http://proxy.example:3128","HttpsProxy":"http://proxy.example:3128","NoProxy":""}`, proxy: "proxy.example:3128", explicit: true},
		{name: "proxy bypass cannot honor forced proxy", info: `{"HttpProxy":"http://proxy.example:3128","HttpsProxy":"http://proxy.example:3128","NoProxy":"*"}`, proxy: "http://proxy.example:3128", explicit: true, wantError: true},
		{name: "masked credentials are unverifiable", info: `{"HttpProxy":"http://xxxxx:xxxxx@proxy.example:3128","HttpsProxy":"http://xxxxx:xxxxx@proxy.example:3128","NoProxy":""}`, proxy: "http://xxxxx:xxxxx@proxy.example:3128", explicit: true, wantError: true},
		{name: "missing https proxy", info: `{"HttpProxy":"http://proxy.example:3128","HttpsProxy":"","NoProxy":""}`, proxy: "http://proxy.example:3128", explicit: true, wantError: true},
		{name: "missing fields unknown", info: `{}`, explicit: true, wantError: true},
		{name: "malformed info", info: `bad info secret`, explicit: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GPUWORKS_TEST_DOCKER_INFO", tc.info)
			log := filepath.Join(t.TempDir(), "calls")
			installFakeDocker(t, "printf '%s\\n' \"$*\" >> '"+log+"'\ncase \"$*\" in 'info --format {{json .}}') printf '%s\\n' \"$GPUWORKS_TEST_DOCKER_INFO\";; 'pull registry.example/model:v1') echo pulled;; *) exit 42;; esac")
			ctx := context.Background()
			if tc.explicit {
				ctx = runtime.WithPullProxy(ctx, tc.proxy)
			}
			err := New().PullImage(ctx, "registry.example/model:v1", nil)
			if (err != nil) != tc.wantError {
				t.Fatalf("pull error=%v, wantError=%v", err, tc.wantError)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("daemon response leaked into error")
			}
			data, readErr := os.ReadFile(log)
			if readErr != nil {
				t.Fatal(readErr)
			}
			pulled := strings.Contains(string(data), "pull registry.example/model:v1")
			if pulled == tc.wantError {
				t.Fatalf("unexpected pull execution: %s", data)
			}
			if !tc.explicit && strings.Contains(string(data), "info") {
				t.Fatal("inherited pull unnecessarily inspected daemon proxy")
			}
		})
	}
}

func TestPullImageDaemonPolicyFailureAndCancellation(t *testing.T) {
	installFakeDocker(t, "echo secret-daemon-info >&2; exit 23")
	err := New().PullImage(runtime.WithPullProxy(context.Background(), ""), "registry.example/model:v1", nil)
	if err == nil || !strings.Contains(err.Error(), "inspect Docker daemon proxy policy") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unexpected info failure: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = New().PullImage(runtime.WithPullProxy(ctx, ""), "registry.example/model:v1", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}
