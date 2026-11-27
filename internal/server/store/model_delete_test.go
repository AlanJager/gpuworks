package store

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// A model only stopped, empty deployments name is refused with those
// deployments listed; asked to, it goes together with them in one
// transaction. One running deployment refuses the whole deletion and leaves
// every row in place (GWKS-530).
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
	// A failed attempt left behind is history, not a running service.
	if err := s.DB().Create(&Instance{UUID: "old", DeploymentID: stopped[0].ID, State: proto.InstFailed}).Error; err != nil {
		t.Fatal(err)
	}

	_, err = s.DeleteModel(model.ID, false)
	var refused *ModelReferenceError
	if !errors.As(err, &refused) || !errors.Is(err, ErrReferenced) {
		t.Fatalf("err = %v, want a ModelReferenceError", err)
	}
	if len(refused.Deployments) != 2 || !refused.AllIdle() || refused.Deployments[0].Name != "a" || refused.Deployments[0].State != DeployStopped || refused.Deployments[0].Instances != 0 {
		t.Fatalf("refusal = %+v", refused.Deployments)
	}

	running, err := s.CreateDeployment(&Deployment{Name: "c", ServedName: "c", ModelID: model.ID, State: DeployStopped})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB().Create(&Instance{UUID: "live", DeploymentID: running.ID, State: proto.InstRunning}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteModel(model.ID, true); !errors.As(err, &refused) || refused.AllIdle() {
		t.Fatalf("a deployment still running an instance did not refuse: %v", err)
	}
	if _, err := s.GetModel(model.ID); err != nil {
		t.Fatalf("a refused deletion removed the model: %v", err)
	}
	if _, err := s.GetDeployment(stopped[0].ID); err != nil {
		t.Fatalf("a refused deletion removed a deployment: %v", err)
	}

	if err := s.DB().Where("uuid = ?", "live").Delete(&Instance{}).Error; err != nil {
		t.Fatal(err)
	}
	deleted, err := s.DeleteModel(model.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 3 {
		t.Fatalf("deleted %+v, want the three deployments", deleted)
	}
	if _, err := s.GetModel(model.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("model still there: %v", err)
	}
	for _, dep := range append(stopped, running) {
		if _, err := s.GetDeployment(dep.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deployment %d still there: %v", dep.ID, err)
		}
	}
	var leftover int64
	if err := s.DB().Model(&Instance{}).Where("uuid = ?", "old").Count(&leftover).Error; err != nil || leftover != 0 {
		t.Fatalf("the deleted deployment's instance rows stayed: %d err=%v", leftover, err)
	}
}
