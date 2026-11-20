package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

func TestEnrollmentInfoRecordsTheStorageTierOnlyWhenItIsNotStandard(t *testing.T) {
	standard, err := json.Marshal(EnrollmentInfo{ClusterID: "c", CAFingerprint: "f", Protocol: 2, ListenAddress: "10.0.0.1:9443", PublicURL: "https://10.0.0.1:9443", StorageTier: storagetier.Standard.Recorded()})
	if err != nil || string(standard) != `{"cluster_id":"c","ca_fingerprint":"f","protocol":2,"listen_address":"10.0.0.1:9443","public_url":"https://10.0.0.1:9443"}` {
		t.Fatalf("standard = %s (%v), want the bytes of a cluster installed before tiers", standard, err)
	}
	slow, err := json.Marshal(EnrollmentInfo{ClusterID: "c", CAFingerprint: "f", Protocol: 2, ListenAddress: "10.0.0.1:9443", PublicURL: "https://10.0.0.1:9443", StorageTier: storagetier.SlowStorage.Recorded()})
	if err != nil || string(slow) != `{"cluster_id":"c","ca_fingerprint":"f","protocol":2,"listen_address":"10.0.0.1:9443","public_url":"https://10.0.0.1:9443","storage_tier":"slow-storage"}` {
		t.Fatalf("slow-storage = %s (%v)", slow, err)
	}
	var decoded EnrollmentInfo
	if err := json.Unmarshal(standard, &decoded); err != nil || decoded.StorageTier != "" {
		t.Fatalf("absent field decoded as %q (%v), want none", decoded.StorageTier, err)
	}
}

func TestRecordedStorageTier(t *testing.T) {
	write := func(t *testing.T, dir, name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "config.json"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name             string
		enrollment, join string
		want             storagetier.Tier
		refused          string
	}{
		{"no cluster files", "", "", storagetier.Standard, ""},
		{"enrollment without a tier", `{"cluster_id":"c"}`, "", storagetier.Standard, ""},
		{"enrollment slow-storage", `{"cluster_id":"c","storage_tier":"slow-storage"}`, "", storagetier.SlowStorage, ""},
		{"joining member has no enrollment configuration yet", "", `{"cluster_id":"c"}`, storagetier.Standard, ""},
		{"unknown tier", `{"cluster_id":"c","storage_tier":"glacial"}`, "", "", `unknown storage tier "glacial"`},
		{"broken file", `{`, "", "", "invalid cluster configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.enrollment != "" {
				write(t, dir, "cluster-enrollment", tc.enrollment)
			}
			if tc.join != "" {
				write(t, dir, "gpuworks.db.join", tc.join)
			}
			got, err := RecordedStorageTier(dir)
			if tc.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("error %v, want %q", err, tc.refused)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("tier %q err %v, want %q", got, err, tc.want)
			}
		})
	}
}
