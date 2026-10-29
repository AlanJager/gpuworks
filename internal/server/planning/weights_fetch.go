package planning

import (
	"errors"
	"fmt"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/sourceurl"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// WeightsFetchCandidatesFor plans where node downloads a model's weights from:
// the site rules that apply to the node (RulesFor, then URLRules) planned
// against the source's own address (modelrepo.BaseURL). It is the one place
// a download's addresses are decided: a replay, a pushed task and the deploy
// form's forecast all come here.
//
// An HTTP directory (source "url") is fetched from the directory it was
// registered with and nowhere else: its address is where the bytes are, and
// a rule that swaps one site for another cannot produce a working address
// for it. A node the rules leave nowhere to download from is an
// ErrorWeightsFetchForbidden product error wrapping
// sourceurl.ErrNoAllowedSource; model names it in that error.
func WeightsFetchCandidatesFor(rules []sourceurl.Rule, node *store.Node, source, sourceURL, model string) ([]proto.ModelFetchCandidate, error) {
	base := modelrepo.BaseURL(source, sourceURL)
	if base == "" {
		return nil, fmt.Errorf("model %s has no address to download from (source %q)", model, source)
	}
	if source == modelrepo.SourceURL {
		return []proto.ModelFetchCandidate{{Base: base, Upstream: true}}, nil
	}
	planned, err := sourceurl.Plan(base, rules)
	if errors.Is(err, sourceurl.ErrNoAllowedSource) {
		return nil, messages.WrapProductError(messages.ErrorWeightsFetchForbidden, err.Error(),
			map[string]any{"node": node.DisplayName(), "model": model, "route": "/api/v1/pull/source-rules"}, err)
	}
	if err != nil {
		return nil, err
	}
	out := make([]proto.ModelFetchCandidate, 0, len(planned))
	for _, candidate := range planned {
		out = append(out, proto.ModelFetchCandidate{Base: candidate.Base, Upstream: candidate.Upstream})
	}
	return out, nil
}

// ModelWeightsFetch is WeightsFetchCandidatesFor for a registered model,
// reading the node's rules: whether, and from where, node may download it.
func ModelWeightsFetch(st *store.Store, resolve NodeResolver, node *store.Node, model *store.Model) ([]proto.ModelFetchCandidate, error) {
	rules, err := RulesFor(st, resolve, node)
	if err != nil {
		return nil, err
	}
	return WeightsFetchCandidatesFor(URLRules(rules), node, model.Source, model.SourceURL, model.DisplayName())
}

// ServerFetchCandidates are the addresses the control plane reads a model's
// repository at - metadata, logo, the deployment admission check - in the
// order it tries them. The control plane is not a node and no requirement
// selects it: the site rules it reads are the ones chosen for it
// (ControlPlane), planned against the source's own address like a node's.
// An HTTP directory is read where it is. Rules that forbid every address are
// an ErrorModelMetadataForbidden product error wrapping
// sourceurl.ErrNoAllowedSource.
func ServerFetchCandidates(st *store.Store, source, sourceURL string) ([]string, error) {
	base := modelrepo.BaseURL(source, sourceURL)
	if base == "" {
		return nil, fmt.Errorf("source %q has no address to read", source)
	}
	if source == modelrepo.SourceURL {
		return []string{base}, nil
	}
	specs, err := st.PullSourceRules()
	if err != nil {
		return nil, err
	}
	var rules []sourceurl.Rule
	for _, spec := range specs {
		if spec.Kind() == store.PullRuleURL && spec.ControlPlane {
			rules = append(rules, spec.URLRule())
		}
	}
	planned, err := sourceurl.Plan(base, rules)
	if errors.Is(err, sourceurl.ErrNoAllowedSource) {
		return nil, messages.WrapProductError(messages.ErrorModelMetadataForbidden, err.Error(),
			map[string]any{"source": source, "route": "/api/v1/pull/source-rules"}, err)
	}
	if err != nil {
		return nil, err
	}
	return sourceurl.Bases(planned), nil
}

// DraftSiteBases are the addresses a site rule being edited makes a node try
// for its site, in order - what "test connectivity" asks nodes about before
// the rule is saved - and which hub's API answers there: ModelScope for
// modelscope.cn, Hugging Face's for any other site. A draft that is not a
// valid site rule is an error naming the field.
func DraftSiteBases(match string, endpoints []string, allowUpstream bool) ([]string, string, error) {
	if err := sourceurl.ValidateMatch(match); err != nil {
		return nil, "", &store.PullSettingError{Field: "match", Err: err}
	}
	for _, endpoint := range endpoints {
		if err := sourceurl.ValidateEndpoint(endpoint); err != nil {
			return nil, "", &store.PullSettingError{Field: "endpoints", Err: err}
		}
	}
	root, _ := sourceurl.Normalize(match)
	planned, err := sourceurl.Plan(root, []sourceurl.Rule{{Match: root, Endpoints: endpoints, AllowUpstream: allowUpstream}})
	if err != nil {
		return nil, "", &store.PullSettingError{Field: "endpoints", Err: err}
	}
	source := modelrepo.SourceHF
	if sourceurl.Host(root) == sourceurl.Host(modelrepo.BaseURL(modelrepo.SourceMS, "")) {
		source = modelrepo.SourceMS
	}
	return sourceurl.Bases(planned), source, nil
}

// ModelLogins are the stored logins as the management node's model reads
// apply them (modelrepo.AuthorizationFor); none when they cannot be read.
func ModelLogins(st *store.Store) []modelrepo.Login {
	if st == nil {
		return nil
	}
	auths, err := st.SourceAuths()
	if err != nil {
		return nil
	}
	out := make([]modelrepo.Login, 0, len(auths))
	for _, auth := range auths {
		out = append(out, modelrepo.Login{Host: auth.Host, Username: auth.Username, Secret: auth.Secret})
	}
	return out
}

// PlanModelFetch gives a download task its candidates, and its Endpoint (the
// first candidate) for agents that predate them. Tasks that are not
// downloads - a peer copy, a registered path, a storage - are left as they
// are: the rules govern only what a node downloads. The store sends an HTTP
// directory's address as the task's endpoint, since it is the model's own.
func PlanModelFetch(rules []sourceurl.Rule, node *store.Node, task *proto.ModelFileTask, model string) error {
	if task.Acquisition != proto.ModelAcquireDownload || (task.DesiredState != "" && task.DesiredState != "present") {
		return nil
	}
	sourceURL := ""
	if task.Source == modelrepo.SourceURL {
		sourceURL = task.Endpoint
	}
	candidates, err := WeightsFetchCandidatesFor(rules, node, task.Source, sourceURL, model)
	if err != nil {
		return err
	}
	task.Candidates = candidates
	task.Endpoint = candidates[0].Base
	return nil
}

// PlanModelFileTasks plans a whole replay's tasks for node with its rules
// selected once (RulesFor, then URLRules). A download the rules forbid is
// left out of the replay - the agent then neither starts it nor removes
// anything - and its error returned for the caller to log; whether the
// deployment waiting for it is refused is the controller's to record.
func PlanModelFileTasks(rules []sourceurl.Rule, node *store.Node, tasks []proto.ModelFileTask) ([]proto.ModelFileTask, []error) {
	out := tasks[:0:0]
	var refused []error
	for _, task := range tasks {
		if err := PlanModelFetch(rules, node, &task, task.SourceID); err != nil {
			refused = append(refused, fmt.Errorf("model file %s: %w", task.Key, err))
			continue
		}
		out = append(out, task)
	}
	return out, refused
}

// PlanModelFileTask is PlanModelFetch for one task pushed on its own, reading
// the node's rules.
func PlanModelFileTask(st *store.Store, resolve NodeResolver, node *store.Node, task *proto.ModelFileTask, model string) error {
	if task.Acquisition != proto.ModelAcquireDownload || (task.DesiredState != "" && task.DesiredState != "present") {
		return nil
	}
	rules, err := RulesFor(st, resolve, node)
	if err != nil {
		return err
	}
	return PlanModelFetch(URLRules(rules), node, task, model)
}

// WeightsFetchRefusal is the scheduling refusal for a planning error that
// says the pull source rules leave node nowhere to download model from.
func WeightsFetchRefusal(err error, node *store.Node, model string) (scheduler.Reason, bool) {
	if !errors.Is(err, sourceurl.ErrNoAllowedSource) || node == nil {
		return scheduler.Reason{}, false
	}
	return scheduler.WeightsFetchForbidden(node.DisplayName(), model), true
}
