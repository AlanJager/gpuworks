package inferencecatalog

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// The metrics port is declared by api.metrics.port: separate and placed by
// ${GPUWORKS_METRICS_PORT} (or its bare alias). Either without the other is a
// spec nothing could serve metrics from, so import refuses it with a stable
// code instead of letting the agent scrape a port nobody listens on.
func TestImportChecksTheMetricsPortAgainstItsVariable(t *testing.T) {
	serve := []string{"--port", "${GPUWORKS_API_PORT}"}
	chat := []APIOperation{{Type: "openai.chat", Endpoint: "/v1/chat/completions", Mode: "stream"}}
	for _, test := range []struct {
		name string
		edit func(*InferenceSpec)
		code string
	}{
		{name: "declared and placed in args", edit: func(spec *InferenceSpec) {
			spec.Engine, spec.Args = "callable", append(append([]string(nil), serve...), "--metrics-port", "${GPUWORKS_METRICS_PORT}")
			spec.API = &APIContract{Operations: chat, Metrics: &inferenceapi.Metrics{Port: "separate"}}
		}},
		{name: "the bare alias places it too", edit: func(spec *InferenceSpec) {
			spec.Engine, spec.Args = "callable", append(append([]string(nil), serve...), "--metrics-port", "${METRICS_PORT}")
			spec.API = &APIContract{Operations: chat, Metrics: &inferenceapi.Metrics{Port: "Separate"}}
		}},
		{name: "declared and placed in env", edit: func(spec *InferenceSpec) {
			spec.Engine, spec.Args = "callable", append([]string(nil), serve...)
			spec.Env = map[string]string{"METRICS_LISTEN": "127.0.0.1:${GPUWORKS_METRICS_PORT}"}
			spec.API = &APIContract{Operations: chat, Metrics: &inferenceapi.Metrics{Port: "separate"}}
		}},
		{name: "declared and placed by the preparation script", edit: func(spec *InferenceSpec) {
			spec.Engine, spec.Args = "callable", append([]string(nil), serve...)
			spec.PreStartArgv, spec.PreStartScript = nil, "echo ${GPUWORKS_METRICS_PORT} > /tmp/metrics-port"
			spec.API = &APIContract{Operations: chat, Metrics: &inferenceapi.Metrics{Port: "separate"}}
		}},
		{name: "api port needs no variable", edit: func(spec *InferenceSpec) {
			spec.Engine, spec.Args = "callable", append([]string(nil), serve...)
			spec.API = &APIContract{Operations: chat, Metrics: &inferenceapi.Metrics{Port: "api", Endpoint: "/m"}}
		}},
		{name: "referenced without a declaration", edit: func(spec *InferenceSpec) {
			spec.Engine, spec.Args = "callable", append(append([]string(nil), serve...), "--metrics-port", "${GPUWORKS_METRICS_PORT}")
		}, code: messages.ErrorCatalogMetricsPortUndeclared},
		{name: "referenced with the api port declared", edit: func(spec *InferenceSpec) {
			spec.Engine, spec.Args = "callable", append(append([]string(nil), serve...), "--metrics-port", "${METRICS_PORT}")
			spec.API = &APIContract{Operations: chat, Metrics: &inferenceapi.Metrics{Port: "api"}}
		}, code: messages.ErrorCatalogMetricsPortUndeclared},
		{name: "a resolver cannot supply it", edit: func(spec *InferenceSpec) {
			spec.Engine, spec.Args = "callable", append(append([]string(nil), serve...), "--metrics-port", "${GPUWORKS_METRICS_PORT}")
			spec.Placement = &Placement{Language: "starlark", Resolver: "def resolve(gpu):\n    return {\"METRICS_PORT\": 9100}\n"}
		}, code: messages.ErrorCatalogMetricsPortUndeclared},
		{name: "declared and never placed", edit: func(spec *InferenceSpec) {
			spec.Engine, spec.Args = "callable", append([]string(nil), serve...)
			spec.API = &APIContract{Operations: chat, Metrics: &inferenceapi.Metrics{Port: "separate"}}
		}, code: messages.ErrorCatalogMetricsPortUnreferenced},
		{name: "a free-form port", edit: func(spec *InferenceSpec) {
			spec.Engine, spec.Args = "callable", append([]string(nil), serve...)
			spec.API = &APIContract{Operations: chat, Metrics: &inferenceapi.Metrics{Port: "9100"}}
		}, code: messages.ErrorCatalogMetricsPortInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := flatFixture()
			test.edit(&catalog.Models[0].Specs[0])
			err := ValidateFlatCatalog(&catalog)
			if test.code == "" {
				if err != nil {
					t.Fatalf("unexpected error=%v", err)
				}
				return
			}
			var product *messages.ProductError
			if !errors.As(err, &product) || product.Code() != test.code {
				t.Fatalf("error=%v, want %s", err, test.code)
			}
			if test.code != messages.ErrorCatalogMetricsPortInvalid && product.Params()["spec"] != catalog.Models[0].Specs[0].Name {
				t.Fatalf("params=%v", product.Params())
			}
		})
	}
}

func TestSchemaDescribesTheMetricsPort(t *testing.T) {
	port, ok := apiMetricsSchema()["properties"].(map[string]any)["port"].(map[string]any)
	if !ok {
		t.Fatal("api.metrics schema has no port")
	}
	if values := port["enum"]; len(values.([]any)) != 2 || values.([]any)[1] != "separate" {
		t.Fatalf("port enum = %v", values)
	}
}
