package store

import (
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

func telemetryStore(t *testing.T) *Store {
	t.Helper()
	st := newTestStore(t)
	box, err := auth.NewSecretbox("telemetry-test")
	if err != nil {
		t.Fatal(err)
	}
	st.SetSecretbox(box)
	return st
}

// The setting round-trips, header values never reach the row in the clear,
// and no row is export off.
func TestTelemetryExportRoundTripsWithEncryptedHeaders(t *testing.T) {
	st := telemetryStore(t)
	if got, err := st.TelemetryExport(); err != nil || !reflect.DeepEqual(got, TelemetryExport{}) {
		t.Fatalf("no row = %+v, %v", got, err)
	}
	want := TelemetryExport{
		Enabled: true, Endpoint: "https://otel.example.com", SampleRatio: 0.25, TraceLink: "https://grafana/trace/{trace_id}",
		Headers: []TelemetryHeader{{Name: "Authorization", Value: "Bearer s3cret-token"}, {Name: "X-Scope-OrgID", Value: "lab"}},
	}
	if err := st.SetTelemetryExport(want); err != nil {
		t.Fatal(err)
	}
	raw := st.GetSetting(SettingTelemetryExport)
	if strings.Contains(raw, "s3cret-token") || strings.Contains(raw, `"lab"`) || !strings.Contains(raw, `"name":"Authorization"`) || !strings.Contains(raw, `"value_cipher":"`) {
		t.Fatalf("row = %s", raw)
	}
	if !strings.HasPrefix(raw, `{"enabled":true,"endpoint":"https://otel.example.com","headers":[`) || !strings.HasSuffix(raw, `"sample_ratio":0.25,"trace_link":"https://grafana/trace/{trace_id}"}`) {
		t.Fatalf("row shape = %s", raw)
	}
	got, err := st.TelemetryExport()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("read back %+v, %v", got, err)
	}
	// Turning it off keeps no headers when none are given.
	if err := st.SetTelemetryExport(TelemetryExport{Endpoint: "x"}); err != nil {
		t.Fatal(err)
	}
	if raw := st.GetSetting(SettingTelemetryExport); raw != `{"enabled":false,"endpoint":"x","sample_ratio":0}` {
		t.Fatalf("row = %s", raw)
	}
}

// A row that does not parse or decrypt is an error, not a silent "off".
func TestTelemetryExportReportsAnUnreadableRow(t *testing.T) {
	st := telemetryStore(t)
	if err := st.SetSetting(SettingTelemetryExport, `{"enabled":true,`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TelemetryExport(); err == nil {
		t.Fatal("corrupt JSON read as a setting")
	}
	if err := st.SetSetting(SettingTelemetryExport, `{"enabled":true,"endpoint":"http://c","headers":[{"name":"A","value_cipher":"garbage"}],"sample_ratio":1}`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TelemetryExport(); err == nil || !strings.Contains(err.Error(), `"A"`) {
		t.Fatalf("undecryptable header: %v", err)
	}
	// Without a secretbox, headers can be neither written nor read.
	bare := newTestStore(t)
	if err := bare.SetTelemetryExport(TelemetryExport{Headers: []TelemetryHeader{{Name: "A", Value: "b"}}}); err == nil {
		t.Fatal("wrote a header without encryption")
	}
	if err := bare.SetSetting(SettingTelemetryExport, `{"enabled":true,"headers":[{"name":"A","value_cipher":"x"}]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := bare.TelemetryExport(); err == nil {
		t.Fatal("read a header without decryption")
	}
}
