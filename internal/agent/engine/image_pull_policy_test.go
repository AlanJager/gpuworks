package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

type pullPolicyDriver struct {
	imageToleranceDriver
	environment chan []string
}

func (d *pullPolicyDriver) PullImage(ctx context.Context, _ string, _ func(string)) error {
	d.environment <- runtime.PullProxyEnv(ctx, []string{"PATH=/bin", "HTTP_PROXY=http://ambient:3128", "ALL_PROXY=socks5://ambient:1080", "NO_PROXY=*"})
	return fmt.Errorf("fixture registry unavailable")
}

func TestDesiredStateImagePullPreservesDirectProxyAndInheritance(t *testing.T) {
	for _, tc := range []struct {
		name, wire       string
		direct, selected bool
	}{
		{name: "direct", wire: `{"uuid":"direct","image":"registry.example/model:v1","desired_state":"running","image_pull_explicit":true}`, direct: true},
		{name: "inherit", wire: `{"uuid":"inherit","image":"registry.example/model:v1","desired_state":"running"}`},
		{name: "proxy", wire: `{"uuid":"proxy","image":"registry.example/model:v1","desired_state":"running","image_pull_explicit":true,"image_pull_proxy":"http://selected:3128"}`, selected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var spec proto.InstanceDesiredState
			if err := json.Unmarshal([]byte(tc.wire), &spec); err != nil {
				t.Fatal(err)
			}
			driver := &pullPolicyDriver{environment: make(chan []string, 1)}
			failed := make(chan proto.InstanceStatus, 1)
			engine := NewWithProber(driver, func(st proto.InstanceStatus) {
				if st.State == proto.InstFailed {
					failed <- st
				}
			}, slog.Default(), t.TempDir(), injectionProber{})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			engine.SetDesiredFull(ctx, []proto.InstanceDesiredState{spec})
			select {
			case env := <-driver.environment:
				text := strings.Join(env, "\n")
				switch {
				case tc.direct:
					if text != "PATH=/bin" {
						t.Fatalf("direct pull environment: %s", text)
					}
				case tc.selected:
					if strings.Contains(text, "ambient") || strings.Contains(text, "NO_PROXY") || !strings.Contains(text, "HTTPS_PROXY=http://selected:3128") {
						t.Fatalf("selected proxy environment: %s", text)
					}
				default:
					if !strings.Contains(text, "HTTP_PROXY=http://ambient:3128") || !strings.Contains(text, "NO_PROXY=*") {
						t.Fatalf("inherited environment: %s", text)
					}
				}
			case <-ctx.Done():
				t.Fatal("desired-state pull never reached runtime")
			}
			select {
			case status := <-failed:
				if !strings.Contains(status.Message, "fixture registry unavailable") {
					t.Fatalf("runtime failure was not propagated: %+v", status)
				}
			case <-ctx.Done():
				t.Fatal("runtime failure was not reported")
			}
		})
	}
}
