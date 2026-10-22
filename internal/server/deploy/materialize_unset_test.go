package deploy

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

var allKVTierUnsetKeys = []string{
	"kv_offloading_size",
	"kv_disk_offload",
	"shared_kv_dir",
	"kv_offload_read_threads",
	"kv_offload_write_threads",
	"kv_disk_limit_gib",
}

func TestMaterializeUnsetRemovesSimpleTemplateValueAndNormalizesDuplicates(t *testing.T) {
	out, err := Materialize(MaterializeInput{
		Engine:       EngineVLLM,
		Unset:        []string{"kv_offloading_size", "kv_offloading_size"},
		TemplateArgs: []string{"--kv-offloading-size", "32", "--max-model-len", "8192"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Quick.KVOffloadingSize != 0 {
		t.Fatalf("disabled template value materialized as %v", out.Quick.KVOffloadingSize)
	}
	if out.Quick.MaxModelLen != 8192 {
		t.Fatalf("unrelated template value = %d, want 8192", out.Quick.MaxModelLen)
	}
	if len(out.TemplateResidual) != 0 {
		t.Fatalf("dictionary flags leaked to residual argv: %v", out.TemplateResidual)
	}
}

func TestMaterializeUnsetRejectsUnknownAndContradictoryRequestValues(t *testing.T) {
	if _, err := Materialize(MaterializeInput{Engine: EngineVLLM, Unset: []string{"not_a_parameter"}}); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("unknown unset key error = %v", err)
	}

	for name, input := range map[string]MaterializeInput{
		"quick params": {
			Engine: EngineVLLM,
			Raw:    map[string]any{"kv_offloading_size": 16},
			Unset:  []string{"kv_offloading_size"},
		},
		"extra args": {
			Engine:    EngineVLLM,
			Unset:     []string{"kv_offloading_size"},
			ExtraArgs: []string{"--kv-offloading-size", "16"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Materialize(input)
			var product *messages.ProductError
			if !errors.As(err, &product) {
				t.Fatalf("conflict error = %v, want ProductError", err)
			}
			if product.Code() != ErrorLaunchConfigUnsetConflict {
				t.Fatalf("code = %q, want %q", product.Code(), ErrorLaunchConfigUnsetConflict)
			}
			if product.Params()["key"] != "kv_offloading_size" {
				t.Fatalf("params = %#v", product.Params())
			}
		})
	}

	// A JSON null has always meant "not stated" in quick_params. Retain that
	// compatibility when a newer client also sends the explicit tombstone.
	nullValue, err := Materialize(MaterializeInput{
		Engine: EngineVLLM,
		Raw:    map[string]any{"kv_offloading_size": nil},
		Unset:  []string{"kv_offloading_size"},
	})
	if err != nil || nullValue.Quick.KVOffloadingSize != 0 {
		t.Fatalf("null compatibility result = %+v, error = %v", nullValue.Quick, err)
	}
}

func TestMaterializeUnsetRemovesOnlyPlatformVLLMDiskConnector(t *testing.T) {
	platformConnector := diskKVTransferConfig(LaunchConfig{
		KVOffloadingSize:      32,
		KVDiskOffload:         true,
		SharedKVDir:           "/mnt/kv",
		KVOffloadReadThreads:  8,
		KVOffloadWriteThreads: 12,
		KVOffloadBlockSize:    16,
	})
	out, err := Materialize(MaterializeInput{
		Engine:       EngineVLLM,
		Unset:        allKVTierUnsetKeys,
		TemplateArgs: []string{"--kv-transfer-config", platformConnector, "--max-model-len", "8192"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Quick.KVDiskOffload || out.Quick.SharedKVDir != "" || out.Quick.KVOffloadReadThreads != 0 || out.Quick.KVOffloadWriteThreads != 0 || out.Quick.KVDiskLimitGiB != 0 {
		t.Fatalf("disabled disk draft materialized: %+v", out.Quick)
	}
	if out.Quick.MaxModelLen != 8192 || len(out.TemplateResidual) != 0 {
		t.Fatalf("materialized=%+v residual=%v", out.Quick, out.TemplateResidual)
	}

	launch, err := ResolveLaunch(ResolveLaunchInput{
		Engine: vLLMEngine{},
		Params: Params{
			ModelPath:      "/model",
			APIPort:        30000,
			AssignedGPUs:   1,
			TensorParallel: 1,
			Quick:          out.Quick,
			TemplateArgs:   out.TemplateResidual,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	command := append(append([]string(nil), launch.Entrypoint...), launch.Argv...)
	if slices.Contains(command, "--kv-transfer-config") || slices.Contains(command, "--kv-offloading-size") {
		t.Fatalf("disabled KV tier leaked into final argv: %v", command)
	}
	for _, mount := range launch.Mounts {
		if mount.Target == "/mnt/kv" {
			t.Fatalf("disabled disk tier leaked into mounts: %+v", launch.Mounts)
		}
	}
}

func TestMaterializeUnsetRejectsOpaqueOrMixedVLLMConnectors(t *testing.T) {
	platform := diskKVTransferConfig(LaunchConfig{
		KVOffloadingSize:      8,
		KVDiskOffload:         true,
		SharedKVDir:           "/mnt/kv",
		KVOffloadReadThreads:  4,
		KVOffloadWriteThreads: 4,
	})
	cases := map[string]string{
		"different connector":     `{"kv_connector":"NixlConnector","kv_role":"kv_both","kv_connector_extra_config":{}}`,
		"unknown top-level field": strings.TrimSuffix(platform, "}") + `,"vendor_extension":true}`,
		"mixed tier":              `{"kv_connector":"OffloadingConnector","kv_role":"kv_both","kv_connector_extra_config":{"spec_name":"TieringOffloadingSpec","cpu_bytes_to_use":8589934592,"secondary_tiers":[{"type":"fs","root_dir":"/mnt/kv","n_read_threads":4,"n_write_threads":4},{"type":"nixl"}]}}`,
	}
	for name, connector := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Materialize(MaterializeInput{
				Engine:       EngineVLLM,
				Unset:        allKVTierUnsetKeys,
				TemplateArgs: []string{"--kv-transfer-config", connector},
			})
			var product *messages.ProductError
			if !errors.As(err, &product) || product.Code() != ErrorLaunchConfigUnsetConflict {
				t.Fatalf("opaque connector error = %v, want %s", err, ErrorLaunchConfigUnsetConflict)
			}
			if product.Params()["source"] != "template_args" {
				t.Fatalf("params = %#v", product.Params())
			}
		})
	}
}

func TestMaterializeUnsetRejectsPartialCompositeRemoval(t *testing.T) {
	platform := diskKVTransferConfig(LaunchConfig{
		KVOffloadingSize:      8,
		KVDiskOffload:         true,
		SharedKVDir:           "/mnt/kv",
		KVOffloadReadThreads:  4,
		KVOffloadWriteThreads: 4,
	})
	for name, connector := range map[string]string{
		"known platform connector": platform,
		"opaque connector":         `{"kv_connector":"NixlConnector","kv_role":"kv_both","kv_connector_extra_config":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Materialize(MaterializeInput{
				Engine:       EngineVLLM,
				Unset:        []string{"shared_kv_dir"},
				TemplateArgs: []string{"--kv-transfer-config", connector},
			})
			var product *messages.ProductError
			if !errors.As(err, &product) || product.Code() != ErrorLaunchConfigUnsetConflict {
				t.Fatalf("partial composite error = %v, want %s", err, ErrorLaunchConfigUnsetConflict)
			}
			if got := product.Params(); got["key"] != "shared_kv_dir" || got["source"] != "template_args" {
				t.Fatalf("params = %#v", got)
			}
		})
	}
}

func TestMaterializeUnsetRejectsUserDiskConnector(t *testing.T) {
	connector := diskKVTransferConfig(LaunchConfig{
		KVOffloadingSize:      8,
		KVDiskOffload:         true,
		SharedKVDir:           "/mnt/kv",
		KVOffloadReadThreads:  4,
		KVOffloadWriteThreads: 4,
	})
	_, err := Materialize(MaterializeInput{
		Engine:    EngineVLLM,
		Unset:     allKVTierUnsetKeys,
		ExtraArgs: []string{"--kv-transfer-config=" + connector},
	})
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Code() != ErrorLaunchConfigUnsetConflict {
		t.Fatalf("user connector error = %v, want %s", err, ErrorLaunchConfigUnsetConflict)
	}
	if got := product.Params(); got["source"] != "extra_args" || got["flag"] != "--kv-transfer-config" {
		t.Fatalf("params = %#v", got)
	}
}

func TestValidateParameterDependenciesRejectsUnknownAndCycles(t *testing.T) {
	definition := func(key, parent string) paramDefinition {
		return paramDefinition{ParamDefinition: ParamDefinition{Key: key, DependsOn: parent}}
	}
	if err := validateParameterDependencies([]paramDefinition{definition("child", "missing")}); err == nil || !strings.Contains(err.Error(), "unknown parameter") {
		t.Fatalf("unknown dependency error = %v", err)
	}
	if err := validateParameterDependencies([]paramDefinition{definition("a", "b"), definition("b", "a")}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error = %v", err)
	}
	if err := validateParameterDependencies([]paramDefinition{definition("root", ""), definition("child", "root")}); err != nil {
		t.Fatalf("valid dependency graph = %v", err)
	}
}

func TestMaterializeUnsetDoesNotBecomeLaunchConfigData(t *testing.T) {
	out, err := Materialize(MaterializeInput{
		Engine: EngineVLLM,
		Raw:    map[string]any{"max_model_len": 8192},
		Unset:  allKVTierUnsetKeys,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(out.Quick)
	if err != nil {
		t.Fatal(err)
	}
	// Unset removes the tier keys; the dictionary defaults that remain are the
	// ones an untouched form showed, and the encoded row carries no tombstone.
	if strings.Contains(string(encoded), "unset") {
		t.Fatalf("persisted launch config carries tombstone data: %s", encoded)
	}
	var persisted map[string]any
	if err := json.Unmarshal(encoded, &persisted); err != nil {
		t.Fatal(err)
	}
	for _, key := range allKVTierUnsetKeys {
		if _, present := persisted[key]; present {
			t.Fatalf("unset key %q persisted: %s", key, encoded)
		}
	}
	if persisted["max_model_len"] != float64(8192) {
		t.Fatalf("persisted launch config lost the stated value: %s", encoded)
	}
}
