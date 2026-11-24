package enrollment_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

// Members of different releases ask each other for their parts: the path,
// the clock header and the request are pinned literally.
func TestClusterDiagnosticsWire(t *testing.T) {
	if enrollment.ClusterDiagnosticsPath != "/_cluster/diagnostics" || enrollment.ClusterDiagnosticsClockHeader != "X-Gpuworks-Member-Clock" {
		t.Fatal("the diagnostics route's path or clock header changed")
	}
	from := time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC)
	raw, err := json.Marshal(enrollment.ClusterDiagnosticsRequest{From: from, To: from.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"from":"2026-11-02T11:00:00Z","to":"2026-11-02T12:00:00Z"}`
	if string(raw) != want {
		t.Fatalf("request = %s, want %s", raw, want)
	}
	var back enrollment.ClusterDiagnosticsRequest
	if err := json.Unmarshal([]byte(want), &back); err != nil || !back.From.Equal(from) || !back.To.Equal(from.Add(time.Hour)) {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
}

func TestClusterDiagnosticsNeedsAMemberCertificate(t *testing.T) {
	client, err := enrollment.NewClient("https://127.0.0.1:1/api/v1/cluster/enrollment/certificate", strings.Repeat("a", 32), strings.Repeat("b", 64), proto.ClusterProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.OpenClusterDiagnostics(t.Context(), []byte("not a certificate"), []byte("not a key"), time.Now().Add(-time.Hour), time.Now()); err == nil {
		t.Fatal("a caller without a member certificate was let through")
	}
}
