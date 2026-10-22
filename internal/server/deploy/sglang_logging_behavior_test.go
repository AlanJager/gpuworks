package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSGLangLoggingHonorsFinalArgumentLevel(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal(err)
		}
		t.Skip("python3 is required to execute Python logging behavior")
	}
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"explicit info", []string{"--log-level", "info"}, []string{"INFO", "WARNING", "ERROR"}},
		{"debug", []string{"--log-level", "debug"}, []string{"DEBUG", "INFO", "WARNING", "ERROR"}},
		{"error equals", []string{"--log-level=error"}, []string{"ERROR"}},
		{"last argument wins", []string{"--log-level", "debug", "--log-level=warning"}, []string{"WARNING", "ERROR"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := sglangSeverityInput(t)
			input.Params.Quick.LogLevel = ""
			input.Params.ExtraArgs = tc.args
			launch, err := ResolveLaunch(input)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "logging.json")
			cmd := exec.Command("/bin/sh", "-ec", launch.PreStartScript)
			cmd.Env = append(os.Environ(), sglangLoggingEnv+"="+path)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("prepare: %v %s", err, out)
			}
			// SGLang v0.5.14 utils/common.py configure_logger's custom-config branch
			// loads JSON with dictConfig and returns before basicConfig. Exercise the
			// real stdlib handlers, not string substitution in the JSON formatter.
			cmd = exec.Command(python, "-c", `import json, logging, logging.config, sys
with open(sys.argv[1]) as f:
    logging.config.dictConfig(json.load(f))
for level in (logging.DEBUG, logging.INFO, logging.WARNING, logging.ERROR):
    logging.log(level, "probe-%s", logging.getLevelName(level))
`, path)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("logging: %v %s", err, out)
			}
			var got []string
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				parts := strings.Split(line, "probe-")
				if len(parts) != 2 {
					t.Fatalf("unexpected line: %q", line)
				}
				level := parts[1]
				if !strings.Contains(line, "] "+level+" ") {
					t.Fatalf("level is not stamped: %q", line)
				}
				got = append(got, level)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("emitted %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSGLangLoggingPreservesUserOwnedAndParallelLogging(t *testing.T) {
	for _, args := range [][]string{
		{"--config", "/config.yaml"}, {"--config=/config.yaml"},
		{"--log-level", "not-a-level"}, {"--log-level"}, {"--log-l", "error"},
		{"--log-file=/logs/engine.log"}, {"--tp", "2"}, {"--pp-size=2"},
		{"--dp-size", "2"}, {"--ep-size", "2"}, {"--nnodes", "2"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			launch := ResolvedLaunch{CommandMode: string(CommandEngine), Entrypoint: []string{"python", "-m", "sglang.launch_server"}, Argv: args}
			before, _ := json.Marshal(launch)
			applySGLangLoggingSeverity(&launch)
			after, _ := json.Marshal(launch)
			if string(before) != string(after) {
				t.Fatalf("user/parallel launch changed: %s", after)
			}
		})
	}
	for _, layer := range []string{"template", "user"} {
		t.Run(layer, func(t *testing.T) {
			input := sglangSeverityInput(t)
			env := map[string]string{sglangLoggingEnv: "/operator/logging.json"}
			switch layer {
			case "template":
				input.Params.TemplateEnv = env
			case "user":
				input.ContainerOverrides.Env = env
			}
			launch, err := ResolveLaunch(input)
			if err != nil {
				t.Fatal(err)
			}
			if launch.Env[sglangLoggingEnv] != env[sglangLoggingEnv] || launch.PreStartScript != "" {
				t.Fatalf("operator logging replaced: %+v", launch)
			}
		})
	}
}

func TestSGLangLoggingPreparationFailureStopsBeforeEngine(t *testing.T) {
	launch, err := ResolveLaunch(sglangSeverityInput(t))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-ec", launch.PreStartScript+"\nprintf engine-started")
	cmd.Env = append(os.Environ(), sglangLoggingEnv+"="+filepath.Join(t.TempDir(), "missing", "logging.json"))
	out, err := cmd.CombinedOutput()
	if err == nil || strings.Contains(string(out), "engine-started") {
		t.Fatalf("missing config must not start the engine: err=%v out=%s", err, out)
	}
}

func TestSGLangUnsetLogLevelKeepsEngineDefault(t *testing.T) {
	input := sglangSeverityInput(t)
	input.Params.Quick.LogLevel = ""
	launch, err := ResolveLaunch(input)
	if err != nil {
		t.Fatal(err)
	}
	if launch.PreStartScript != "" || launch.Env[sglangLoggingEnv] != "" || strings.Contains(strings.Join(launch.Entrypoint, " "), "--log-level") {
		t.Fatalf("unset must not invent a logging threshold: %+v", launch)
	}
}

func TestManualCommandOwnsPreparationExactlyOnce(t *testing.T) {
	for _, argvPreparation := range []bool{false, true} {
		t.Run(fmt.Sprint(argvPreparation), func(t *testing.T) {
			input := sglangSeverityInput(t)
			dir := t.TempDir()
			count := filepath.Join(dir, "count")
			prepare := "printf prepared >> '" + count + "'"
			if argvPreparation {
				input.PreStartArgv = []string{"/bin/sh", "-c", prepare}
			} else {
				input.PreStartScript = prepare
			}
			automatic, err := ResolveLaunch(input)
			if err != nil {
				t.Fatal(err)
			}
			script := "set -eu\n"
			if automatic.Env[sglangLoggingEnv] != "" {
				script += "export " + sglangLoggingEnv + "='" + filepath.Join(dir, "logging.json") + "'\n"
			}
			script += automatic.PreStartScript + "\n"
			if argvPreparation {
				script += prepare + "\n"
			}
			script += "printf engine-started"
			input.ContainerOverrides.CommandMode = string(CommandManual)
			wireScript, _ := json.Marshal(script)
			wire := `{"command_mode":"manual","command_includes_preparation":true,"cmd_display":` + string(wireScript) + `}`
			if err := json.Unmarshal([]byte(wire), &input.ContainerOverrides); err != nil {
				t.Fatal(err)
			}
			roundTrip, _ := json.Marshal(input.ContainerOverrides)
			if !strings.Contains(string(roundTrip), `"command_includes_preparation":true`) {
				t.Fatalf("ownership lost on persistence: %s", roundTrip)
			}
			manual, err := ResolveLaunch(input)
			if err != nil {
				t.Fatal(err)
			}
			if manual.PreStartScript != "" || len(manual.PreStartArgv) != 0 {
				t.Fatalf("preparation would run twice: %+v", manual)
			}
			command := append(append([]string{}, manual.Entrypoint...), manual.Argv...)
			out, err := exec.Command(command[0], command[1:]...).CombinedOutput()
			if err != nil || string(out) != "engine-started" {
				t.Fatalf("manual command: %v %s", err, out)
			}
			got, err := os.ReadFile(count)
			if err != nil || string(got) != "prepared" {
				t.Fatalf("preparation count: %v %q", err, got)
			}
			input.ContainerOverrides.CommandMode = string(CommandEngine)
			input.ContainerOverrides.CmdDisplay = ""
			input.ContainerOverrides.CommandIncludesPreparation = false
			restored, err := ResolveLaunch(input)
			if err != nil || !reflect.DeepEqual(restored, automatic) {
				t.Fatalf("return to form did not restore launch: %v", err)
			}
		})
	}
}

func TestLegacyManualCommandKeepsTemplatePreparation(t *testing.T) {
	input := sglangSeverityInput(t)
	input.PreStartScript = "echo template setup"
	input.ContainerOverrides.CommandMode = string(CommandManual)
	input.ContainerOverrides.CmdDisplay = "python -m sglang.launch_server --model-path /model"
	launch, err := ResolveLaunch(input)
	if err != nil {
		t.Fatal(err)
	}
	if launch.PreStartScript != input.PreStartScript {
		t.Fatalf("legacy manual preparation changed: %+v", launch)
	}
}
