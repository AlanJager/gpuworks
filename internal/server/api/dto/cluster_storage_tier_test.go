package dto

import (
	"encoding/json"
	"testing"
)

func TestClusterCertificateResponseCarriesTheStorageTierOnlyWhenItIsNotStandard(t *testing.T) {
	base := ClusterCertificateResponse{ClusterID: "c", Protocol: 1, Address: "10.0.0.2:9001", KeyFingerprint: "f", CertificatePEM: "cert", CACertificatePEM: "ca"}
	const standard = `{"cluster_id":"c","protocol":1,"address":"10.0.0.2:9001","key_fingerprint":"f","certificate_pem":"cert","ca_certificate_pem":"ca"}`
	const slow = `{"cluster_id":"c","protocol":1,"address":"10.0.0.2:9001","key_fingerprint":"f","certificate_pem":"cert","ca_certificate_pem":"ca","storage_tier":"slow-storage"}`

	got, err := json.Marshal(base)
	if err != nil || string(got) != standard {
		t.Fatalf("standard wire = %s, %v; want %s", got, err, standard)
	}
	withTier := base
	withTier.StorageTier = "slow-storage"
	got, err = json.Marshal(withTier)
	if err != nil || string(got) != slow {
		t.Fatalf("slow-storage wire = %s, %v; want %s", got, err, slow)
	}
	for wire, want := range map[string]ClusterCertificateResponse{standard: base, slow: withTier} {
		var back ClusterCertificateResponse
		if err := json.Unmarshal([]byte(wire), &back); err != nil || back != want {
			t.Fatalf("decode %s = %+v, %v; want %+v", wire, back, err, want)
		}
	}
}
