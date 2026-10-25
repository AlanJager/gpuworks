package store

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

func TestCreateDeploymentWithTemplateInferenceConfigRollsBackThenCommits(t *testing.T) {
	st := newTestStore(t)
	model, err := st.UpsertModel(&Model{
		Source: "modelscope", SourceID: "acme/atomic", Revision: "main", Name: "atomic",
		Operations: legacyChatPlaceholderFixture,
	})
	if err != nil {
		t.Fatal(err)
	}
	sub := st.Bus.Subscribe(8, "model", "deployment")
	defer sub.Close()
	if err := st.db.Exec(`CREATE TRIGGER fail_template_modalities BEFORE UPDATE OF input_modalities ON models
		WHEN NEW.input_modalities = '["text"]'
		BEGIN SELECT RAISE(ABORT, 'injected template modalities failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	reqs := nodelabel.Requirements{"pool": {"b", "a", "b"}}
	_, err = st.CreateDeploymentWithTemplateInferenceConfig(
		&Deployment{Name: "atomic", ServedName: "atomic", ModelID: model.ID}, reqs, model.ID,
		`[{"type":"openai.embeddings"}]`, `["text"]`,
	)
	if err == nil || !strings.Contains(err.Error(), "injected template modalities failure") {
		t.Fatalf("create error = %v", err)
	}
	var deployments, requirements int64
	if err := st.db.Model(&Deployment{}).Count(&deployments).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.db.Model(&DeploymentNodeRequirement{}).Count(&requirements).Error; err != nil {
		t.Fatal(err)
	}
	if deployments != 0 || requirements != 0 {
		t.Fatalf("failed create left %d deployments and %d requirements", deployments, requirements)
	}
	afterFailure, err := st.GetModel(model.ID)
	if err != nil || afterFailure.Operations != legacyChatPlaceholderFixture || afterFailure.InputModalities != "" {
		t.Fatalf("failed create changed the model: %+v, %v", afterFailure, err)
	}
	select {
	case event := <-sub.C():
		t.Fatalf("rolled-back create published %+v", event)
	default:
	}

	if err := st.db.Exec(`DROP TRIGGER fail_template_modalities`).Error; err != nil {
		t.Fatal(err)
	}
	created, err := st.CreateDeploymentWithTemplateInferenceConfig(
		&Deployment{Name: "atomic", ServedName: "atomic", ModelID: model.ID}, reqs, model.ID,
		`[{"type":"openai.embeddings"}]`, `["text"]`,
	)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	storedModel, err := st.GetModel(model.ID)
	if err != nil || storedModel.Operations != `[{"type":"openai.embeddings"}]` || storedModel.InputModalities != `["text"]` {
		t.Fatalf("committed model = %+v, %v", storedModel, err)
	}
	storedReqs, err := st.DeploymentRequirements(created.ID)
	if err != nil || !reflect.DeepEqual(storedReqs, nodelabel.Requirements{"pool": {"a", "b"}}) {
		t.Fatalf("committed requirements = %v, %v", storedReqs, err)
	}
	nextEvent := func() (string, string, string) {
		t.Helper()
		select {
		case event := <-sub.C():
			return event.Topic, event.Key, event.Reason
		case <-time.After(time.Second):
			t.Fatal("committed create did not publish its events")
			return "", "", ""
		}
	}
	if topic, key, reason := nextEvent(); topic != "model" || key != uintKey(model.ID) || reason != "inference_config" {
		t.Fatalf("first event = %s %s %s", topic, key, reason)
	}
	if topic, key, reason := nextEvent(); topic != "deployment" || key != uintKey(created.ID) || reason != "created" {
		t.Fatalf("second event = %s %s %s", topic, key, reason)
	}
	if _, err := st.GetDeployment(created.ID); err != nil {
		t.Fatalf("deployment was not committed before its events: %v", err)
	}
	select {
	case event := <-sub.C():
		t.Fatalf("unexpected extra event: %+v", event)
	default:
	}
}

func TestCreateDeploymentWithTemplateInferenceConfigKeepsUserEdit(t *testing.T) {
	st := newTestStore(t)
	model, err := st.UpsertModel(&Model{
		Source: "modelscope", SourceID: "acme/edited", Revision: "main", Name: "edited",
		Operations: legacyChatPlaceholderFixture,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The create's candidate was computed from the legacy placeholder. A user
	// edit then lands before its transaction; the conditional writes must keep
	// that newer explicit contract rather than restoring either candidate.
	editedOperations := `[{"inputs":[],"type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]`
	if _, err := st.SetModelInferenceConfig(model.ID, editedOperations, `["text"]`); err != nil {
		t.Fatal(err)
	}
	sub := st.Bus.Subscribe(4, "model", "deployment")
	defer sub.Close()
	created, err := st.CreateDeploymentWithTemplateInferenceConfig(
		&Deployment{Name: "edited", ServedName: "edited", ModelID: model.ID}, nil, model.ID,
		`[{"type":"openai.embeddings"}]`, `["image"]`,
	)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetModel(model.ID)
	if err != nil || stored.Operations != editedOperations || stored.InputModalities != `["text"]` {
		t.Fatalf("user edit was overwritten: %+v, %v", stored, err)
	}
	select {
	case event := <-sub.C():
		if event.Topic != "deployment" || event.Key != uintKey(created.ID) || event.Reason != "created" {
			t.Fatalf("event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("deployment created event is missing")
	}
	select {
	case event := <-sub.C():
		t.Fatalf("unchanged model published an event: %+v", event)
	default:
	}
}
