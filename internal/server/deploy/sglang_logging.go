// sglang_logging.go owns the SGLang severity layer, exposed as the engine's
// EngineHooks.FinalizeLaunch: SGLang's default logging format stamps only a timestamp and
// a rank prefix, never the level, so a real engine error is indistinguishable
// from request chatter and the platform's level filters and histogram cannot
// see it. SGLang honors a Python dictConfig file named by
// SGLANG_LOGGING_CONFIG_PATH (upstream PR #4592, March 2025), so the finalizer
// prepends a pre-start block that writes that file inside the container and
// declares the variable in ResolvedLaunch.Env. The script and environment are
// one launch contract; agents must support pre_start_script to execute it.

package deploy

import (
	"strconv"
	"strings"
)

// sglangLoggingEnv is the variable SGLang's configure_logger reads before
// falling back to its levelless default format.
const sglangLoggingEnv = "SGLANG_LOGGING_CONFIG_PATH"
const sglangLoggingPath = "/tmp/gpuworks-sglang-logging.json"

// sglangLoggingScript is prepended to the launch's pre-start script. It runs
// on every container start, before the engine command, so the config file and
// the variable always exist together: SGLang treats a set-but-missing path as
// a fatal configuration error. The path comes only from the resolved Env.
const sglangLoggingScript = `# gpuworks: SGLang stamps no severity by default; give it a dictConfig that does.
cat > "$SGLANG_LOGGING_CONFIG_PATH" <<'GPUWORKS_SGLANG_LOGGING'
{
  "version": 1,
  "disable_existing_loggers": false,
  "formatters": {
    "gpuworks": {
      "format": "[%(asctime)s %(process)d] %(levelname)s %(message)s",
      "datefmt": "%Y-%m-%d %H:%M:%S"
    }
  },
  "handlers": {
    "gpuworks": {
      "class": "logging.StreamHandler",
      "stream": "ext://sys.stderr",
      "formatter": "gpuworks"
    }
  },
  "loggers": {
    "httpx": {"level": "WARNING"},
    "httpcore": {"level": "WARNING"},
    "flashinfer.jit": {"level": "ERROR"}
  },
  "root": {"level": GPUWORKS_LOG_LEVEL, "handlers": ["gpuworks"]}
}
GPUWORKS_SGLANG_LOGGING`

// sglangLoggingMarker identifies the block once composed, so a second finalize
// pass over the same launch never duplicates it.
const sglangLoggingMarker = "gpuworks: SGLang stamps no severity"

// FinalizeLaunch adds the severity block to the merged launch's pre-start
// script when this launch is a plain SGLang engine command the platform can
// wrap.
func (sglangEngine) FinalizeLaunch(launch ResolvedLaunch) ResolvedLaunch {
	applySGLangLoggingSeverity(&launch)
	return launch
}

// applySGLangLoggingSeverity prepends the severity block when every gate
// passes. The gates each guard a real failure mode rather than taste:
//   - engine command mode: manual and image-entry commands belong to their
//     authors, and the wrapper needs argv the platform assembled itself;
//   - an explicit entrypoint: the agent's wrapper execs the original command
//     boundaries, which is only sound when the launch states them;
//   - no pre_start_argv: the agent rejects script and argv together, and the
//     template's wrapper wins over our additive block;
//   - no operator-provided SGLANG_LOGGING_CONFIG_PATH: whoever set it owns
//     logging;
//   - no --log-file in the final argv: SGLang's custom-config path skips its
//     own file-handler setup, so a deployment asking for file logging keeps
//     the status quo instead of silently losing the file.
func applySGLangLoggingSeverity(launch *ResolvedLaunch) {
	_, loggingOwned := launch.Env[sglangLoggingEnv]
	if launch.CommandMode != string(CommandEngine) ||
		len(launch.Entrypoint) == 0 ||
		len(launch.PreStartArgv) > 0 ||
		loggingOwned ||
		strings.Contains(launch.PreStartScript, sglangLoggingMarker) ||
		// SGLang builds put the whole engine command in Entrypoint while user
		// overrides can land in Argv; scan both.
		argvWantsSGLangLogFile(launch.Entrypoint) || argvWantsSGLangLogFile(launch.Argv) {
		return
	}
	level, ok := sglangLoggingLevel(append(append([]string{}, launch.Entrypoint...), launch.Argv...))
	if !ok {
		return
	}
	launch.Env = cloneEnv(launch.Env)
	launch.Env[sglangLoggingEnv] = sglangLoggingPath
	launch.EnvLayers.System = cloneEnv(launch.EnvLayers.System)
	launch.EnvLayers.System[sglangLoggingEnv] = sglangLoggingPath
	script := strings.ReplaceAll(sglangLoggingScript, "GPUWORKS_LOG_LEVEL", strconv.Quote(level))
	if launch.PreStartScript != "" {
		script += "\n" + launch.PreStartScript
	}
	launch.PreStartScript = script
}

// The final argv is authoritative. Do not reinterpret a user-owned config
// file, abbreviated option, or invalid level; let SGLang handle those launches.
func sglangLoggingLevel(argv []string) (string, bool) {
	params, residual, err := ReverseArgs(EngineSGLang, argv)
	level, stated := params["log_level"].(string)
	if err != nil || !stated || !sglangSingleProcessLogging(argv) {
		return "", false
	}
	// Invalid or abbreviated level options remain residual; never let a valid
	// earlier argument hide a later option that SGLang will interpret itself.
	flag := engineParams(EngineSGLang)["log_level"].Flag
	for _, arg := range residual {
		name, _, _ := strings.Cut(arg, "=")
		if name == "--config" || (strings.HasPrefix(name, "--") && strings.HasPrefix(flag, name)) {
			return "", false
		}
	}
	return strings.ToUpper(level), true
}

func argvWantsSGLangLogFile(argv []string) bool {
	for _, arg := range argv {
		if arg == "--log-file" || strings.HasPrefix(arg, "--log-file=") {
			return true
		}
	}
	return false
}
