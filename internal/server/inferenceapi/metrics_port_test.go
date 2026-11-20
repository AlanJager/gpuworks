package inferenceapi

import (
	"encoding/json"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestMetricsPortNormalizesAndReportsASeparatePort(t *testing.T) {
	for _, tc := range []struct{ written, stored string }{
		{"", ""}, {"api", ""}, {" API ", ""}, {"separate", "separate"}, {" Separate", "separate"},
	} {
		normalized, err := NormalizeContract(Contract{Operations: chatOperation, Metrics: &Metrics{Port: tc.written}})
		if err != nil {
			t.Fatalf("port %q: %v", tc.written, err)
		}
		if normalized.Metrics.Port != tc.stored {
			t.Fatalf("port %q stored as %q, want %q", tc.written, normalized.Metrics.Port, tc.stored)
		}
		encoded, _ := json.Marshal(normalized)
		if got := SeparateMetricsPort(encoded); got != (tc.stored == "separate") {
			t.Fatalf("SeparateMetricsPort(%s) = %v", encoded, got)
		}
	}
	// The default is stored as no key, so a contract that never declared a
	// port keeps its stored JSON.
	plain, err := NormalizeContract(Contract{Operations: chatOperation, Metrics: &Metrics{Port: "api", Endpoint: "/m"}})
	if err != nil {
		t.Fatal(err)
	}
	if encoded, _ := json.Marshal(plain.Metrics); string(encoded) != `{"endpoint":"/m"}` {
		t.Fatalf("api port stored as %s", encoded)
	}
}

func TestMetricsPortRefusesAFreeFormPort(t *testing.T) {
	for _, port := range []string{"9100", "same", "127.0.0.1:9100"} {
		_, err := NormalizeContract(Contract{Operations: chatOperation, Metrics: &Metrics{Port: port}})
		var product *messages.ProductError
		if !errors.As(err, &product) || product.Code() != messages.ErrorCatalogMetricsPortInvalid || product.Params()["port"] != port {
			t.Fatalf("port %q: err = %v", port, err)
		}
	}
}

func TestSeparateMetricsPortReadsOnlyTheDeclaredValue(t *testing.T) {
	for raw, want := range map[string]bool{
		``: false, `null`: false, `{}`: false, `{"metrics":null}`: false, `not json`: false,
		`{"metrics":{"port":"api"}}`:                                      false,
		`{"metrics":{"endpoint":"/m"}}`:                                   false,
		`{"metrics":{"port":"separate"}}`:                                 true,
		`{"operations":[],"metrics":{"port":"separate","endpoint":"/m"}}`: true,
	} {
		if got := SeparateMetricsPort(json.RawMessage(raw)); got != want {
			t.Errorf("SeparateMetricsPort(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestMetricsPortYAMLKey(t *testing.T) {
	contract := decodeContractYAML(t, "operations:\n- type: openai.chat\n  endpoint: /v1/chat/completions\nmetrics:\n  endpoint: /m\n  port: separate\n")
	if contract.Metrics == nil || contract.Metrics.Port != "separate" || contract.Metrics.Endpoint != "/m" {
		t.Fatalf("metrics = %+v", contract.Metrics)
	}
}
