package resourceclaim

import "testing"

func TestSamePCIAddressAcceptsEquivalentDomainWidths(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
	}{
		{name: "four digit and eight digit domain", left: "0000:17:00.0", right: "00000000:17:00.0"},
		{name: "implicit zero domain", left: "17:00.0", right: "0000:17:00.0"},
		{name: "case and whitespace", left: "  0001:AB:1F.7 ", right: "00000001:ab:1f.7"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !samePCIAddress(test.left, test.right) {
				t.Fatalf("samePCIAddress(%q, %q) = false, want true", test.left, test.right)
			}
		})
	}
}

func TestSamePCIAddressRejectsDifferentOrMalformedAddresses(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
	}{
		{name: "different bus", left: "0000:17:00.0", right: "0000:18:00.0"},
		{name: "different domain", left: "0000:17:00.0", right: "0001:17:00.0"},
		{name: "malformed left", left: "00000000:17:00", right: "0000:17:00.0"},
		{name: "malformed right", left: "0000:17:00.0", right: "not-a-pci-address"},
		{name: "empty", left: "", right: "0000:17:00.0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if samePCIAddress(test.left, test.right) {
				t.Fatalf("samePCIAddress(%q, %q) = true, want false", test.left, test.right)
			}
		})
	}
}
