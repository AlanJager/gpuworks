package provider

import "testing"

// Every state E-HPC documents has a code, a tone and, unless it runs, a reason
// it cannot be connected; the documentation's own misspelling and unknown
// words are handled.
func TestHPCStateCodesTonesAndReasons(t *testing.T) {
	cases := []struct{ word, code, tone, reason string }{
		{"running", "running", HPCToneSuccess, ""},
		{"Running ", "running", HPCToneSuccess, ""},
		{"uninit", "uninit", HPCToneInfo, HPCNotConnectableStarting},
		{"creating", "creating", HPCToneInfo, HPCNotConnectableStarting},
		{"initing", "initing", HPCToneInfo, HPCNotConnectableStarting},
		{"pending", "pending", HPCToneNeutral, HPCNotConnectablePending},
		{"exception", "exception", HPCToneDanger, HPCNotConnectableException},
		{"stopping", "stopping", HPCToneNeutral, HPCNotConnectableStopped},
		{"stopped", "stopped", HPCToneNeutral, HPCNotConnectableStopped},
		{"releasing", "releasing", HPCToneNeutral, HPCNotConnectableReleasing},
		{"raleasing", "releasing", HPCToneNeutral, HPCNotConnectableReleasing},
		{"", "unknown", HPCToneNeutral, HPCNotConnectableUnknown},
		{"upgrading", "unknown", HPCToneNeutral, HPCNotConnectableUnknown},
	}
	for _, tc := range cases {
		code := HPCStateCode(tc.word)
		if code != tc.code || HPCStateTone(code) != tc.tone || HPCNotConnectableReason(code) != tc.reason {
			t.Errorf("%q = %s/%s/%s, want %s/%s/%s", tc.word, code, HPCStateTone(code), HPCNotConnectableReason(code), tc.code, tc.tone, tc.reason)
		}
	}
}
