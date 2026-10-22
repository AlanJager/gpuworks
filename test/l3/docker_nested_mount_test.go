//go:build docker

package l3

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
)

func TestDockerNestedReadonlyMountFailureMatchesLaunchPreflight(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		skipDocker(t, err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		skipDocker(t, err)
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	if err := ensureFakeImage(t, root); err != nil {
		skipDocker(t, err)
	}
	parent, child := t.TempDir(), t.TempDir()
	name := fmt.Sprintf("gpuworks-l3-nested-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	// The run fails in milliseconds; the bound only has to outlast a loaded
	// dind runner. 20s did not (pipeline 41422: docker run killed, no output).
	const runBudget = 2 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), runBudget)
	defer cancel()
	command := exec.CommandContext(ctx, "docker", "run", "--name", name, "--rm",
		"--volume", parent+":/model:ro", "--volume", child+":/model/frontend:ro",
		"--entrypoint", "/missing-entrypoint", "gpuworks/fake-engine:test")
	output, runtimeErr := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("docker run did not return within %s: %v", runBudget, runtimeErr)
	}
	if runtimeErr == nil || (!strings.Contains(strings.ToLower(string(output)), "read-only") && !strings.Contains(strings.ToLower(string(output)), "mount")) {
		t.Fatalf("Docker did not expose the expected nested read-only mount failure: err=%v output=%s", runtimeErr, output)
	}

	engine, err := deploy.SelectEngine("vllm")
	if err != nil {
		t.Fatal(err)
	}
	_, err = deploy.ResolveLaunch(deploy.ResolveLaunchInput{
		Engine: engine,
		ComponentMounts: []proto.MountSpec{{
			Source: child, Target: "/model/frontend", ReadOnly: true, Purpose: "model_component:frontend",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), `nested mount targets "/model" and "/model/frontend" overlap`) {
		t.Fatalf("launch preflight did not reject the Docker failure shape: %v", err)
	}
}
