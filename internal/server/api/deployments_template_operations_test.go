package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/modelconfig"
)

// templateOperationsFixture is a template whose spec declares an embeddings
// API, which is not what a model's config.json would suggest on its own.
func templateOperationsFixture(t *testing.T, name string) (*store.Store, *apiHandlers, uint) {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := deploymentFlatFixture()
	fixture.ModelID = "acme/embed"
	fixture.InputModalities = `["text"]`
	fixture.Specs[0].Name = "embed"
	fixture.Specs[0].WeightSources = `[{"provider":"modelscope","repo_id":"Acme/Embed","revision":"0123456789abcdef0123456789abcdef01234567"}]`
	fixture.Specs[0].APIContract = `{"health_endpoint":"/health","operations":[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]}`
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	launchConfigNode(t, st, name+"-node")
	return st, &apiHandlers{d: Deps{Store: st}}, template.ID
}

func createTemplateDeployment(t *testing.T, h *apiHandlers, body map[string]any) dto.Deployment {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(string(raw))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create=%d: %s", rec.Code, rec.Body.String())
	}
	var deployment dto.Deployment
	if err := json.Unmarshal(rec.Body.Bytes(), &deployment); err != nil {
		t.Fatal(err)
	}
	return deployment
}

func modelOperationTypes(t *testing.T, st *store.Store, modelID uint) []string {
	t.Helper()
	model, err := st.GetModel(modelID)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, operation := range inferenceapi.ModelContract(model).Operations {
		types = append(types, operation.Type)
	}
	return types
}

func deploymentOperationTypes(deployment dto.Deployment) []string {
	var types []string
	for _, operation := range deployment.Operations {
		types = append(types, operation.Type)
	}
	return types
}

// A template deployment that resolves its own weights creates the model row.
// The template is the only source of the API the model serves, so that row
// must carry it: the deployment view, the Playground and its proxy read the
// model row and nothing else.
func TestTemplateDeploymentGivesItsNewModelRowTheTemplateOperations(t *testing.T) {
	st, h, templateID := templateOperationsFixture(t, "template_ops_direct")
	deployment := createTemplateDeployment(t, h, map[string]any{
		"template_id": templateID, "template_spec_name": "embed", "node_uuid": "template_ops_direct-node",
	})
	want := fmt.Sprint([]string{inferenceapi.OperationEmbeddings})
	if got := fmt.Sprint(deploymentOperationTypes(deployment)); got != want {
		t.Fatalf("deployment operations=%s want %s", got, want)
	}
	if got := fmt.Sprint(modelOperationTypes(t, st, deployment.ModelID)); got != want {
		t.Fatalf("model row operations=%s want %s", got, want)
	}
	model, err := st.GetModel(deployment.ModelID)
	if err != nil || model.InputModalities != `["text"]` {
		t.Fatalf("model row modalities=%q err=%v", model.InputModalities, err)
	}
}

// The deploy form registers the template's weights before it deploys, and that
// registration reads config.json in the background. Nothing in a config.json
// names an API, so the registration must not settle the operations: the
// template deploying the row does.
func TestQuickRegisteredTemplateWeightsTakeTheTemplateOperations(t *testing.T) {
	st, h, templateID := templateOperationsFixture(t, "template_ops_quick")
	refreshed := make(chan struct{}, 1)
	h.d.FetchModelMeta = func(ctx context.Context, source, repo, revision, _ string, candidates []string) (*modelmeta.Meta, error) {
		defer func() { refreshed <- struct{}{} }()
		return &modelmeta.Meta{
			Config:    &modelconfig.Config{ModelType: "xlm-roberta", Architectures: []string{"XLMRobertaModel"}},
			ConfigRaw: `{"model_type":"xlm-roberta","architectures":["XLMRobertaModel"]}`,
			Arch:      "XLMRobertaModel", WeightBytes: 2 << 30,
		}, nil
	}
	rec := httptest.NewRecorder()
	h.quickRegisterModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models/quick-register", strings.NewReader(
		`{"source":"modelscope","source_id":"Acme/Embed","revision":"0123456789abcdef0123456789abcdef01234567"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("quick register=%d: %s", rec.Code, rec.Body.String())
	}
	var registered dto.Model
	if err := json.Unmarshal(rec.Body.Bytes(), &registered); err != nil {
		t.Fatal(err)
	}
	select {
	case <-refreshed:
	case <-time.After(2 * time.Second):
		t.Fatal("background metadata refresh did not run")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if model, err := st.GetModel(registered.ID); err == nil && model.Arch == "XLMRobertaModel" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	deployment := createTemplateDeployment(t, h, map[string]any{
		"template_id": templateID, "template_spec_name": "embed", "model_id": registered.ID, "node_uuid": "template_ops_quick-node",
	})
	want := fmt.Sprint([]string{inferenceapi.OperationEmbeddings})
	if got := fmt.Sprint(deploymentOperationTypes(deployment)); got != want {
		t.Fatalf("deployment operations=%s want %s", got, want)
	}
	if got := fmt.Sprint(modelOperationTypes(t, st, registered.ID)); got != want {
		t.Fatalf("model row operations=%s want %s", got, want)
	}
}

// Operations someone set on the model are its current contract; a later
// template deployment does not reset them.
func TestTemplateDeploymentKeepsOperationsSetOnTheModel(t *testing.T) {
	st, h, templateID := templateOperationsFixture(t, "template_ops_kept")
	stored := `[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]`
	model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Embed", Revision: "0123456789abcdef0123456789abcdef01234567", Name: "Acme/Embed", Operations: stored})
	if err != nil {
		t.Fatal(err)
	}
	deployment := createTemplateDeployment(t, h, map[string]any{
		"template_id": templateID, "template_spec_name": "embed", "model_id": model.ID, "node_uuid": "template_ops_kept-node",
	})
	after, err := st.GetModel(model.ID)
	if err != nil || after.Operations != stored {
		t.Fatalf("operations=%q want %q err=%v", after.Operations, stored, err)
	}
	if got, want := fmt.Sprint(deploymentOperationTypes(deployment)), fmt.Sprint([]string{inferenceapi.OperationChat}); got != want {
		t.Fatalf("deployment operations=%s want %s", got, want)
	}
}

// A model registered with no inferable task stores [] and the proxy refuses
// every request for it; deploying it with a template that declares the API
// is what makes it usable, and the proxy's contract lookup then allows it.
func TestTemplateDeploymentMakesRegisteredModelWithoutTaskUsable(t *testing.T) {
	st, h, templateID := templateOperationsFixture(t, "template_ops_registered")
	entity := &store.Model{Source: "modelscope", SourceID: "Acme/Embed", Revision: "0123456789abcdef0123456789abcdef01234567", Name: "Acme/Embed"}
	initRegisteredModelContract(entity)
	if entity.Operations != `[]` {
		t.Fatalf("registration without a task stored %q", entity.Operations)
	}
	model, err := st.UpsertModel(entity)
	if err != nil {
		t.Fatal(err)
	}
	if before := inferenceapi.ModelContract(model); inferenceapi.AllowsContractRequest(before, http.MethodPost, "/v1/embeddings") {
		t.Fatalf("precondition: the registered model already exposes embeddings: %+v", before.Operations)
	}
	deployment := createTemplateDeployment(t, h, map[string]any{
		"template_id": templateID, "template_spec_name": "embed", "model_id": model.ID, "node_uuid": "template_ops_registered-node",
	})
	row, err := st.GetDeployment(deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	contract := inferenceapi.ResolveDeploymentContract(st, row, nil)
	if !inferenceapi.AllowsContractRequest(contract, http.MethodPost, "/v1/embeddings") {
		t.Fatalf("proxy contract still refuses the template's operation: %+v", contract.Operations)
	}
}

// A model whose operations are [] is unusable, so the template's declaration
// fills them - whether the [] is what registration inferred from a model with
// no recognizable task, a user's clearing, or a hand-written spelling of it.
func TestTemplateDeploymentFillsEmptyModelOperations(t *testing.T) {
	for name, stored := range map[string]string{
		"registered_without_task": `[]`,
		"spaced":                  " [ ]\n",
		"never_set":               ``,
	} {
		t.Run(name, func(t *testing.T) {
			st, h, templateID := templateOperationsFixture(t, "template_ops_fill_"+name)
			model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Embed", Revision: "0123456789abcdef0123456789abcdef01234567", Name: "Acme/Embed", Operations: stored})
			if err != nil {
				t.Fatal(err)
			}
			deployment := createTemplateDeployment(t, h, map[string]any{
				"template_id": templateID, "template_spec_name": "embed", "model_id": model.ID, "node_uuid": "template_ops_fill_" + name + "-node",
			})
			want := fmt.Sprint([]string{inferenceapi.OperationEmbeddings})
			if got := fmt.Sprint(deploymentOperationTypes(deployment)); got != want {
				t.Fatalf("deployment operations=%s want %s", got, want)
			}
			if got := fmt.Sprint(modelOperationTypes(t, st, model.ID)); got != want {
				t.Fatalf("model row operations=%s want %s", got, want)
			}
		})
	}
}
