package store

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// A model only stopped deployments the controller has finished with name is
// refused with those deployments listed; asked to, it goes together with them
// in one transaction. A stopped deployment that still has an instance row -
// even a stopped or failed one, whose teardown or lease release is not done -
// refuses the whole deletion and leaves every row in place (GWKS-530).
func TestAModelGoesWithTheStoppedDeploymentsThatNameIt(t *testing.T) {
	s := newTestStore(t)
	model, err := s.UpsertModel(&Model{Source: "huggingface", SourceID: "org/shells", Revision: "main", Name: "shells"})
	if err != nil {
		t.Fatal(err)
	}
	var stopped []*Deployment
	for _, name := range []string{"a", "b"} {
		dep, err := s.CreateDeployment(&Deployment{Name: name, ServedName: name, ModelID: model.ID, State: DeployStopped})
		if err != nil {
			t.Fatal(err)
		}
		stopped = append(stopped, dep)
	}

	_, err = s.DeleteModel(model.ID, false)
	var refused *ModelReferenceError
	if !errors.As(err, &refused) || !errors.Is(err, ErrReferenced) {
		t.Fatalf("err = %v, want a ModelReferenceError", err)
	}
	if len(refused.Deployments) != 2 || !refused.AllIdle() || refused.Deployments[0].Name != "a" || refused.Deployments[0].State != DeployStopped || refused.Deployments[0].Instances != 0 {
		t.Fatalf("refusal = %+v", refused.Deployments)
	}

	for _, leftover := range []string{proto.InstFailed, proto.InstStopped, proto.InstRunning} {
		if err := s.DB().Create(&Instance{UUID: "left-" + leftover, DeploymentID: stopped[0].ID, State: leftover}).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := s.DeleteModel(model.ID, true); !errors.As(err, &refused) || refused.AllIdle() || refused.Deployments[0].Instances != 1 {
			t.Fatalf("a stopped deployment with a %s row left did not refuse: %v", leftover, err)
		}
		if _, err := s.GetModel(model.ID); err != nil {
			t.Fatalf("a refused deletion removed the model: %v", err)
		}
		if _, err := s.GetDeployment(stopped[0].ID); err != nil {
			t.Fatalf("a refused deletion removed a deployment: %v", err)
		}
		if err := s.DB().Where("uuid = ?", "left-"+leftover).Delete(&Instance{}).Error; err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := s.DeleteModel(model.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 2 {
		t.Fatalf("deleted %+v, want the two deployments", deleted)
	}
	if _, err := s.GetModel(model.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("model still there: %v", err)
	}
	for _, dep := range stopped {
		if _, err := s.GetDeployment(dep.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deployment %d still there: %v", dep.ID, err)
		}
	}
}
