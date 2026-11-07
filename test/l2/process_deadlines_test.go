//go:build multiproc

package l2

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

// Deadlines of the process cases that wait on the platform's own retry and
// teardown machinery. They are sums of that machinery's caps, not guesses
// about how fast a runner is: a starved runner (several jobs sharing one
// docker daemon) may spend a whole cap, and a deadline shorter than one cap
// fails exactly the runs the cap was written for.
const (
	// controllerRetryBackoff is how long the controller waits before it tries
	// a failed unit again, and before it pushes a removal that did not
	// complete again (controller.New retryBackoff).
	controllerRetryBackoff = 15 * time.Second
	// controllerMaxAttempts is how many launch attempts a unit gets
	// (controller.New maxAttempts); the last one failing ends the deployment.
	controllerMaxAttempts = 3
	// agentTeardownCap bounds one container teardown on the agent: the
	// cleanup context of Engine.finishEnsure, which holds docker stop and
	// docker rm.
	agentTeardownCap = 30 * time.Second
	// launchAttemptAllowance is what one good launch attempt gets on a loaded
	// host: pull check, container create, sleep prewarm and the first health
	// answers, which take about two seconds on an idle one.
	launchAttemptAllowance = 30 * time.Second
	// slack covers the debounce and polling around the mechanisms above.
	slack = 15 * time.Second

	// instanceRunningDeadline: every attempt but the last may fail at once
	// (an engine port another listener took is refused before any container
	// exists, ENGINE_PORT_IN_USE) and costs one backoff; then comes one good
	// attempt, and a sibling replica launches the same way.
	instanceRunningDeadline = (controllerMaxAttempts-1)*controllerRetryBackoff + 2*launchAttemptAllowance
	// deploymentRemovalDeadline: a teardown that hits the agent's cap is
	// reported as still removing, the controller pushes it again after one
	// backoff, and the second teardown gets its own cap.
	deploymentRemovalDeadline = agentTeardownCap + controllerRetryBackoff + agentTeardownCap + slack
)

// deleteDeployment asks for the deployment's removal and waits until the
// agent has confirmed it. The delete must be accepted: a refused delete would
// otherwise surface only as a wait that never ends.
func deleteDeployment(t *testing.T, base, token string, id uint) {
	t.Helper()
	resp, data := authedDo(t, http.MethodDelete, base, token, "/api/v1/deployments/"+itoa(id), nil)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete deployment %d status=%d body=%s, want 202", id, resp.StatusCode, data)
	}
	waitDeploymentGone(t, base, token, id, deploymentRemovalDeadline)
}

// removalDiagnostics says where a removal that never finished is stuck: the
// deployment's state and what each of its instances last reported.
func removalDiagnostics(t *testing.T, base, token string, id uint) string {
	t.Helper()
	var out []string
	_, dep := authedDo(t, http.MethodGet, base, token, "/api/v1/deployments/"+itoa(id), nil)
	out = append(out, "deployment: "+strings.TrimSpace(string(dep)))
	_, list := authedDo(t, http.MethodGet, base, token, "/api/v1/instances", nil)
	var instances []map[string]any
	if err := apiwire.UnmarshalList(list, &instances); err != nil {
		return strings.Join(out, "\n") + fmt.Sprintf("\ninstances: undecodable (%v): %s", err, list)
	}
	for _, in := range instances {
		if fmt.Sprint(in["deployment_id"]) == itoa(id) {
			out = append(out, fmt.Sprintf("instance %v: state=%v message=%v", in["uuid"], in["state"], in["message"]))
		}
	}
	return strings.Join(out, "\n")
}
