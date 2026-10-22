package docker

import (
	"context"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/imagepull"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Lines docker 29.8.1 printed for a real pull through a terminal (2026-10-20,
// artifacts/archive/2026-10-20-t73-nerdctl-pull-progress/evidence), and the
// layer statuses an older docker prints that one did not show.
var realDockerPullLines = []string{
	"1.26: Pulling from library/golang",
	"4f4fb700ef54: Pulling fs layer",
	"4f4fb700ef54: Already exists",
	"8f7a3eec6e08: Downloading [=====================================>             ]  50.44MB/67.35MB",
	"4846f2900730: Download complete",
	"4846f2900730: Extracting 1 s",
	"4f4fb700ef54: Pull complete",
	"Digest: sha256:d7722066f0b60ceccb6c0643cbed1f5f9e15506ac237146c95333504d7805d89",
	"Status: Downloaded newer image for registry.zstack.ai/library/golang:1.26",
	"registry.zstack.ai/library/golang:1.26",
	"25f1d6b1951a: Waiting",
	"25f1d6b1951a: Verifying Checksum",
	"25f1d6b1951a: Retrying in 5 seconds",
	"25f1d6b1951a: Extracting [==>          ]  1.2MB/30MB",
	"25f1d6b1951a: Layer already being pulled by another client. Waiting.",
	"abcdef123456: Pulling from team/image",
}

func TestPullStatusCheckKnowsDockersOwnStatuses(t *testing.T) {
	var c pullStatusCheck
	for _, line := range realDockerPullLines {
		if notice := c.notice(line); notice != "" {
			t.Fatalf("%q was not understood: %q", line, notice)
		}
	}
}

// A layer status docker has not printed before turns the stall check off,
// once, naming the status but none of the line's bytes.
func TestPullStatusCheckReportsAnUnknownStatusOnce(t *testing.T) {
	var c pullStatusCheck
	notice := c.notice("25f1d6b1951a: Unpacking [==>   ]  1.2MB/30MB")
	if notice != runtime.PullLineUnreadable(`docker layer status "Unpacking"`) {
		t.Fatalf("notice = %q", notice)
	}
	if again := c.notice("25f1d6b1951a: Unpacked"); again != "" {
		t.Fatalf("second notice = %q", again)
	}
	if strings.Contains(notice, "MB") {
		t.Fatalf("the notice carries a byte count: %q", notice)
	}
}

// End to end: a docker that renamed how a layer finishes downloads, then
// unpacks for far longer than the stall timeout. The pull must succeed.
func TestWalkLetsADockerPullWithAnUnknownStatusFinish(t *testing.T) {
	oldStall, oldBeat := imagepull.StallTimeout, imagepull.HeartbeatInterval
	imagepull.StallTimeout, imagepull.HeartbeatInterval = 150*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { imagepull.StallTimeout, imagepull.HeartbeatInterval = oldStall, oldBeat })
	installFakeDocker(t, `printf '25f1d6b1951a: Pulling fs layer\n'
printf '25f1d6b1951a: Downloading [=====>    ]  1.049MB/3.624MB\n'
i=0; while [ $i -lt 60 ]; do printf '25f1d6b1951a: Unpacking %ds\n' $i; sleep 0.01; i=$((i+1)); done
printf '25f1d6b1951a: Pull complete\n'`)
	var unreadable bool
	_, err := imagepull.New(imagepull.Config{}).Walk(context.Background(), New(),
		[]proto.ImagePullCandidate{{Ref: "example.com/team/image:v1"}}, imagepull.Options{}, imagepull.Hooks{
			Line: func(_, line string, _ imagepull.Snapshot, _ bool) {
				if runtime.IsPullLineUnreadable(line) {
					unreadable = true
				}
			},
		})
	if err != nil {
		t.Fatalf("a pull with an unknown layer status was given up on: %v", err)
	}
	if !unreadable {
		t.Fatal("the unknown status was not reported as unreadable")
	}
}
