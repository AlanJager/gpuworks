package engine

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// A teardown that could not finish reaches the control plane as a code rather
// than as a sentence (ADR 0067): the driver's own code when it stated one -
// processes of the container are still on the node, so its cards and ports
// are not free - and a pending removal for every other failure. The English
// text stays the fallback beside it.
func TestTeardownPendingReportsTheDriversCode(t *testing.T) {
	const uuid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	name := containerName(uuid)
	for _, tc := range []struct {
		scenario string
		err      error
		want     string
	}{
		{
			scenario: "processes of the container are still running",
			err:      &runtime.TeardownError{Code: proto.ErrorContainerProcessesNotDrained, Container: name, Err: errors.New("process group of " + name + " is still alive after SIGKILL (kill wait 15s)")},
			want:     proto.ErrorContainerProcessesNotDrained,
		},
		{
			scenario: "any other failure",
			err:      errors.New("rootfs cleanup failed"),
			want:     proto.ErrorContainerRemovalPending,
		},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			driver := &orphanDriver{
				headlessTestDriver: headlessTestDriver{status: runtime.Status{Exists: true, Running: true}},
				removeErr:          map[string]error{name: tc.err},
			}
			var reported []proto.InstanceStatus
			e := newHermeticEngine(driver, func(st proto.InstanceStatus) { reported = append(reported, st) }, slog.New(slog.DiscardHandler), t.TempDir())

			if e.teardown(context.Background(), uuid) {
				t.Fatal("a teardown that left the container behind was reported as done")
			}

			if len(reported) != 1 || reported[0].State != proto.InstRemoving {
				t.Fatalf("reported statuses = %+v, want one removing status", reported)
			}
			failure := reported[0].RuntimeFailure
			if failure == nil {
				t.Fatal("the pending teardown reported no structured reason, only its message")
			}
			if failure.Code != tc.want {
				t.Fatalf("code = %q, want %q", failure.Code, tc.want)
			}
			if failure.Resource != name {
				t.Fatalf("resource = %q, want the container %q", failure.Resource, name)
			}
			if !strings.Contains(failure.Detail, tc.err.Error()) {
				t.Fatalf("detail = %q, want the driver's own account %q", failure.Detail, tc.err.Error())
			}
			if !strings.HasPrefix(reported[0].Message, "teardown pending: ") {
				t.Fatalf("message = %q, want the English fallback kept beside the code", reported[0].Message)
			}
		})
	}
}
