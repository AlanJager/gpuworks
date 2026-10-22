package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/compshare"
)

type stagedImageClient struct{ calls int }

func (f *stagedImageClient) CustomImageProgress(context.Context, provider.Credentials, string, string, string) (float64, error) {
	return 100, nil
}

func (f *stagedImageClient) ListCustomImages(context.Context, provider.Credentials, string, string) ([]compshare.CustomImage, error) {
	f.calls++
	status := "Making"
	if f.calls > 1 {
		status = "Available"
	}
	return []compshare.CustomImage{{ID: "image-1", Status: status}}, nil
}

func TestWaitImageRequiresAvailableAfterProgress100(t *testing.T) {
	client := &stagedImageClient{}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := waitImageWithInterval(ctx, client, provider.Credentials{}, "image-1", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if client.calls != 2 {
		t.Fatalf("status calls=%d, want 2", client.calls)
	}
}

func TestRemoteCommandsUseSudoAndNoRegistryPull(t *testing.T) {
	prepare := remotePrepareCommand(defaultModelPath, defaultDockerImage, []remoteFile{{Path: "config.json", URL: "https://example.invalid/config.json", Size: 1}})
	for _, want := range []string{"sudo -n docker image inspect", "sudo -n python3"} {
		if !strings.Contains(prepare, want) {
			t.Errorf("prepare command does not contain %q", want)
		}
	}
	if strings.Contains(prepare, "docker pull") {
		t.Fatal("prepare command unexpectedly pulls a Docker image")
	}
	validate := remoteValidationCommand(defaultModelPath, defaultDockerImage)
	if !strings.Contains(validate, "sudo -n") || !strings.Contains(validate, "sha256sum -c") {
		t.Fatalf("validation command lacks sudo/checksum: %s", validate)
	}
}
