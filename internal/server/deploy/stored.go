// stored.go normalizes a deployment row written before the launch
// configuration had a single representation. It is a pure function so the
// startup pass that rewrites those rows has nothing to decide.

package deploy

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// StoredLaunchConfig is a deployment's launch configuration as three stored
// columns: the typed configuration, the user's free-form arguments and the
// catalog spec arguments kept in the selection snapshot.
type StoredLaunchConfig struct {
	Engine          string
	QuickParamsJSON string
	ExtraArgsText   string
	TemplateArgs    []string
}

// NormalizeStoredLaunchConfig rewrites a stored row into the materialized form:
// a complete typed configuration, the user's residual arguments and the
// template's residual arguments. It keeps the precedence the row was written
// under - extra_args beat the fields then, and re-deciding that now would
// change a running deployment's command - so it never reports a conflict.
//
// It is idempotent: an already-normalized row comes back unchanged with
// changed=false, which is what lets the startup pass run on every boot.
func NormalizeStoredLaunchConfig(in StoredLaunchConfig) (StoredLaunchConfig, bool, error) {
	raw := map[string]any{}
	if text := strings.TrimSpace(in.QuickParamsJSON); text != "" && text != "null" {
		if err := json.Unmarshal([]byte(text), &raw); err != nil {
			return in, false, fmt.Errorf("stored quick_params: %w", err)
		}
	}
	renameStoredValues(in.Engine, raw)
	extra, err := ParseArgsText(in.ExtraArgsText)
	if err != nil {
		return in, false, fmt.Errorf("stored extra_args: %w", err)
	}
	materialized, err := materialize(MaterializeInput{
		Engine: in.Engine, Raw: raw, TemplateArgs: in.TemplateArgs, ExtraArgs: extra,
	}, true)
	if err != nil {
		return in, false, err
	}
	encoded, err := json.Marshal(materialized.Quick)
	if err != nil {
		return in, false, fmt.Errorf("stored quick_params: %w", err)
	}
	out := StoredLaunchConfig{
		Engine:          in.Engine,
		QuickParamsJSON: string(encoded),
		ExtraArgsText:   FormatArgsText(materialized.UserResidual),
		TemplateArgs:    nonEmptyArgs(materialized.TemplateResidual),
	}
	changed := out.QuickParamsJSON != in.QuickParamsJSON ||
		out.ExtraArgsText != in.ExtraArgsText ||
		!slices.Equal(out.TemplateArgs, nonEmptyArgs(in.TemplateArgs))
	return out, changed, nil
}

// nonEmptyArgs keeps "no arguments" a single value, so an empty residual never
// looks like a change against a column that never had one.
func nonEmptyArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	return args
}

// storedValueRenames maps a value the dictionary once offered for an engine to
// the one that engine understands. SGLang's speculative list used to carry
// vLLM's method names: SGLang calls MTP NEXTN and a separate draft model
// STANDALONE, and refuses the vLLM names. Only the stored typed configuration
// is renamed, here, once, so the preview and the launch keep reading one
// stored value; a token the user typed into the command stays as typed.
var storedValueRenames = map[string]map[string]map[string]string{
	EngineSGLang: {"speculative_algorithm": {"mtp": "NEXTN", "draft_model": "STANDALONE"}},
}

func renameStoredValues(engine string, raw map[string]any) {
	for key, renames := range storedValueRenames[engine] {
		text, ok := raw[key].(string)
		if !ok {
			continue
		}
		if renamed, ok := renames[strings.ToLower(strings.TrimSpace(text))]; ok {
			raw[key] = renamed
		}
	}
}
