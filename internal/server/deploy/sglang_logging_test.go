package deploy

import (
	"encoding/json"
	"strings"
	"testing"
)

// sglangSeverityInput is the smallest real-engine resolve input that injects
// the severity block: a platform-owned SGLang command with an entrypoint.
func sglangSeverityInput(t *testing.T) ResolveLaunchInput {
	t.Helper()
	engine, err := SelectEngine(EngineSGLang)
	if err != nil {
		t.Fatal(err)
	}
	return ResolveLaunchInput{
		Engine: engine,
		Params: Params{ModelPath: "/model", APIPort: 30000, ServedName: "qwen", Quick: LaunchConfig{LogLevel: "info"}},
	}
}

func TestResolveLaunchSGLangInjectsSeverityScript(t *testing.T) {
	launch, err := ResolveLaunch(sglangSeverityInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(launch.PreStartScript, sglangLoggingMarker) {
		t.Fatalf("pre-start script does not contain the severity block:\n%s", launch.PreStartScript)
	}
	if launch.Env[sglangLoggingEnv] != sglangLoggingPath || launch.EnvLayers.System[sglangLoggingEnv] != sglangLoggingPath {
		t.Fatalf("logging environment is missing from launch/provenance: %+v", launch)
	}
	if strings.Contains(launch.PreStartScript, "export ") || !strings.Contains(launch.PreStartScript, `"$SGLANG_LOGGING_CONFIG_PATH"`) {
		t.Fatal("pre-start must consume the resolved environment, not decide it again")
	}
	if got := strings.Join(launch.Entrypoint, " "); !strings.HasPrefix(got, "python -m sglang.launch_server") {
		t.Fatalf("entrypoint = %q, want the untouched engine command", got)
	}
	// Finalizing twice over the same launch must not stack a second block.
	if again := (sglangEngine{}).FinalizeLaunch(launch); again.PreStartScript != launch.PreStartScript {
		t.Fatal("second finalize duplicated the severity block")
	}
}

func TestSGLangSeverityComposesWithTemplatePreStart(t *testing.T) {
	input := sglangSeverityInput(t)
	input.PreStartScript = "echo template setup"
	launch, err := ResolveLaunch(input)
	if err != nil {
		t.Fatal(err)
	}
	markerAt := strings.Index(launch.PreStartScript, sglangLoggingMarker)
	templateAt := strings.Index(launch.PreStartScript, "echo template setup")
	if markerAt < 0 || templateAt < 0 || markerAt > templateAt {
		t.Fatalf("severity block must run before the template script:\n%s", launch.PreStartScript)
	}
}

func TestResolveLaunchSGLangSeverityGates(t *testing.T) {
	cases := []struct {
		name     string
		mutator  func(*testing.T, *ResolveLaunchInput)
		directGx bool // gate unreachable through the real engine; assert on the gate itself
	}{
		{
			name: "operator already configured logging",
			mutator: func(t *testing.T, in *ResolveLaunchInput) {
				in.ContainerOverrides.Env = map[string]string{sglangLoggingEnv: "/my/logging.json"}
			},
		},
		{
			name:    "template owns pre_start_argv",
			mutator: func(t *testing.T, in *ResolveLaunchInput) { in.PreStartArgv = []string{"prepare"} },
		},
		{
			name: "manual command mode",
			mutator: func(t *testing.T, in *ResolveLaunchInput) {
				in.ContainerOverrides.CommandMode = string(CommandManual)
				in.ContainerOverrides.CmdDisplay = "# launch\nsglang.launch_server --model-path /model"
			},
		},
		{
			name: "image-entry command mode",
			mutator: func(t *testing.T, in *ResolveLaunchInput) {
				in.ContainerOverrides.CommandMode = string(CommandImageEntry)
				in.ContainerOverrides.CmdDisplay = "sglang.launch_server --model-path /model"
			},
		},
		{
			name: "engine without the finalizer",
			mutator: func(t *testing.T, in *ResolveLaunchInput) {
				engine, err := SelectEngine(EngineVLLM)
				if err != nil {
					t.Fatal(err)
				}
				in.Engine = engine
			},
		},
		{
			// The real SGLang Build always states an entrypoint, so the guard
			// is defensive; exercise it against the gate directly.
			name:     "no explicit entrypoint to wrap",
			directGx: true,
		},
		{
			name: "sglang writes logs to a file",
			mutator: func(t *testing.T, in *ResolveLaunchInput) {
				in.Params.ExtraArgs = []string{"--log-file", "/logs/sglang.log"}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.directGx {
				launch := ResolvedLaunch{
					CommandMode: string(CommandEngine),
					Argv:        []string{"--model-path", "/model"},
				}
				applySGLangLoggingSeverity(&launch)
				if strings.Contains(launch.PreStartScript, sglangLoggingMarker) {
					t.Fatal("severity block injected without an entrypoint to wrap")
				}
				return
			}
			input := sglangSeverityInput(t)
			tc.mutator(t, &input)
			launch, err := ResolveLaunch(input)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(launch.PreStartScript, sglangLoggingMarker) {
				t.Fatalf("severity block injected but this launch must keep the status quo:\n%s", launch.PreStartScript)
			}
		})
	}
}

func TestSGLangLoggingDictConfigContract(t *testing.T) {
	launch, err := ResolveLaunch(sglangSeverityInput(t))
	if err != nil {
		t.Fatal(err)
	}
	script := launch.PreStartScript
	start := strings.Index(script, "{\n")
	end := strings.Index(script, "\nGPUWORKS_SGLANG_LOGGING")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("severity script does not embed the dictConfig between heredoc markers")
	}
	var cfg struct {
		Version                int  `json:"version"`
		DisableExistingLoggers bool `json:"disable_existing_loggers"`
		Formatters             map[string]struct {
			Format  string `json:"format"`
			Datefmt string `json:"datefmt"`
		} `json:"formatters"`
		Root struct {
			Level    string   `json:"level"`
			Handlers []string `json:"handlers"`
		} `json:"root"`
	}
	if err := json.Unmarshal([]byte(script[start:end]), &cfg); err != nil {
		t.Fatalf("embedded dictConfig is not valid JSON: %v", err)
	}
	if cfg.Version != 1 {
		t.Fatalf("version = %d, want 1", cfg.Version)
	}
	// True would silence loggers configured before configure_logger runs.
	if cfg.DisableExistingLoggers {
		t.Fatal("disable_existing_loggers must be false")
	}
	formatter, ok := cfg.Formatters["gpuworks"]
	if !ok {
		t.Fatal("gpuworks formatter missing")
	}
	for _, field := range []string{"%(asctime)s", "%(levelname)s", "%(message)s"} {
		if !strings.Contains(formatter.Format, field) {
			t.Fatalf("format %q lacks %s: no severity without it", formatter.Format, field)
		}
	}
	if cfg.Root.Level != "INFO" {
		t.Fatalf("root level = %q, want INFO (SGLang's default threshold)", cfg.Root.Level)
	}
	if len(cfg.Root.Handlers) == 0 {
		t.Fatal("root has no handler: nothing would be emitted")
	}

	// The rendered line is what the agent's per-line classifier sees; pin the
	// exact shape so a format edit that silences level parsing fails here
	// first: a space-bounded uppercase level token after the bracket prefix.
	rendered := strings.NewReplacer(
		"%(asctime)s", "2026-09-19 10:32:44",
		"%(process)d", "4242",
		"%(levelname)s", "INFO",
		"%(message)s", "Load weight end. weight mem=15.54 GB",
	).Replace(formatter.Format)
	if want := "[2026-09-19 10:32:44 4242] INFO Load weight end. weight mem=15.54 GB"; rendered != want {
		t.Fatalf("rendered sample = %q, want %q", rendered, want)
	}
}

func TestSGLangLoggingFormChoices(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raw       map[string]any
		template  []string
		wantLevel string
	}{
		{"untouched form", map[string]any{"enable_cache_report": true, "enable_metrics": true, "log_level": engineParams(EngineSGLang)["log_level"].Default}, nil, "info"},
		{"unset", map[string]any{"log_level": ""}, nil, ""},
		{"unset overrides template", map[string]any{"log_level": ""}, []string{"--log-level", "error"}, ""},
		{"edited level", map[string]any{"log_level": "debug"}, nil, "debug"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Materialize(MaterializeInput{Engine: EngineSGLang, Raw: tc.raw, TemplateArgs: tc.template})
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateLaunchConfigForEngine(EngineSGLang, m.Quick); err != nil {
				t.Fatal(err)
			}
			input := sglangSeverityInput(t)
			input.Params.Quick = m.Quick
			input.Params.TemplateArgs = m.TemplateResidual
			launch, err := ResolveLaunch(input)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantLevel == "" {
				if launch.Env[sglangLoggingEnv] != "" || launch.PreStartScript != "" || strings.Contains(strings.Join(launch.Entrypoint, " "), "--log-level") {
					t.Fatalf("unset added logging configuration: %+v", launch)
				}
			} else if launch.Env[sglangLoggingEnv] != sglangLoggingPath || !strings.Contains(launch.PreStartScript, `"level": "`+strings.ToUpper(tc.wantLevel)+`"`) || !strings.Contains(strings.Join(launch.Entrypoint, " "), "--log-level "+tc.wantLevel) {
				t.Fatalf("form choice not reflected in launch: %+v", launch)
			}
		})
	}
}
