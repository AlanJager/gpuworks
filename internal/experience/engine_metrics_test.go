package experience

import (
	"bufio"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/inferencemetrics"
)

// TestSimulatedEngineSpeaksThePlatformsMetrics keeps the simulated engine's
// exposition in step with what the platform reads from vLLM and SGLang
// (inferencemetrics.EngineRules, their only definition): a family no rule
// reads any more is a rename the simulation missed, and the demo's charts
// would go blank. The simulation need not emit every mapped family, so a
// new mapping costs the platform nothing here.
func TestSimulatedEngineSpeaksThePlatformsMetrics(t *testing.T) {
	engine := claimedEngine(t, "Qwen/Qwen3-8B")
	resp, err := http.Get(engine + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	emitted := map[string]bool{}
	typeLine := regexp.MustCompile(`^# TYPE (\S+) `)
	for sc := bufio.NewScanner(resp.Body); sc.Scan(); {
		if m := typeLine.FindStringSubmatch(sc.Text()); m != nil {
			emitted[m[1]] = true
		}
	}
	mapped := map[string]bool{}
	for _, engine := range []string{engines.VLLM, engines.SGLang} {
		for _, rule := range inferencemetrics.EngineRules(engine) {
			family, _, _ := strings.Cut(rule.Source, "{")
			mapped[family] = true
		}
	}
	for family := range emitted {
		if !mapped[family] {
			t.Errorf("the simulated engine emits %s, which no engine rule reads", family)
		}
	}
	if len(emitted) == 0 {
		t.Fatal("the simulated engine emits no metrics")
	}
}
