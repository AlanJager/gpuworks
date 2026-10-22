package templateprobe

import "testing"

func TestCompareOutcomes(t *testing.T) {
	tests := []struct {
		name      string
		full      ProbeOutcome
		truncated ProbeOutcome
		aligned   bool
		falseNeg  bool
		falsePos  bool
	}{
		{"pass aligns", ProbeOutcome{Status: "pass"}, ProbeOutcome{Status: "pass"}, true, false, false},
		{"same failure aligns", ProbeOutcome{Status: "fail", SignatureID: "x"}, ProbeOutcome{Status: "fail", SignatureID: "x"}, true, false, false},
		{"different failure refuses", ProbeOutcome{Status: "fail", SignatureID: "x"}, ProbeOutcome{Status: "fail", SignatureID: "y"}, false, false, false},
		{"false negative invalidates", ProbeOutcome{Status: "fail", SignatureID: "x"}, ProbeOutcome{Status: "pass"}, false, true, false},
		{"false positive tunes prefix", ProbeOutcome{Status: "pass"}, ProbeOutcome{Status: "fail", SignatureID: "x"}, false, false, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := CompareOutcomes(test.full, test.truncated)
			if got.Aligned != test.aligned || got.FalseNegative != test.falseNeg || got.FalsePositive != test.falsePos {
				t.Fatalf("CompareOutcomes() = %+v", got)
			}
		})
	}
}
