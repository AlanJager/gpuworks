package extapi

import (
	"strings"
	"testing"
)

// A host-identity request bigger than any valid one is refused before it
// is parsed; the longest valid salt, every byte escaped, still fits.
func TestHostIdentityRequestSizeBound(t *testing.T) {
	escaped := `{"salt":"` + strings.Repeat(`\u0001`, MaxNodeIdentitySalt) + `"}`
	if len(escaped) > MaxHostIdentityRequestBytes {
		t.Fatalf("the longest escaped request is %d bytes", len(escaped))
	}
	if _, err := DecodeHostIdentityRequest([]byte(escaped)); err != nil {
		t.Fatalf("longest valid request: %v", err)
	}
	huge := []byte(`{"salt":"` + strings.Repeat("a", MaxHostIdentityRequestBytes) + `"}`)
	if _, err := DecodeHostIdentityRequest(huge); CodeOf(err) != ErrorWireMalformed {
		t.Fatalf("huge request = %v", err)
	}
	over := HostIdentityRequest{Salt: strings.Repeat("é", MaxNodeIdentitySalt/2+1)}
	if err := over.Validate(); err == nil || err.(*Error).Params["field"] != "salt" {
		t.Fatalf("over-long multibyte salt = %v", err)
	}
}
