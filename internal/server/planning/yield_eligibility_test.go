package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/sleep"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Who may hold a yield policy other than never: a deployment that can sleep,
// single-node and not PD. A deployment that cannot sleep says why first,
// whatever its shape, as the API's refusal does.
func TestYieldPolicyRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		dep  store.Deployment
		want string
	}{
		{"sleepable single-node", store.Deployment{Engine: "vllm", Sleepable: true}, ""},
		{"sleepable, nnodes 1", store.Deployment{Engine: "vllm", Sleepable: true, NNodes: 1}, ""},
		{"not sleepable", store.Deployment{Engine: "vllm"}, sleep.CodeNotEnabled},
		{"engine cannot sleep", store.Deployment{Engine: "no-such-engine", Sleepable: true}, sleep.CodeEngineUnsupported},
		{"not sleepable and multi-node", store.Deployment{Engine: "vllm", NNodes: 2}, sleep.CodeNotEnabled},
		{"multi-node", store.Deployment{Engine: "vllm", Sleepable: true, NNodes: 2}, messages.ErrorYieldPolicyShape},
		{"PD", store.Deployment{Engine: "vllm", Sleepable: true, Placement: schedulePDPlacement}, messages.ErrorYieldPolicyShape},
	} {
		if got := YieldPolicyRefusal(&tc.dep); got != tc.want {
			t.Errorf("%s: refusal %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := DraftYieldPolicyRefusal(true, "vllm", 1, true); got != messages.ErrorYieldPolicyShape {
		t.Errorf("a PD draft: refusal %q, want the shape refusal", got)
	}
	if got := DraftYieldPolicyRefusal(true, "vllm", 0, false); got != "" {
		t.Errorf("a draft that states no node count: refusal %q, want none", got)
	}
}
