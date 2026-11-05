// export_args.go turns a deployment's launch configuration back into the args
// of a catalog spec, for saving a deployment as a template. It is the inverse
// of Materialize for one launch: the typed configuration, the template's
// residual and the user's residual become one argv a spec can carry, and
// materializing that argv again must give the engine the same command line.

package deploy

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// ErrorLaunchNotExportable reports a launch configuration whose command line
// cannot be reproduced from catalog args. Params carry the engine.
const ErrorLaunchNotExportable = messages.ErrorLaunchNotExportable

// ExportedArgs is a launch configuration written as catalog spec args.
type ExportedArgs struct {
	// Args is the spec's argv: the tokens the configuration's parameters
	// produce, then the template's residual, then the user's residual.
	Args []string
	// Dropped are the configuration's keys that no flag of the engine
	// carries (cpu_cores, the disk tier ceiling, internal estimates). A spec cannot
	// state them, so a deployment made from the exported spec does not
	// inherit them.
	Dropped []string
}

// ExportLaunchArgs writes quick, templateArgs and userArgs as one catalog argv.
//
// The parameter tokens are read off the engine's own Build rather than
// re-rendered: an adapter decides when a stated value becomes a flag (a dtype
// of auto says nothing) and composes some flags from several keys (vLLM's
// --speculative-config, -O3), and only Build knows those rules. Building the
// same placeholder launch with and without the configuration isolates exactly
// the tokens the configuration contributes, because the platform's own flags
// come first and the residual layers come last in every adapter.
//
// The result is checked before it is returned: materializing it the way a
// stored row is materialized and building again must give the same flags and
// values as the deployment's own launch. A configuration that fails the check
// is refused rather than exported approximately.
func ExportLaunchArgs(engine string, quick LaunchConfig, templateArgs, userArgs []string) (ExportedArgs, error) {
	selected, err := SelectEngine(engine)
	if err != nil {
		return ExportedArgs{}, err
	}
	// The served name is the platform's: every deployment states its own.
	quick.ServedModelName = nil
	full := exportBuild(selected, quick, templateArgs, userArgs)
	base := exportBuild(selected, LaunchConfig{}, templateArgs, userArgs)
	_, filteredTemplate := residualLayers(templateArgs, userArgs)
	tail := append(append([]string(nil), filteredTemplate...), userArgs...)
	prefix := len(base) - len(tail)
	if prefix < 0 || len(full) < len(base) ||
		!slices.Equal(full[:prefix], base[:prefix]) || !slices.Equal(full[len(full)-len(tail):], tail) {
		return ExportedArgs{}, notExportable(selected.Name(), "the engine command does not separate into platform, parameter and residual tokens")
	}
	parameters := full[prefix : len(full)-len(tail)]
	args := append(append([]string(nil), parameters...), tail...)

	again, err := materialize(MaterializeInput{Engine: selected.Name(), TemplateArgs: args}, true)
	if err != nil {
		return ExportedArgs{}, err
	}
	rebuilt := exportBuild(selected, again.Quick, again.TemplateResidual, nil)
	if !slices.Equal(flagValues(rebuilt), flagValues(full)) {
		return ExportedArgs{}, notExportable(selected.Name(), "the exported args do not reproduce the engine command")
	}
	dropped, err := unflaggedKeys(selected.Name(), quick)
	if err != nil {
		return ExportedArgs{}, err
	}
	return ExportedArgs{Args: args, Dropped: dropped}, nil
}

// exportBuild builds a placeholder launch: one GPU, fixed paths and port, no
// sleep, no distribution. Only the configuration and the residual layers vary
// between the builds ExportLaunchArgs compares, so the placeholders cancel out.
func exportBuild(engine Engine, quick LaunchConfig, templateArgs, userArgs []string) []string {
	launch := engine.Build(Params{
		ModelPath:       "/model",
		ServedName:      "model",
		APIPort:         30000,
		AssignedGPUs:    1,
		TensorParallel:  1,
		Quick:           quick,
		TemplateArgs:    templateArgs,
		ExtraArgs:       userArgs,
		PositionalModel: true,
	})
	// SGLang writes its whole command as the entrypoint and vLLM splits it;
	// the command is the two together either way.
	return append(append([]string(nil), launch.Entrypoint...), launch.Argv...)
}

// flagValues reads an argv as the set of flags and the values they carry,
// sorted: the engines parse flags by name, so the order the layers emit them
// in is not part of what a command means.
func flagValues(argv []string) []string {
	out := make([]string, 0, len(argv))
	for index := 0; index < len(argv); index++ {
		token := argv[index]
		if !strings.HasPrefix(token, "-") || looksLikeNegativeNumber(token) {
			out = append(out, "\x00"+token)
			continue
		}
		if strings.Contains(token, "=") || index+1 >= len(argv) ||
			(strings.HasPrefix(argv[index+1], "-") && !looksLikeNegativeNumber(argv[index+1])) {
			out = append(out, token)
			continue
		}
		out = append(out, token+"\x00"+argv[index+1])
		index++
	}
	sort.Strings(out)
	return out
}

// unflaggedKeys lists the keys quick states that no flag of this engine
// carries: the dictionary has no row for them here, or the row is the
// platform's own (ADR 0021). A key with a flag is never one of them, even when
// its value writes nothing (a dtype of auto): the command says the same then.
func unflaggedKeys(engine string, quick LaunchConfig) ([]string, error) {
	encoded, err := json.Marshal(quick)
	if err != nil {
		return nil, fmt.Errorf("encode launch configuration: %w", err)
	}
	stated := map[string]json.RawMessage{}
	if err := json.Unmarshal(encoded, &stated); err != nil {
		return nil, fmt.Errorf("encode launch configuration: %w", err)
	}
	params := engineParams(engine)
	var dropped []string
	for key := range stated {
		if spec, known := params[key]; !known || spec.Platform {
			dropped = append(dropped, key)
		}
	}
	sort.Strings(dropped)
	return dropped, nil
}

func notExportable(engine, reason string) error {
	return messages.NewProductError(ErrorLaunchNotExportable, reason, map[string]any{"engine": engine})
}
