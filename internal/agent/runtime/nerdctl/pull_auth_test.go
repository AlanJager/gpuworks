package nerdctl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestPullImageLogsInThroughATemporaryDockerConfig(t *testing.T) {
	for _, fail := range []bool{false, true} {
		logDir := t.TempDir()
		t.Setenv("LOG", logDir)
		exit := "0"
		if fail {
			exit = "1"
		}
		installFakeNerdctl(t, `echo "$DOCKER_CONFIG" > "$LOG/config_dir"
cat "$DOCKER_CONFIG/config.json" > "$LOG/config_json"
printf '%s\n' "$*" > "$LOG/args"
exit `+exit)
		ctx := runtime.WithRegistryAuth(context.Background(), []proto.SourceAuth{{Host: "registry.example", Username: "robot", Secret: "tok3n"}})
		err := New().PullImage(ctx, "registry.example/model:v1", nil)
		if (err != nil) != fail {
			t.Fatalf("fail=%v err=%v", fail, err)
		}
		dirRaw, _ := os.ReadFile(filepath.Join(logDir, "config_dir"))
		dir := strings.TrimSpace(string(dirRaw))
		if dir == "" {
			t.Fatal("nerdctl was not given a DOCKER_CONFIG")
		}
		if raw, _ := os.ReadFile(filepath.Join(logDir, "config_json")); !strings.Contains(string(raw), `"registry.example"`) {
			t.Fatalf("config.json = %s", raw)
		}
		if args, _ := os.ReadFile(filepath.Join(logDir, "args")); strings.Contains(string(args), "tok3n") {
			t.Fatalf("secret on the command line: %s", args)
		}
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Fatalf("temporary docker config survived (fail=%v)", fail)
		}
	}
}

func TestPullImageWithoutLoginSetsNoDockerConfig(t *testing.T) {
	logDir := t.TempDir()
	t.Setenv("LOG", logDir)
	t.Setenv("DOCKER_CONFIG", "")
	installFakeNerdctl(t, `echo "[${DOCKER_CONFIG:-}]" > "$LOG/config_dir"`)
	if err := New().PullImage(context.Background(), "registry.example/model:v1", nil); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(logDir, "config_dir")); strings.TrimSpace(string(raw)) != "[]" {
		t.Fatalf("DOCKER_CONFIG leaked into an anonymous pull: %s", raw)
	}
}
