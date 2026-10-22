package inferencecatalog

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func warningCodes(warnings []*messages.ProductError) []string {
	codes := []string{}
	for _, warning := range warnings {
		codes = append(codes, warning.Code())
	}
	return codes
}

func TestCatalogWarningsForCallableSpecs(t *testing.T) {
	serve := []string{"--port", "${API_PORT}"}
	chatOps := []APIOperation{{Type: "openai.chat"}}
	for _, test := range []struct {
		name string
		edit func(*InferenceSpec)
		want []string
	}{
		{name: "vllm declares nothing and needs nothing", edit: func(*InferenceSpec) {}, want: []string{}},
		{name: "callable without an api block", edit: func(s *InferenceSpec) { s.Engine, s.Args = "callable", serve },
			want: []string{messages.WarningCatalogCallableMetricsUndeclared, messages.WarningCatalogHealthEndpointUndeclared}},
		{name: "callable with operations only", edit: func(s *InferenceSpec) {
			s.Engine, s.Args = "callable", serve
			s.API = &APIContract{Operations: chatOps}
		}, want: []string{messages.WarningCatalogCallableMetricsUndeclared, messages.WarningCatalogHealthEndpointUndeclared}},
		{name: "callable with metrics, no health", edit: func(s *InferenceSpec) {
			s.Engine, s.Args = "callable", serve
			s.API = &APIContract{Operations: chatOps, Metrics: &inferenceapi.Metrics{Endpoint: "/metrics"}}
		}, want: []string{messages.WarningCatalogHealthEndpointUndeclared}},
		{name: "callable with health, no metrics", edit: func(s *InferenceSpec) {
			s.Engine, s.Args = "callable", serve
			s.API = &APIContract{Operations: chatOps, HealthEndpoint: "/healthz"}
		}, want: []string{messages.WarningCatalogCallableMetricsUndeclared}},
		{name: "callable with both", edit: func(s *InferenceSpec) {
			s.Engine, s.Args = "callable", serve
			s.API = &APIContract{Operations: chatOps, HealthEndpoint: "/healthz", Metrics: &inferenceapi.Metrics{Endpoint: "/metrics"}}
		}, want: []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := flatFixture()
			test.edit(&catalog.Models[0].Specs[0])
			if err := ValidateFlatCatalog(&catalog); err != nil {
				t.Fatal(err)
			}
			warnings := CatalogWarnings(catalog)
			if got := warningCodes(warnings); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("codes=%v, want %v", got, test.want)
			}
			for _, warning := range warnings {
				want := map[string]any{"model_id": catalog.Models[0].ModelID, "spec": "bf16-balanced"}
				if !reflect.DeepEqual(warning.Params(), want) || warning.Error() == "" {
					t.Fatalf("warning %s params=%v error=%q, want params %v and an English fallback", warning.Code(), warning.Params(), warning.Error(), want)
				}
			}
		})
	}
}

func TestCatalogWarningsCoverEverySpecInOrder(t *testing.T) {
	catalog := flatFixture()
	second := catalog.Models[0].Specs[0]
	second.Name = "second"
	catalog.Models[0].Specs[0].Engine = "callable"
	second.Engine = "callable"
	catalog.Models[0].Specs = append(catalog.Models[0].Specs, second)
	var specs []any
	for _, warning := range CatalogWarnings(catalog) {
		specs = append(specs, warning.Params()["spec"])
	}
	if want := []any{"bf16-balanced", "bf16-balanced", "second", "second"}; !reflect.DeepEqual(specs, want) {
		t.Fatalf("specs=%v, want %v", specs, want)
	}
}
