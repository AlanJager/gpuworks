package deploy

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Ascend sleep-mode refusals. Each carries its interpolation params; the
// human-readable text belongs to the client locale.
const (
	// ErrorAscendSleepAdditionalConfigInvalid: a template's {flag} value
	// ({value}) is not a JSON object, so weight_nz_mode cannot be merged in.
	ErrorAscendSleepAdditionalConfigInvalid = "ASCEND_SLEEP_ADDITIONAL_CONFIG_INVALID"
	// ErrorAscendSleepWeightNZConflict: extra_args sets weight_nz_mode to
	// {value} in {flag}; Ascend sleep mode requires 0.
	ErrorAscendSleepWeightNZConflict = "ASCEND_SLEEP_WEIGHT_NZ_CONFLICT"
	// ErrorAscendSleepExpandableSegments: the deployment's own environment sets
	// {variable}={value} with expandable_segments:True, which Ascend sleep
	// mode cannot offload.
	ErrorAscendSleepExpandableSegments = "ASCEND_SLEEP_EXPANDABLE_SEGMENTS"
)

const (
	vllmAdditionalConfigFlag = "--additional-config"
	vllmAscendSleepConfig    = `{"weight_nz_mode":0}`
	ascendAllocConfEnv       = "PYTORCH_NPU_ALLOC_CONF"
	ascendExpandableSegments = "expandable_segments"
	// vLLM's CUDA runtime wrapper reads VLLM_CUDART_SO_PATH only as a fallback
	// when the runtime library it looks for is not already mapped into the
	// process, which is the case on Hygon's DTK runtime. An image that maps
	// the library ignores it, and one that neither maps it nor has this file
	// fails the same lookup without it.
	vllmHygonCUDARTPath = "/opt/dtk/cuda/cuda-12/targets/x86_64-linux/lib/libcudart.so"
)

// prepareVLLMSleepable adapts the template and user layers of a sleepable
// vLLM launch on Ascend. The platform owns the template layer, so a template
// value sleep cannot run with (a nonzero weight_nz_mode, an expandable
// segments allocator) is rewritten; the same value stated by the user is an
// explicit choice and is refused instead (see validateVLLMSleepableEnv for the
// environment).
func prepareVLLMSleepable(p *Params) error {
	if !p.Sleepable || accelerator.NormalizeVendor(p.Vendor) != accelerator.VendorAscend {
		return nil
	}
	templateArgs, err := mergeAscendSleepAdditionalConfig(p.TemplateArgs, false)
	if err != nil {
		return err
	}
	extraArgs, err := mergeAscendSleepAdditionalConfig(p.ExtraArgs, true)
	if err != nil {
		return err
	}
	p.TemplateArgs, p.ExtraArgs = templateArgs, extraArgs
	if value, ok := p.TemplateEnv[ascendAllocConfEnv]; ok {
		if rewritten, changed := disableExpandableSegments(value); changed {
			env := cloneEnv(p.TemplateEnv)
			env[ascendAllocConfEnv] = rewritten
			p.TemplateEnv = env
		}
	}
	return nil
}

func mergeAscendSleepAdditionalConfig(args []string, user bool) ([]string, error) {
	out := append([]string(nil), args...)
	for index := 0; index < len(out); index++ {
		flag, value, glued := strings.Cut(out[index], "=")
		if flag != vllmAdditionalConfigFlag {
			continue
		}
		if !glued {
			if index+1 >= len(out) {
				continue
			}
			index++
			value = out[index]
		}
		if !json.Valid([]byte(value)) {
			if user {
				return nil, extraArgsJSONError(flag, value)
			}
			return nil, ascendSleepAdditionalConfigInvalid(flag, value)
		}
		decoder := json.NewDecoder(strings.NewReader(value))
		decoder.UseNumber()
		var config map[string]any
		if err := decoder.Decode(&config); err != nil || config == nil {
			return nil, ascendSleepAdditionalConfigInvalid(flag, value)
		}
		if current, stated := config["weight_nz_mode"]; stated && user && !zeroJSONNumber(current) {
			stated := jsonText(current)
			return nil, messages.NewProductError(ErrorAscendSleepWeightNZConflict,
				fmt.Sprintf("Ascend sleep mode requires weight_nz_mode=0 in %s, but extra_args sets it to %s; remove the explicit value or set it to 0", flag, stated),
				map[string]any{"flag": flag, "value": stated})
		}
		config["weight_nz_mode"] = 0
		encoded, err := json.Marshal(config)
		if err != nil {
			return nil, err
		}
		if glued {
			out[index] = flag + "=" + string(encoded)
		} else {
			out[index] = string(encoded)
		}
	}
	return out, nil
}

func ascendSleepAdditionalConfigInvalid(flag, value string) error {
	return messages.NewProductError(ErrorAscendSleepAdditionalConfigInvalid,
		"Ascend sleep mode requires the template's "+flag+" to be a JSON object so weight_nz_mode can be set to 0, but it is "+value,
		map[string]any{"flag": flag, "value": value})
}

func zeroJSONNumber(value any) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	rational, ok := new(big.Rat).SetString(number.String())
	return ok && rational.Sign() == 0
}

// disableExpandableSegments turns an expandable_segments:True setting in an
// allocator configuration off, keeping every other setting as written. It is
// set to False rather than dropped so it also overrides a True the image's
// own environment may carry.
func disableExpandableSegments(value string) (string, bool) {
	settings := strings.Split(value, ",")
	changed := false
	for index, setting := range settings {
		if expandableSegmentsEnabled(setting) {
			settings[index] = ascendExpandableSegments + ":False"
			changed = true
		}
	}
	return strings.Join(settings, ","), changed
}

func expandableSegmentsEnabled(setting string) bool {
	key, stated, ok := strings.Cut(setting, ":")
	return ok && strings.EqualFold(strings.TrimSpace(key), ascendExpandableSegments) && strings.EqualFold(strings.TrimSpace(stated), "true")
}

// validateVLLMSleepableEnv refuses an effective allocator configuration that
// Ascend sleep mode cannot run with. prepareVLLMSleepable has already turned
// the template's setting off, so what remains here is the deployment's own.
func validateVLLMSleepableEnv(engine string, p Params, env map[string]string) error {
	if engine != EngineVLLM || !p.Sleepable || accelerator.NormalizeVendor(p.Vendor) != accelerator.VendorAscend {
		return nil
	}
	value := env[ascendAllocConfEnv]
	for _, setting := range strings.Split(value, ",") {
		if expandableSegmentsEnabled(setting) {
			return messages.NewProductError(ErrorAscendSleepExpandableSegments,
				"Ascend sleep mode is incompatible with "+ascendAllocConfEnv+"="+value+"; remove expandable_segments:True or set expandable_segments:False",
				map[string]any{"variable": ascendAllocConfEnv, "value": value})
		}
	}
	return nil
}
