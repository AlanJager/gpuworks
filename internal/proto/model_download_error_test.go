package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestModelDownloadFailureCodeWireRoundTrip(t *testing.T) {
	const wire = `{"key":"f7","state":"failed","bytes_total":0,"bytes_done":0,"rate_bytes_per_s":0,"transfer_id":"try7","transfer_error_code":"MODEL_DOWNLOAD_SOURCE_NOT_FOUND"}`
	var report ModelFileStatus
	if err := json.Unmarshal([]byte(wire), &report); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(report)
	if err != nil || string(encoded) != wire {
		t.Fatalf("typed outcome lost on wire: %s err=%v", encoded, err)
	}
	var old ModelFileStatus
	if err := json.Unmarshal([]byte(`{"key":"f7","state":"failed"}`), &old); err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(old)
	if strings.Contains(string(encoded), "transfer_error_code") {
		t.Fatalf("old agent synthesized a code: %s", encoded)
	}
}
