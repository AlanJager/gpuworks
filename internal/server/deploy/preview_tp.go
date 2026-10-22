package deploy

import "strings"

// SymbolizePreviewTP marks an engine-injected single-GPU placeholder while
// placement is unresolved. Each engine owns the flags that claim its TP value.
func SymbolizePreviewTP(engine Engine, launch ResolvedLaunch, templateResidual, userResidual []string) ResolvedLaunch {
	return hooks(engine).SymbolizePreviewTP(launch, templateResidual, userResidual)
}

func symbolizeBuilderTP(launch ResolvedLaunch, injectedFlag string, aliases, templateResidual, userResidual []string) ResolvedLaunch {
	if residualHasAnyFlag(templateResidual, aliases) || residualHasAnyFlag(userResidual, aliases) {
		return launch
	}
	containsBuilderPlaceholder := func(args []string) bool {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == injectedFlag && args[i+1] == "1" {
				return true
			}
		}
		return false
	}
	if !containsBuilderPlaceholder(launch.Entrypoint) && !containsBuilderPlaceholder(launch.Argv) {
		return launch
	}
	replace := func(args []string) []string {
		out := append([]string(nil), args...)
		for i := 0; i+1 < len(out); i++ {
			if out[i] == injectedFlag && out[i+1] == "1" {
				out[i+1] = "$TP_SIZE"
			}
		}
		return out
	}
	launch.Entrypoint = replace(launch.Entrypoint)
	launch.Argv = replace(launch.Argv)
	return launch
}

func residualHasAnyFlag(args, names []string) bool {
	for _, arg := range args {
		name, _, _ := strings.Cut(arg, "=")
		for _, candidate := range names {
			if name == candidate {
				return true
			}
		}
	}
	return false
}
