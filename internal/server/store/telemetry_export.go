package store

import (
	"encoding/json"
	"errors"
	"fmt"
)

// TelemetryExport is where the gateway's request traces go (ADR 0046): an
// OTLP/HTTP collector, the headers it needs and the sampling of the traces
// the gateway starts, plus how the console links a trace id to the tracing
// backend. A missing row is export off. Header values are secrets (tokens):
// they are encrypted with the platform secretbox at rest and only ever in
// the clear in this type.
type TelemetryExport struct {
	Enabled     bool
	Endpoint    string
	Headers     []TelemetryHeader
	SampleRatio float64
	// TraceLink is a URL with {trace_id} where the tracing backend shows a
	// trace; empty for none.
	TraceLink string
}

// TelemetryHeader is one header sent with every export.
type TelemetryHeader struct {
	Name, Value string
}

// telemetryExportValue is the setting's JSON (SettingTelemetryExport): the
// same fields, each header value as secretbox ciphertext.
type telemetryExportValue struct {
	Enabled     bool                   `json:"enabled"`
	Endpoint    string                 `json:"endpoint"`
	Headers     []telemetryHeaderValue `json:"headers,omitempty"`
	SampleRatio float64                `json:"sample_ratio"`
	TraceLink   string                 `json:"trace_link,omitempty"`
}

type telemetryHeaderValue struct {
	Name        string `json:"name"`
	ValueCipher string `json:"value_cipher"`
}

// TelemetryExport returns the stored setting with header values decrypted;
// no row is export off. A value that does not parse or decrypt is an error,
// not a silent "off": the operator turned export on and should hear why it
// is not running.
func (s *Store) TelemetryExport() (TelemetryExport, error) {
	raw := s.GetSetting(SettingTelemetryExport)
	if raw == "" {
		return TelemetryExport{}, nil
	}
	var v telemetryExportValue
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return TelemetryExport{}, fmt.Errorf("telemetry export setting: %w", err)
	}
	out := TelemetryExport{Enabled: v.Enabled, Endpoint: v.Endpoint, SampleRatio: v.SampleRatio, TraceLink: v.TraceLink}
	for _, h := range v.Headers {
		if s.secretbox == nil {
			return TelemetryExport{}, errors.New("telemetry export setting: header decryption is not configured")
		}
		plain, err := s.secretbox.Decrypt(h.ValueCipher)
		if err != nil {
			return TelemetryExport{}, fmt.Errorf("telemetry export header %q: %w", h.Name, err)
		}
		out.Headers = append(out.Headers, TelemetryHeader{Name: h.Name, Value: string(plain)})
	}
	return out, nil
}

// SetTelemetryExport replaces the setting, encrypting the header values.
// Validation is the caller's (the exporter knows what a valid endpoint is).
func (s *Store) SetTelemetryExport(t TelemetryExport) error {
	v := telemetryExportValue{Enabled: t.Enabled, Endpoint: t.Endpoint, SampleRatio: t.SampleRatio, TraceLink: t.TraceLink}
	for _, h := range t.Headers {
		if s.secretbox == nil {
			return errors.New("telemetry export header encryption is not configured")
		}
		cipher, err := s.secretbox.Encrypt([]byte(h.Value))
		if err != nil {
			return err
		}
		v.Headers = append(v.Headers, telemetryHeaderValue{Name: h.Name, ValueCipher: cipher})
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.SetSetting(SettingTelemetryExport, string(b))
}
