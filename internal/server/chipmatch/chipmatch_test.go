package chipmatch

import (
	"strings"
	"testing"
)

// These are exact detector values, including variations in vendor prefixes and
// product suffixes. They are deliberately not catalog aliases.
func TestEvaluateRealDetectorNames(t *testing.T) {
	tests := []struct {
		name string
		gpu  GPU
		gpus Predicate
		want bool
	}{
		{"2080 Ti matches >=7.5", GPU{Name: "GeForce RTX 2080 Ti", Vendor: "NVIDIA", CC: 7.5}, Predicate{Vendor: "nvidia", CC: ">=7.5"}, true},
		{"V100 matches exact 7", GPU{Name: "Tesla V100-SXM2-32GB", Vendor: "nvidia", CC: 7}, Predicate{Vendor: "nvidia", CC: "7"}, true},
		{"T4 matches closed range", GPU{Name: "Tesla T4", Vendor: "nvidia", CC: 7.5}, Predicate{Vendor: "nvidia", CC: "7.0..7.5"}, true},
		{"4090 fails Blackwell", GPU{Name: "NVIDIA GeForce RTX 4090", Vendor: "nvidia", CC: 8.9}, Predicate{Vendor: "nvidia", CC: ">=10.0"}, false},
		{"H100 matches lower bound", GPU{Name: "NVIDIA H100 80GB HBM3", Vendor: "nvidia", CC: 9}, Predicate{Vendor: "nvidia", CC: ">=8.9"}, true},
		{"RTX PRO 6000 name ignores case", GPU{Name: "NVIDIA RTX PRO 6000 Blackwell Server Edition", Vendor: "nvidia", CC: 12}, Predicate{Vendor: "nvidia", Name: `rtx pro\s+6000`}, true},
		{"Ascend 910B4 name regex", GPU{Name: "Ascend 910B4", Vendor: "Ascend"}, Predicate{Vendor: "ascend", Name: `910[bc]`}, true},
		{"Ascend name regex is case insensitive", GPU{Name: "ascend 910c", Vendor: "ascend"}, Predicate{Vendor: "ascend", Name: `910B|910C`}, true},
		{"CC ignored outside NVIDIA", GPU{Name: "Ascend 910B4", Vendor: "ascend", CC: 0}, Predicate{Vendor: "ascend", CC: ">=99", Name: `910b`}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(tt.gpu, tt.gpus)
			if got.Matched != tt.want {
				t.Fatalf("Evaluate(%+v, %+v) = %+v, want matched=%v", tt.gpu, tt.gpus, got, tt.want)
			}
		})
	}
}

func TestEvaluateCCFormsAndInvalidPredicate(t *testing.T) {
	gpu := GPU{Name: "NVIDIA H100", Vendor: "nvidia", CC: 9}
	for _, expression := range []string{">=9", "<10", "9", "8.9..9.0"} {
		t.Run(expression, func(t *testing.T) {
			if got := Evaluate(gpu, Predicate{Vendor: "nvidia", CC: expression}); !got.Matched {
				t.Fatalf("CC %q did not match: %+v", expression, got)
			}
		})
	}
	for _, predicate := range []Predicate{{Vendor: "nvidia", CC: ">9"}, {Vendor: "nvidia", CC: ">=8e0"}, {Vendor: "nvidia", CC: "10..8"}, {Vendor: "nvidia", Name: "["}} {
		if got := Evaluate(gpu, predicate); got.Matched || strings.TrimSpace(got.Reason) == "" {
			t.Fatalf("invalid predicate %+v = %+v", predicate, got)
		}
	}
}

func TestSelectUnsupportedVetoAndQuantizationAlternative(t *testing.T) {
	v100 := GPU{Name: "Tesla V100-SXM2-32GB", Vendor: "nvidia", CC: 7}
	unsupported := []Unsupported{{GPUs: Predicate{Vendor: "nvidia", CC: "<8.9"}, Quantization: "fp8", Reason: "FP8 weights require Ada or newer"}}

	denied := Select(v100, []Spec{{Name: "fp8-balanced", Quantization: "fp8", GPUs: Predicate{Vendor: "nvidia", CC: ">=7.0"}}}, unsupported)
	if denied.Matched || denied.UnsupportedIndex != 0 || denied.Reason != unsupported[0].Reason || !strings.Contains(denied.MatchedBy, "unsupported") {
		t.Fatalf("unsupported must veto matching spec: %+v", denied)
	}

	allowed := Select(v100, []Spec{
		{Name: "fp8-balanced", Quantization: "fp8", GPUs: Predicate{Vendor: "nvidia", CC: ">=7.0"}},
		{Name: "fp16-balanced", Quantization: "fp16", GPUs: Predicate{Vendor: "nvidia", CC: ">=7.0"}},
	}, unsupported)
	if !allowed.Matched || allowed.SpecIndex != 1 {
		t.Fatalf("quantization-scoped unsupported should leave fp16 selectable: %+v", allowed)
	}
}

func TestSelectSpecificityAndAmbiguity(t *testing.T) {
	gpu := GPU{Name: "NVIDIA RTX PRO 6000 Blackwell Server Edition", Vendor: "nvidia", CC: 12}
	narrowerRange := Select(gpu, []Spec{
		{Name: "wide", GPUs: Predicate{Vendor: "nvidia", CC: "10..12"}},
		{Name: "narrow", GPUs: Predicate{Vendor: "nvidia", CC: "11..12"}},
	}, nil)
	if !narrowerRange.Matched || narrowerRange.SpecIndex != 1 {
		t.Fatalf("narrow CC range should outrank wider range: %+v", narrowerRange)
	}

	specific := Select(gpu, []Spec{
		{Name: "open", GPUs: Predicate{Vendor: "nvidia", CC: ">=10"}},
		{Name: "range", GPUs: Predicate{Vendor: "nvidia", CC: "10..12"}},
		{Name: "named", GPUs: Predicate{Vendor: "nvidia", Name: `rtx pro\s+6000`}},
	}, nil)
	if !specific.Matched || specific.SpecIndex != 2 || !strings.Contains(specific.MatchedBy, "spec:named") {
		t.Fatalf("name predicate should outrank range and open predicate: %+v", specific)
	}

	ambiguous := Select(gpu, []Spec{
		{Name: "first", GPUs: Predicate{Vendor: "nvidia", CC: ">=10"}},
		{Name: "second", GPUs: Predicate{Vendor: "nvidia", CC: ">=10"}},
	}, nil)
	if !ambiguous.Matched || ambiguous.SpecIndex != 0 || !ambiguous.Ambiguous || !strings.Contains(ambiguous.MatchedBy, "ambiguous") {
		t.Fatalf("equal predicates must select catalog order and mark ambiguity: %+v", ambiguous)
	}
}
