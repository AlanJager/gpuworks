package planning

import (
	"encoding/json"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// LaunchSource is the launch-side half of a deployment that is not an estimate
// input: which deployment this is, the runtime image it confirmed and the
// container override layer.
//
// The api preview and the controller build one each - from a request, and from
// a stored row - so both hand the launch assembly the same shape instead of a
// row struct one of them does not have. It is a value the server derives and
// never a column: no json tags, no dto, nothing persisted (plan §3.2 allows
// in-memory types).
type LaunchSource struct {
	// DeploymentID names the deployment in messages; a preview that has not
	// created one yet leaves it zero.
	DeploymentID uint
	// Image is the runtime image ref the deployment runs.
	Image string
	// Overrides is the deployment's own container override layer.
	Overrides deploy.ContainerOverrides
}

// LaunchSourceFromDeployment reads the source off a stored deployment row. It
// is the controller's one assembly point for these facts.
func LaunchSourceFromDeployment(dep *store.Deployment) LaunchSource {
	if dep == nil {
		return LaunchSource{}
	}
	source := LaunchSource{DeploymentID: dep.ID, Image: strings.TrimSpace(dep.Image)}
	if strings.TrimSpace(dep.ContainerOverrides) != "" {
		_ = json.Unmarshal([]byte(dep.ContainerOverrides), &source.Overrides)
	}
	return source
}

// LaunchSourceFromRequest reads the same facts off an evaluation or create
// request: the image it states and the override layer the api already decoded. It is the preview's one assembly
// point, which is why it takes the decoded values rather than the request type:
// the wire type belongs to the api layer and this package must not read it.
func LaunchSourceFromRequest(image string, overrides deploy.ContainerOverrides) LaunchSource {
	return LaunchSource{Image: strings.TrimSpace(image), Overrides: overrides}
}
