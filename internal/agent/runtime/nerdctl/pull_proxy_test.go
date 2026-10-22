package nerdctl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func TestPullImageExplicitDirectClearsProxyEnvironment(t *testing.T) {
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(name, "http://ambient.invalid:3128")
	}
	t.Setenv("NO_PROXY", "*")
	log := filepath.Join(t.TempDir(), "environment")
	installFakeNerdctl(t, "[ \"$*\" = '-n gpuworks pull registry.example/model:v1' ] || exit 42\nenv > '"+log+"'\necho pulled")
	if err := New().PullImage(runtime.WithPullProxy(context.Background(), ""), "registry.example/model:v1", nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ambient.invalid") {
		t.Fatal("explicit direct image pull inherited a proxy")
	}
}

func TestPullImageRejectsInvalidProxyBeforeStartingCLI(t *testing.T) {
	log := filepath.Join(t.TempDir(), "called")
	installFakeNerdctl(t, "touch '"+log+"'")
	err := New().PullImage(runtime.WithPullProxy(context.Background(), "ftp://user:secret@proxy.example:21"), "registry.example/model:v1", nil)
	if err == nil {
		t.Fatal("unsupported proxy scheme was accepted")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaked credentials: %v", err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("CLI ran before proxy validation")
	}
}
