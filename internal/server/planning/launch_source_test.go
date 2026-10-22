package planning

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The two constructors are the api preview's and the controller's single
// assembly points for the same facts, so for one deployment they have to answer
// the same thing. This is the seam the launch equality test builds on: a
// difference here would make the two paths assemble different launches.
func TestLaunchSourceConstructorsAgreeOnOneDeployment(t *testing.T) {
	dep := &store.Deployment{
		ID: 7, Image: "registry.example/app:v1", TemplateID: 3, TemplateSpecName: "balanced",
		ContainerOverrides: `{"env":{"USER":"1"},"extra_mounts":[{"host_path":"/data","container_path":"/data"}]}`,
	}
	fromRow := LaunchSourceFromDeployment(dep)
	if fromRow.DeploymentID != 7 || fromRow.Image != "registry.example/app:v1" {
		t.Fatalf("row source = %+v", fromRow)
	}
	if fromRow.Overrides.Env["USER"] != "1" || len(fromRow.Overrides.ExtraMounts) != 1 {
		t.Fatalf("row overrides = %+v", fromRow.Overrides)
	}

	// The api decodes the request's override map into the same type and states
	// the image the request confirmed.
	fromRequest := LaunchSourceFromRequest(dep.Image, fromRow.Overrides)
	if fromRequest.DeploymentID != 0 {
		t.Fatalf("a preview names no deployment yet: %+v", fromRequest)
	}
	fromRow.DeploymentID, fromRequest.DeploymentID = 0, 0
	if !reflect.DeepEqual(fromRow, fromRequest) {
		t.Fatalf("constructors disagree:\n row  %+v\n req  %+v", fromRow, fromRequest)
	}

	// A deployment without a template or overrides is the empty shape, not a
	// zero-valued row with stray fields.
	bare := LaunchSourceFromDeployment(&store.Deployment{ID: 8, Image: " app:v2 "})
	if !reflect.DeepEqual(bare, LaunchSource{DeploymentID: 8, Image: "app:v2"}) {
		t.Fatalf("bare source = %+v", bare)
	}
	if got := LaunchSourceFromDeployment(nil); !reflect.DeepEqual(got, LaunchSource{}) {
		t.Fatalf("nil deployment source = %+v", got)
	}
	if got := LaunchSourceFromRequest("", deploy.ContainerOverrides{}); !reflect.DeepEqual(got, LaunchSource{}) {
		t.Fatalf("empty request source = %+v", got)
	}
}
