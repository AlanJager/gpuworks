package app

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/sourceurl"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// ensureModelEndpointRules sends every model download and metadata request
// for Hugging Face and ModelScope to endpoint, as the server's
// --model-endpoint flag asks: one site rule per hub, for every node, that
// never reaches the hub itself, read by the control plane as well. It exists
// for in-process harnesses whose fake hub serves both. A hub that already has a site rule is left as the operator
// configured it, and running it again adds nothing.
func ensureModelEndpointRules(st *store.Store, endpoint string) error {
	normalized, err := sourceurl.Normalize(endpoint)
	if err != nil {
		return fmt.Errorf("model endpoint: %w", err)
	}
	rules, err := st.PullSourceRules()
	if err != nil {
		return err
	}
	for _, site := range []string{modelrepo.BaseURL(modelrepo.SourceHF, ""), modelrepo.BaseURL(modelrepo.SourceMS, "")} {
		configured := false
		for _, rule := range rules {
			if rule.Kind() == store.PullRuleURL && rule.Match == site {
				configured = true
				break
			}
		}
		if configured {
			continue
		}
		if _, err := st.CreatePullSourceRule(store.PullSourceRuleSpec{
			Match: site, Endpoints: []string{normalized}, AllowUpstream: false, ControlPlane: true, Note: "--model-endpoint",
		}); err != nil {
			return err
		}
	}
	return nil
}
