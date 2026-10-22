package templateprobe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestK100CalibrationFixtureIsRealHardwareEvidence(t *testing.T) {
	fixtures := []struct {
		name   string
		digest string
	}{
		{"k100-qwen3-4b-dense-calibration.json", "sha256:164afad43171dd431f0344d9808e42777ccfd1af2264e2409a8326ca4da509f4"},
		{"k100-qwen3-4b-dense-vllm021-calibration.json", "sha256:b889671afa7624f259069d037809825de6debf6480f11cc06d37328f3923e94d"},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("testdata", fixture.name))
			if err != nil {
				t.Fatal(err)
			}
			var input CalibrationRecord
			if err := json.Unmarshal(body, &input); err != nil {
				t.Fatal(err)
			}
			record, err := NewCalibrationRecord(input)
			if err != nil {
				t.Fatal(err)
			}
			if record.ModelID != "Qwen/Qwen3-4B" || record.ArchFamily != "dense" || record.Chip != "K100_AI" {
				t.Fatalf("identity = %+v", record)
			}
			if record.ImageDigest != fixture.digest {
				t.Fatalf("image digest = %q", record.ImageDigest)
			}
			if !record.Alignment.Aligned || record.DisableTruncation {
				t.Fatalf("alignment = %+v", record.Alignment)
			}
		})
	}
}
