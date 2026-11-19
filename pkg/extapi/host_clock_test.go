package extapi

import (
	"strings"
	"testing"
	"time"
)

// A held table built the way the server builds it (the canonical body and
// the push's digest) decodes back to the table.
func TestPushedStateCarriesTheCanonicalTableAndItsDigest(t *testing.T) {
	table := &NodeHoldsV1{Nodes: map[string]Notice{"9": {Code: "ACME_QUARANTINE"}}}
	body, err := Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := DigestValue(table)
	if err != nil {
		t.Fatal(err)
	}
	at := NewTime(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	held := PushedState{Kind: KindNodeHolds, Schema: 1, Generation: 4, Digest: digest, PushedAt: &at, Body: body}
	raw, err := Marshal(held)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodePushedState(raw)
	if err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if !got.Held() {
		t.Fatal("a held table reads as not held")
	}
	back, err := got.Table()
	if err != nil {
		t.Fatal(err)
	}
	if h := back.(*NodeHoldsV1); h.Nodes["9"].Code != "ACME_QUARANTINE" {
		t.Fatalf("table = %+v", h)
	}
	if empty, err := DecodePushedState([]byte(`{"kind":"banners"}`)); err != nil || empty.Held() {
		t.Fatalf("not held = %+v, %v", empty, err)
	}
}

// Requests larger than a host call reads are refused before decoding.
func TestHostCallBodiesAreBounded(t *testing.T) {
	big := `{"label":"usb.challenge","length":32,"x":"` + strings.Repeat("a", MaxHostCallBytes) + `"}`
	if _, err := DecodeKeyRequest([]byte(big)); CodeOf(err) != ErrorWireMalformed {
		t.Fatalf("oversized key request = %v", err)
	}
	if _, err := DecodeClockCorrection([]byte(big)); CodeOf(err) != ErrorWireMalformed {
		t.Fatalf("oversized correction = %v", err)
	}
	if _, err := DecodeClockCorrection([]byte(`{"reason":"\xff","to":"2027-01-15T00:00:00Z"}`)); err == nil {
		t.Fatal("a reason that is not UTF-8 decoded")
	}
	for label, ok := range map[string]bool{"usb.challenge": true, "a": true, strings.Repeat("a", 64): true,
		"": false, strings.Repeat("a", 65): false, "Usb": false, "usb/key": false} {
		if ValidKeyLabel(label) != ok {
			t.Errorf("ValidKeyLabel(%q) = %v", label, !ok)
		}
	}
}
