package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/engine"
	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/uninstallcheck"
)

const leftoverCheck = "gpuworks-card-smoke-1-dmtplbn8ojg7"

// dockerInventoryFixture answers the three docker calls the uninstall check
// makes the way the CLI does: `ps -a` and `ps` print names, `inspect` prints
// the state template's line, and a container that is not there is the CLI's
// own "No such object" with exit status 1.
const dockerInventoryFixture = `#!/bin/sh
last=""
for arg in "$@"; do last="$arg"; done
case "$*" in
  'ps -a --format {{.Names}}') printf '%s' "$FIXTURE_ALL" ;;
  'ps --format {{.Names}}') printf '%s' "$FIXTURE_RUNNING" ;;
  'inspect --format '*)
    case "$FIXTURE_INSPECT" in
      exited) printf '%s\n' 'false|0|false|' ;;
      created) printf '%s\n' 'false|0|false|' ;;
      failed) printf '%s\n' 'false|137|true|' ;;
      running) printf '%s\n' 'true|0|false|' ;;
      gone) printf '%s\n' "Error: No such object: $last" >&2; exit 1 ;;
      daemon) printf '%s\n' 'Cannot connect to the Docker daemon at unix:///var/run/docker.sock' >&2; exit 1 ;;
      garbage) printf '%s\n' 'template parsing error' ;;
      *) exit 94 ;;
    esac ;;
  *) exit 92 ;;
esac
`

func runUninstallAgainstDocker(t *testing.T, all, running, inspect string) (out string, removed bool, err error) {
	t.Helper()
	state, tools := t.TempDir(), t.TempDir()
	t.Setenv("PATH", tools)
	t.Setenv("FIXTURE_ALL", all)
	t.Setenv("FIXTURE_RUNNING", running)
	t.Setenv("FIXTURE_INSPECT", inspect)
	if err := os.WriteFile(filepath.Join(state, "gpuworks-agent.conf"), []byte("RUNTIME_DRIVER=docker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tools, "docker"), []byte(dockerInventoryFixture), 0o700); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	d := agentUninstallDeps{
		resolve: func(string) ([]string, error) { return []string{state}, nil },
		// The runtime gate alone: GPU and local-state checks have their own tests.
		observe: func(ctx context.Context, state string) []uninstallcheck.Item {
			var items []uninstallcheck.Item
			for _, item := range agentRuntimeChecklist(ctx, state) {
				if strings.HasPrefix(item.ID, "runtime.") || item.ID == "agent.configuration" {
					items = append(items, item)
				}
			}
			return items
		},
		remove: func(context.Context, []string) error { removed = true; return nil },
	}
	err = runAgentUninstall(context.Background(), []string{"--yes"}, strings.NewReader(""), &buf, false, d)
	return buf.String(), removed, err
}

// The field case: a card runtime-check container that exited days ago is all
// that is left in docker. It is reported with the command that removes it and
// the agent is uninstalled.
func TestAnExitedCardCheckContainerDoesNotBlockUninstall(t *testing.T) {
	for _, inspect := range []string{"exited", "created", "failed"} {
		t.Run(inspect, func(t *testing.T) {
			out, removed, err := runUninstallAgainstDocker(t, leftoverCheck+"\nunrelated-container\n", "", inspect)
			if err != nil || !removed {
				t.Fatalf("err = %v, removed = %v, want the agent uninstalled\n%s", err, removed, out)
			}
			for _, want := range []string{
				"[WARN] runtime.docker",
				leftoverCheck + ": card runtime-check container that is not running (it exited or never started)",
				"docker rm " + leftoverCheck + ".",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("checklist lacks %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "unrelated-container") || strings.Contains(out, "ownership unknown") {
				t.Errorf("checklist reports more than the leftover:\n%s", out)
			}
		})
	}
}

// What the runtime does not confirm as stopped stays in the way, and the
// refusal names the check and what to run.
func TestUnverifiedRuntimeCandidatesStillBlockUninstallAndNameTheRemedy(t *testing.T) {
	const instance = "gpuworks-59e3b54200d29e6e6bcce0911b550040"
	for _, tc := range []struct {
		name, all, running, inspect string
		want, absent                []string
	}{
		{"running check container", leftoverCheck + "\n", leftoverCheck + "\n", "running",
			[]string{"[UNKNOWN] runtime.docker", leftoverCheck + ": reported running; state ownership unknown", "docker inspect --format '{{json .Config.Labels}}' " + leftoverCheck}, []string{"runtime-check container that is not running", "rm -f"}},
		{"check container started after the running list", leftoverCheck + "\n", "", "running",
			[]string{"[UNKNOWN] runtime.docker", leftoverCheck + ": retained/stopped record; state ownership unknown"}, []string{"runtime-check container that is not running"}},
		{"check container the daemon will not describe", leftoverCheck + "\n", "", "daemon",
			[]string{"[UNKNOWN] runtime.docker", "docker rm " + leftoverCheck}, []string{"runtime-check container that is not running", "Cannot connect"}},
		{"check container with an unreadable state", leftoverCheck + "\n", "", "garbage",
			[]string{"[UNKNOWN] runtime.docker"}, []string{"runtime-check container that is not running"}},
		{"exited instance container of an unknown agent", instance + "\n", "", "exited",
			[]string{"[UNKNOWN] runtime.docker", instance + ": retained/stopped record; state ownership unknown",
				"docker inspect --format '{{json .Config.Labels}}' " + instance, "docker rm " + instance + ".", "Never remove shared-daemon containers by name prefix."}, nil},
		{"running instance beside an exited check", instance + "\n" + leftoverCheck + "\n", instance + "\n", "exited",
			[]string{"[UNKNOWN] runtime.docker", instance + ": reported running", "docker rm " + leftoverCheck + ".", "stop or migrate its deployment"}, []string{"rm -f", "docker rm " + instance}},
		{"a name that only looks like a check's", "gpuworks-card-smoke-x-1\ngpuworks-card-smoke-1-\ngpuworks-card-smoke-01-abc\n", "", "exited",
			[]string{"[UNKNOWN] runtime.docker", "gpuworks-card-smoke-x-1: retained/stopped record; state ownership unknown"}, []string{"runtime-check container that is not running"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, removed, err := runUninstallAgainstDocker(t, tc.all, tc.running, tc.inspect)
			if err == nil || removed {
				t.Fatalf("err = %v, removed = %v, want the uninstall refused\n%s", err, removed, out)
			}
			if !strings.Contains(err.Error(), "uninstall blocked by runtime.docker:") {
				t.Errorf("refusal %q does not name the check", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("checklist lacks %q:\n%s", want, out)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(out, absent) {
					t.Errorf("checklist has %q:\n%s", absent, out)
				}
			}
		})
	}
}

// A check container its creator removed between the list and the inspection
// is not there to report.
func TestACardCheckContainerRemovedMeanwhileIsNotReported(t *testing.T) {
	out, removed, err := runUninstallAgainstDocker(t, leftoverCheck+"\n", "", "gone")
	if err != nil || !removed || !strings.Contains(out, "[PASS] runtime.docker: No GPUWorks-named workload candidates observed") {
		t.Fatalf("err = %v, removed = %v\n%s", err, removed, out)
	}
}

// inventoryRuntime is the part of a runtime the inventory reads.
type inventoryRuntime struct {
	agentruntime.Driver
	name      string
	all, live []string
	status    map[string]agentruntime.Status
	statusErr error
	// inspect, when set, answers Status: a runtime that takes its time.
	inspect func(ctx context.Context, id string) (agentruntime.Status, error)
	// listDeadline, when set, receives the deadline the list ran under.
	listDeadline *time.Time
}

func (r inventoryRuntime) Name() string { return r.name }
func (r inventoryRuntime) ListContainers(ctx context.Context) ([]string, error) {
	if r.listDeadline != nil {
		*r.listDeadline, _ = ctx.Deadline()
	}
	return r.all, nil
}
func (r inventoryRuntime) RunningContainers(context.Context) ([]string, error) {
	return r.live, nil
}
func (r inventoryRuntime) Status(ctx context.Context, id string) (agentruntime.Status, error) {
	if r.inspect != nil {
		return r.inspect(ctx, id)
	}
	return r.status[id], r.statusErr
}

func TestRuntimeInventoryRemediesUseTheRuntimesOwnCommand(t *testing.T) {
	exited := map[string]agentruntime.Status{leftoverCheck: {Exists: true}, "gpuworks-abc": {Exists: true}}
	item := inspectAgentRuntime(t.Context(), inventoryRuntime{name: "nerdctl", all: []string{leftoverCheck, "gpuworks-abc", "gpuworks-bad name;rm"}, status: exited}, true)
	if item.Status != uninstallcheck.Unknown {
		t.Fatalf("status = %s", item.Status)
	}
	for _, want := range []string{
		"nerdctl -n gpuworks rm " + leftoverCheck + ".",
		"nerdctl -n gpuworks rm gpuworks-abc.",
	} {
		if !strings.Contains(item.Action, want) {
			t.Errorf("action lacks %q: %s", want, item.Action)
		}
	}
	if strings.Contains(item.Action, ";rm") || strings.Contains(strings.Join(item.Details, "\n"), ";rm") {
		t.Errorf("an unsafe name reached the output: %s %v", item.Action, item.Details)
	}

	// A runtime only this agent uses keeps its own rule: records without a
	// live leader prove nothing, whatever they are called.
	local := inspectAgentRuntime(t.Context(), inventoryRuntime{name: "udocker", all: []string{leftoverCheck}, status: exited}, false)
	if local.Status != uninstallcheck.Unknown || strings.Contains(strings.Join(local.Details, "\n"), "runtime-check container") {
		t.Fatalf("local runtime: %+v", local)
	}

	failing := inspectAgentRuntime(t.Context(), inventoryRuntime{name: "docker", all: []string{leftoverCheck}, statusErr: errors.New("inspect timed out")}, true)
	if failing.Status != uninstallcheck.Unknown {
		t.Fatalf("an inspection that failed cleared the container: %+v", failing)
	}
}

// A check container that is not running but is young by its name may be
// another agent's, between its create and its start or between its exit and
// its removal. It is not called finished and it stays in the way until it is
// gone or old.
func TestARecentCardCheckContainerIsNotTakenForFinished(t *testing.T) {
	recent := engine.CardSmokeContainerName(2, time.Now().Add(-20*time.Second))
	item := inspectAgentRuntime(t.Context(), inventoryRuntime{name: "docker", all: []string{recent, leftoverCheck},
		status: map[string]agentruntime.Status{recent: {Exists: true}, leftoverCheck: {Exists: true}}}, true)
	details := strings.Join(item.Details, "\n")
	if item.Status != uninstallcheck.Unknown ||
		!strings.Contains(details, recent+": card runtime-check container not running yet or just ended") ||
		!strings.Contains(details, leftoverCheck+": card runtime-check container that is not running") {
		t.Fatalf("status %s details %v", item.Status, item.Details)
	}
	if !strings.Contains(item.Action, "docker rm "+leftoverCheck+".") || strings.Contains(item.Action, "rm "+recent) || !strings.Contains(item.Action, "run the uninstall again") {
		t.Fatalf("action = %s", item.Action)
	}
}

// The field case at its size: dozens of leftovers on a runtime that answers
// slowly. Each inspection has its own budget, so the later ones are not
// timed out behind the earlier and left unverified; one inspection that
// hangs costs its own budget and only its own container stays in the way.
func TestManyLeftoversOnASlowRuntimeAreEachInspected(t *testing.T) {
	var names []string
	for i := range 40 {
		names = append(names, engine.CardSmokeContainerName(i%8, time.Now().Add(-72*time.Hour-time.Duration(i)*time.Second)))
	}
	// Forty inspections of a slow runtime outlast the budget its lists ran
	// under. Each must run under a deadline of its own, later than the
	// lists' and a full inspection budget away.
	var listDeadline time.Time
	slow := inventoryRuntime{name: "nerdctl", all: names, listDeadline: &listDeadline, inspect: func(ctx context.Context, id string) (agentruntime.Status, error) {
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.After(listDeadline) || time.Until(deadline) < runtimeInspectBudget-time.Second {
			return agentruntime.Status{}, errors.New("inspection shares the lists' deadline")
		}
		return agentruntime.Status{Exists: true}, nil
	}}
	item := inspectAgentRuntime(t.Context(), slow, true)
	if item.Status != uninstallcheck.Warn || len(item.Details) != len(names) {
		t.Fatalf("status %s with %d details, want every leftover seen finished: %v", item.Status, len(item.Details), item.Details)
	}
	for _, name := range names {
		if !strings.Contains(item.Action, name) {
			t.Fatalf("action lacks %s: %s", name, item.Action)
		}
	}

	hung := names[7]
	stuck := inventoryRuntime{name: "docker", all: names, inspect: func(ctx context.Context, id string) (agentruntime.Status, error) {
		if id == hung {
			return agentruntime.Status{}, context.DeadlineExceeded
		}
		return agentruntime.Status{Exists: true}, nil
	}}
	item = inspectAgentRuntime(t.Context(), stuck, true)
	if item.Status != uninstallcheck.Unknown || !strings.Contains(strings.Join(item.Details, "\n"), hung+": retained/stopped record; state ownership unknown") {
		t.Fatalf("status %s details %v", item.Status, item.Details)
	}
	if n := strings.Count(strings.Join(item.Details, "\n"), "card runtime-check container that is not running"); n != len(names)-1 {
		t.Fatalf("%d leftovers seen finished, want all but the one that hung", n)
	}
}

// A shared runtime with nothing of GPUWorks' in it keeps the item's standing
// advice, not an empty one.
func TestASharedRuntimeWithoutCandidatesKeepsItsAdvice(t *testing.T) {
	item := inspectAgentRuntime(t.Context(), inventoryRuntime{name: "docker", all: []string{"unrelated"}}, true)
	if item.Status != uninstallcheck.Pass || item.Action == "" {
		t.Fatalf("item = %+v", item)
	}
}
