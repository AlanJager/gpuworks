package uxrecord

import "testing"

// Every request has exactly one result: an error status first, then how the
// copy ended, then whether the engine stopped at the length limit.
func TestResultOf(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Request
		want Result
	}{
		{"finished", Request{Status: 200, FinishReason: "stop"}, ResultCompleted},
		{"no finish reason", Request{Status: 200}, ResultCompleted},
		{"at the output limit", Request{Status: 200, FinishReason: "length"}, ResultLength},
		{"client left", Request{Status: 200, Outcome: OutcomeClientClosed, FinishReason: "length"}, ResultClientClosed},
		{"connection cut", Request{Status: 200, Outcome: OutcomeTruncated}, ResultInterrupted},
		{"never answered", Request{Status: 502, Outcome: OutcomeUpstreamError}, ResultError},
		{"error status", Request{Status: 400}, ResultError},
		{"error status, then the client left", Request{Status: 500, Outcome: OutcomeClientClosed}, ResultError},
	} {
		if got := ResultOf(tc.r); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	for _, r := range Results() {
		if back, ok := ParseResult(string(r)); !ok || back != r {
			t.Errorf("ParseResult(%q) = %q %v", r, back, ok)
		}
	}
	if _, ok := ParseResult("slow"); ok {
		t.Error("an unknown result parsed")
	}
}
