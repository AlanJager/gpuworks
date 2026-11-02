package seed

import (
	"strings"
	"testing"
)

// vLLM (0.30 and 0.31, the runner images' versions) refuses
// indexer_kv_dtype=mxfp4 unless the GPU is datacenter Blackwell (sm_10x:
// B200, B300, GB200, GB300); consumer Blackwell (sm_120) and GB10 (sm_121)
// fail at start. It also rejects attention options it does not know, and
// use_fp4_indexer_cache was replaced by indexer_kv_dtype.
func TestFP4IndexerCacheSpecsMatchOnlyDatacenterBlackwell(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			joined := strings.Join(spec.Args, " ")
			if strings.Contains(joined, "use_fp4_indexer_cache") {
				t.Errorf("%s/%s passes the retired use_fp4_indexer_cache option", model.ModelID, spec.Name)
			}
			if !strings.Contains(joined, "indexer_kv_dtype=mxfp4") && !strings.Contains(joined, `"indexer_kv_dtype":"mxfp4"`) {
				continue
			}
			if spec.GPUs.Name == "" && spec.GPUs.CC != "10.0..10.9" {
				t.Errorf("%s/%s uses the MXFP4 indexer cache but matches cc %q, not only sm_10x", model.ModelID, spec.Name, spec.GPUs.CC)
			}
			if name := spec.GPUs.Name; name != "" && (strings.Contains(name, "RTX") || strings.Contains(name, "GB10") || strings.Contains(name, "Spark")) {
				t.Errorf("%s/%s uses the MXFP4 indexer cache on cards it does not support: %s", model.ModelID, spec.Name, name)
			}
		}
	}
}
