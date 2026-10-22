package deploy

import (
	"fmt"
	"strings"
)

// EffectiveCommand is the command context shared by parameter materialization
// and final launch assembly. InterpretEngineDialect is false for manual mode:
// arbitrary user shell or argv is intentionally not parsed as an engine
// adapter dialect.
type EffectiveCommand struct {
	Mode                   CommandMode
	Entrypoint             []string
	InterpretEngineDialect bool

	templateEntrypoint bool
}

// ResolveEffectiveCommand applies the command-mode and entrypoint precedence
// ResolveLaunch uses: engine mode unless the deployment chose otherwise, and
// the template entrypoint as a starting point the deployment override replaces.
func ResolveEffectiveCommand(overrides ContainerOverrides, templateEntrypoint []string) (EffectiveCommand, error) {
	if err := overrides.Validate(); err != nil {
		return EffectiveCommand{}, fmt.Errorf("invalid container overrides: %w", err)
	}
	mode := CommandMode(overrides.CommandMode)
	if mode == "" {
		mode = CommandEngine
	}
	var entrypoint []string
	templateEffective := false
	if len(templateEntrypoint) > 0 {
		entrypoint = append([]string(nil), templateEntrypoint...)
		templateEffective = true
	}
	if strings.TrimSpace(overrides.EntrypointDisplay) != "" {
		override, err := parseOptionalCommand(overrides.EntrypointDisplay)
		if err != nil {
			return EffectiveCommand{}, fmt.Errorf("invalid deployment entrypoint: %w", err)
		}
		entrypoint = override
		templateEffective = false
	}
	return EffectiveCommand{
		Mode:                   mode,
		Entrypoint:             entrypoint,
		InterpretEngineDialect: mode != CommandManual,
		templateEntrypoint:     templateEffective,
	}, nil
}
