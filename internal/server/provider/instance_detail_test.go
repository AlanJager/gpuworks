package provider

import (
	"errors"
	"testing"
)

// The detail read reaches the provider's detail endpoint, which carries the
// direct SSH endpoint a listing may leave out (RunPod's pod list has none).
func TestInstanceDetailReadsWhatTheListingLeavesOut(t *testing.T) {
	d := &flakyDetailDriver{keyTestDriver: keyTestDriver{name: "key-test-instance-detail", host: "203.0.113.31", port: 19815}, sshFromCall: 1}
	svc, _, providerID, _ := newKeyService(t, &d.keyTestDriver, true)
	Register(d)

	listed, err := d.ListInstances(t.Context(), Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SSHInstallCandidates(listed[0], "auto"); err == nil {
		t.Fatal("the listing fixture offers a direct SSH endpoint; it should only list the proxy")
	}

	detail, err := svc.InstanceDetail(t.Context(), providerID, "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := SSHInstallCandidates(detail, "auto")
	if err != nil || len(candidates) != 1 || candidates[0].Host != "203.0.113.31" || candidates[0].Port != 19815 {
		t.Fatalf("detail SSH candidates = %+v, %v", candidates, err)
	}
	if d.gets != 1 {
		t.Fatalf("detail endpoint read %d times", d.gets)
	}
}

// A provider without a detail endpoint is not asked: its listing is complete.
func TestInstanceDetailIsUnsupportedWithoutADetailEndpoint(t *testing.T) {
	d := &keyTestDriver{name: "key-test-instance-detail-none", host: "203.0.113.32", port: 22}
	svc, _, providerID, _ := newKeyService(t, d, true)
	if _, err := svc.InstanceDetail(t.Context(), providerID, "inst-1"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if _, err := svc.InstanceDetail(t.Context(), providerID+99, "inst-1"); err == nil {
		t.Fatal("an unknown provider was described")
	}
}
