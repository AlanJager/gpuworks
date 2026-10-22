package docker

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The fake CLI records which config directory it was given and what that
// directory held, then succeeds or fails as the test asks.
const fakeDockerRecordingAuth = `
case "$*" in
  'pull registry.example/model:v1')
    echo "${DOCKER_CONFIG:-none}" > "$LOG/config_dir"
    if [ -n "${DOCKER_CONFIG:-}" ]; then
      cat "$DOCKER_CONFIG/config.json" > "$LOG/config_json"
      stat -c '%a' "$DOCKER_CONFIG" > "$LOG/dir_mode"
    fi
    printf '%s\n' "$*" > "$LOG/args"
    [ "${FAKE_PULL_FAIL:-}" = 1 ] && { echo "denied: requested access to the resource is denied" >&2; exit 1; }
    echo pulled;;
  *) exit 42;;
esac`

func authCtx() context.Context {
	return runtime.WithRegistryAuth(context.Background(), []proto.SourceAuth{{Host: "registry.example", Username: "robot", Secret: "tok3n"}})
}

func TestPullImageLogsInThroughATemporaryDockerConfig(t *testing.T) {
	for _, fail := range []string{"", "1"} {
		name := "success"
		if fail != "" {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			logDir := t.TempDir()
			t.Setenv("LOG", logDir)
			t.Setenv("FAKE_PULL_FAIL", fail)
			installFakeDocker(t, fakeDockerRecordingAuth)
			err := New().PullImage(authCtx(), "registry.example/model:v1", nil)
			if (err != nil) != (fail != "") {
				t.Fatalf("pull err = %v", err)
			}
			dirRaw, _ := os.ReadFile(filepath.Join(logDir, "config_dir"))
			dir := strings.TrimSpace(string(dirRaw))
			if dir == "" || dir == "none" {
				t.Fatal("docker was not given a DOCKER_CONFIG")
			}
			raw, _ := os.ReadFile(filepath.Join(logDir, "config_json"))
			want := base64.StdEncoding.EncodeToString([]byte("robot:tok3n"))
			if !strings.Contains(string(raw), `"registry.example":{"auth":"`+want+`"}`) {
				t.Fatalf("config.json = %s", raw)
			}
			if mode, _ := os.ReadFile(filepath.Join(logDir, "dir_mode")); strings.TrimSpace(string(mode)) != "700" {
				t.Fatalf("config dir mode = %q", mode)
			}
			if args, _ := os.ReadFile(filepath.Join(logDir, "args")); strings.Contains(string(args), "tok3n") || strings.Contains(string(args), "robot") {
				t.Fatalf("credentials on the command line: %s", args)
			}
			if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
				t.Fatalf("temporary docker config survived the pull (err=%v): %s", err, dir)
			}
			if err != nil && strings.Contains(err.Error(), "tok3n") {
				t.Fatalf("error leaks the secret: %v", err)
			}
		})
	}
}

func TestPullImageCancelledPullStillRemovesDockerConfig(t *testing.T) {
	logDir := t.TempDir()
	t.Setenv("LOG", logDir)
	installFakeDocker(t, `echo "$DOCKER_CONFIG" > "$LOG/config_dir"; sleep 30`)
	ctx, cancel := context.WithCancel(authCtx())
	done := make(chan error, 1)
	go func() { done <- New().PullImage(ctx, "registry.example/model:v1", nil) }()
	for start := time.Now(); ; time.Sleep(10 * time.Millisecond) {
		if raw, _ := os.ReadFile(filepath.Join(logDir, "config_dir")); strings.TrimSpace(string(raw)) != "" {
			break
		}
		if time.Since(start) > 10*time.Second {
			t.Fatal("fake docker never started")
		}
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled pull reported success")
	}
	raw, _ := os.ReadFile(filepath.Join(logDir, "config_dir"))
	if _, err := os.Stat(strings.TrimSpace(string(raw))); !os.IsNotExist(err) {
		t.Fatalf("temporary docker config survived cancellation: %v", err)
	}
}

func TestPullImageWithoutMatchingLoginSetsNoDockerConfig(t *testing.T) {
	logDir := t.TempDir()
	t.Setenv("LOG", logDir)
	t.Setenv("DOCKER_CONFIG", "")
	installFakeDocker(t, fakeDockerRecordingAuth)
	other := runtime.WithRegistryAuth(context.Background(), []proto.SourceAuth{{Host: "elsewhere.example", Username: "u", Secret: "s"}})
	for _, ctx := range []context.Context{context.Background(), other} {
		_ = os.Remove(filepath.Join(logDir, "config_dir"))
		if err := New().PullImage(ctx, "registry.example/model:v1", nil); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(filepath.Join(logDir, "config_dir"))
		if got := strings.TrimSpace(string(raw)); got != "none" {
			t.Fatalf("DOCKER_CONFIG = %q, want unset", got)
		}
	}
}
