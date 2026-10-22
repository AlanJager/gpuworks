package templateprobe

import "fmt"

type ProbeOutcome struct {
	Status      string `json:"status"`
	SignatureID string `json:"signature_id,omitempty"`
}

type AlignmentResult struct {
	Aligned       bool   `json:"aligned"`
	FalseNegative bool   `json:"false_negative"`
	FalsePositive bool   `json:"false_positive"`
	Reason        string `json:"reason"`
}

// CompareOutcomes enforces the calibration rule: pass/pass aligns, and
// fail/fail aligns only when both failures have the same deterministic
// signature. A full failure hidden by truncation is the invalidating case.
func CompareOutcomes(full, truncated ProbeOutcome) AlignmentResult {
	if full.Status == "pass" && truncated.Status == "pass" {
		return AlignmentResult{Aligned: true, Reason: "full and truncated probes passed"}
	}
	if full.Status == "fail" && truncated.Status == "fail" {
		if full.SignatureID != "" && full.SignatureID == truncated.SignatureID {
			return AlignmentResult{Aligned: true, Reason: "full and truncated probes have the same deterministic failure signature"}
		}
		return AlignmentResult{Reason: "failure signatures differ or are not deterministic"}
	}
	if full.Status == "fail" && truncated.Status == "pass" {
		return AlignmentResult{FalseNegative: true, Reason: "truncation hid a full-model failure; disable truncation for this architecture"}
	}
	if full.Status == "pass" && truncated.Status == "fail" {
		return AlignmentResult{FalsePositive: true, Reason: "truncation introduced a failure; increase the minimum prefix"}
	}
	return AlignmentResult{Reason: fmt.Sprintf("unsupported outcome pair %q/%q", full.Status, truncated.Status)}
}
