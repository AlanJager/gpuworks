package extapi

import (
	"net/url"
	"strings"
	"testing"
)

// The inventory query takes an opaque cursor and a limit of 1 to 500, each
// at most once, and nothing else; a missing limit asks for a full page.
func TestNodesQueryRules(t *testing.T) {
	ok := []struct {
		raw  string
		want NodesQuery
	}{
		{"", NodesQuery{}},
		{"limit=500", NodesQuery{Limit: 500}},
		{"cursor=7&limit=1", NodesQuery{Cursor: "7", Limit: 1}},
		{"cursor=a_b-C9", NodesQuery{Cursor: "a_b-C9"}},
	}
	for _, c := range ok {
		q, _ := url.ParseQuery(c.raw)
		got, err := ParseNodesQuery(q)
		if err != nil || got != c.want {
			t.Errorf("%q = %+v, %v; want %+v", c.raw, got, err, c.want)
		}
		again, _ := url.ParseQuery(got.Encode())
		if back, err := ParseNodesQuery(again); err != nil || back != got {
			t.Errorf("%q: Encode does not round-trip: %+v, %v", c.raw, back, err)
		}
	}
	bad := []struct{ raw, code, field string }{
		{"limit=0", ErrorWireInvalid, "limit"},
		{"limit=501", ErrorWireInvalid, "limit"},
		{"limit=-1", ErrorWireInvalid, "limit"},
		{"limit=05", ErrorWireInvalid, "limit"},
		{"limit=x", ErrorWireInvalid, "limit"},
		{"limit=1&limit=2", ErrorWireInvalid, "limit"},
		{"cursor=7/8", ErrorWireInvalid, "cursor"},
		{"cursor=" + strings.Repeat("a", 65), ErrorWireInvalid, "cursor"},
		{"after=7", ErrorWireUnknownField, "after"},
	}
	for _, c := range bad {
		q, _ := url.ParseQuery(c.raw)
		_, err := ParseNodesQuery(q)
		e, isErr := err.(*Error)
		if !isErr || e.Code != c.code || e.Params["field"] != c.field {
			t.Errorf("%q = %v; want %s at %s", c.raw, err, c.code, c.field)
		}
	}
}

// An identity request bigger than any valid one is refused before it is
// parsed; the largest valid one fits.
func TestNodeIdentityRequestSizeBound(t *testing.T) {
	ids := make([]uint64, MaxNodeRead)
	for i := range ids {
		ids[i] = maxSafeInteger - uint64(i)
	}
	largest, err := Marshal(NodeIdentityRequest{Salt: strings.Repeat("é", MaxNodeIdentitySalt/2), NodeIDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeNodeIdentityRequest(largest); err != nil {
		t.Fatalf("largest valid request (%d bytes): %v", len(largest), err)
	}
	huge := []byte(`{"node_ids":[1],"salt":"` + strings.Repeat("a", MaxNodeIdentityRequestBytes) + `"}`)
	if _, err := DecodeNodeIdentityRequest(huge); CodeOf(err) != ErrorWireMalformed {
		t.Fatalf("huge request = %v", err)
	}
}

// A salt is counted in bytes, so the server's key never exceeds 256 bytes
// for the longest package id.
func TestNodeIdentitySaltFitsTheServerKey(t *testing.T) {
	longestID := "a" + strings.Repeat("b", 31)
	if !ValidPackageID(longestID) || ValidPackageID(longestID+"c") {
		t.Fatal("the package id grammar changed; recompute MaxNodeIdentitySalt")
	}
	if key := len("pkg\x00" + longestID + "\x00" + strings.Repeat("s", MaxNodeIdentitySalt)); key != 256 {
		t.Fatalf("key = %d bytes", key)
	}
	over := NodeIdentityRequest{Salt: strings.Repeat("é", MaxNodeIdentitySalt/2+1), NodeIDs: []uint64{1}}
	if err := over.Validate(); err == nil || err.(*Error).Params["field"] != "salt" {
		t.Fatalf("over-long multibyte salt = %v", err)
	}
}
