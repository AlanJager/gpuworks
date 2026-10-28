package deploy

import (
	"encoding/json"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// ErrorExtraArgsJSONInvalid reports a user-typed engine argument whose value
// must be JSON and is not. The flag param names the argument and value the
// text the engine would have received.
const ErrorExtraArgsJSONInvalid = "EXTRA_ARGS_JSON_INVALID"

// jsonValuedFlags are the engine flags whose value is a JSON document. The
// parameter dictionary types only some of them as json, because the ones that
// several keys share (--speculative-config, --kv-transfer-config) are composed
// by the adapter and cannot carry one key's type. So the set is the dictionary's
// json-typed flags (see jsonValuedFlag) plus this short list, each flag taken
// from the engine's own documentation as one that takes a JSON object. A flag
// that merely accepts JSON among other forms (vLLM's -O level, key=value
// spellings) is left out on purpose: judging it would reject a value the engine
// reads.
var jsonValuedFlags = map[string]struct{}{
	"--compilation-config":         {},
	"--additional-config":          {},
	"--speculative-config":         {},
	"--kv-transfer-config":         {},
	"--kv-events-config":           {},
	"--reasoning-config":           {},
	"--hf-overrides":               {},
	"--override-generation-config": {},
	"--json-model-override-args":   {},
}

// jsonValuedFlag reports whether the flag takes a JSON document.
func jsonValuedFlag(flag string) bool {
	if _, ok := jsonValuedFlags[flag]; ok {
		return true
	}
	for _, definition := range parameterDefinitions {
		if definition.Type != "json" {
			continue
		}
		for _, pair := range definition.Flags {
			if pair.On == flag {
				return true
			}
		}
	}
	return false
}

// ValidateExtraArgsJSON checks the JSON-valued arguments in the argv the user
// typed into the extra-args text box. That box is split like a shell command
// line, so JSON in double quotes loses them (`{"a":1}` becomes `{a:1}`) and the
// engine only fails after the container started; this refuses it up front.
//
// It judges only the user's tokens, never a template's: template arguments are
// a real list of strings that never went through the lexer. A flag with no value
// is left to the engine and the launch checks. The --flag=value spelling is
// read too.
func ValidateExtraArgsJSON(args []string) error {
	for index := 0; index < len(args); index++ {
		flag, value, glued := strings.Cut(args[index], "=")
		if !strings.HasPrefix(flag, "--") || !jsonValuedFlag(flag) {
			continue
		}
		if !glued {
			if index+1 >= len(args) {
				continue
			}
			index++
			value = args[index]
		}
		if json.Valid([]byte(value)) {
			continue
		}
		return messages.NewProductError(ErrorExtraArgsJSONInvalid,
			flag+" takes JSON but "+value+" is not valid JSON; wrap the value in single quotes, e.g. "+flag+" '{\"key\":\"value\"}'",
			map[string]any{"flag": flag, "value": value})
	}
	return nil
}
