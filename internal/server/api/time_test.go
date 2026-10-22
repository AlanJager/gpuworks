package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAPITimeHelpersAlwaysMarshalUTC(t *testing.T) {
	local := time.Date(2026, 8, 25, 14, 30, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	payload := struct {
		Value   time.Time  `json:"value"`
		Pointer *time.Time `json:"pointer"`
	}{Value: utcTime(local), Pointer: utcTimePtr(&local)}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(encoded); strings.Contains(got, "+08:00") || strings.Count(got, "Z") != 2 {
		t.Fatalf("API timestamps = %s, want two Z-suffixed UTC values", got)
	}
}
