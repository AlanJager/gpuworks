package deploy

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

const (
	vllmAdditionalConfigFlag = "--additional-config"
	vllmAscendSleepConfig    = `{"weight_nz_mode":0}`
	vllmHygonCUDARTPath      = "/opt/dtk/cuda/cuda-12/targets/x86_64-linux/lib/libcudart.so"
)

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
			return nil, invalidVLLMSleepConfig("Ascend sleep mode requires --additional-config to be a JSON object so weight_nz_mode can be set to 0", map[string]any{"flag": flag, "value": value})
		}
		decoder := json.NewDecoder(strings.NewReader(value))
		decoder.UseNumber()
		var config map[string]any
		if err := decoder.Decode(&config); err != nil || config == nil {
			return nil, invalidVLLMSleepConfig("Ascend sleep mode requires --additional-config to be a JSON object so weight_nz_mode can be set to 0", map[string]any{"flag": flag, "value": value})
		}
		if current, stated := config["weight_nz_mode"]; stated && user && !zeroJSONNumber(current) {
			return nil, invalidVLLMSleepConfig(
				fmt.Sprintf("Ascend sleep mode requires weight_nz_mode=0 in --additional-config, but extra_args sets it to %s; remove the explicit value or set it to 0", jsonText(current)),
				map[string]any{"flag": flag, "key": "weight_nz_mode", "value": current, "required_value": 0},
			)
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

func zeroJSONNumber(value any) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	rational, ok := new(big.Rat).SetString(number.String())
	return ok && rational.Sign() == 0
}

func validateVLLMSleepableEnv(engine string, p Params, env map[string]string) error {
	if engine != EngineVLLM || !p.Sleepable || accelerator.NormalizeVendor(p.Vendor) != accelerator.VendorAscend {
		return nil
	}
	const name = "PYTORCH_NPU_ALLOC_CONF"
	value := env[name]
	for _, setting := range strings.Split(value, ",") {
		key, stated, ok := strings.Cut(setting, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), "expandable_segments") && strings.EqualFold(strings.TrimSpace(stated), "true") {
			detail := "Ascend sleep mode is incompatible with PYTORCH_NPU_ALLOC_CONF=expandable_segments:True; remove that setting or set expandable_segments:False"
			return invalidVLLMSleepConfig(detail, map[string]any{"variable": name, "value": value})
		}
	}
	return nil
}

func invalidVLLMSleepConfig(detail string, params map[string]any) error {
	params["detail"] = detail
	return messages.NewProductError(messages.ErrorFieldInvalid, detail, params)
}
