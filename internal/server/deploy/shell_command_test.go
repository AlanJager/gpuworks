package deploy

import (
	"encoding/json"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func commandJSON(t *testing.T, args []string) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestOpaqueShellCommandForms(t *testing.T) {
	const script = "vllm serve /model"
	for name, test := range map[string]struct {
		entrypoint []string
		argv       []string
		wantScript string
		wantOK     bool
	}{
		"sh short option cluster": {
			entrypoint: []string{"/bin/sh"}, argv: []string{"-ec", script}, wantScript: script, wantOK: true,
		},
		"bash short option cluster": {
			entrypoint: []string{"/bin/bash"}, argv: []string{"-euc", script}, wantScript: script, wantOK: true,
		},
		"env bash": {
			entrypoint: []string{"/usr/bin/env"}, argv: []string{"bash", "-c", script}, wantScript: script, wantOK: true,
		},
		"env options and assignment": {
			entrypoint: []string{"/usr/bin/env"}, argv: []string{"-i", "FOO=1", "/bin/sh", "-lc", script}, wantScript: script, wantOK: true,
		},
		"env split string": {
			entrypoint: []string{"/usr/bin/env"}, argv: []string{"-S", "bash -ec 'vllm serve /model'"}, wantScript: script, wantOK: true,
		},
		"direct executable": {
			entrypoint: []string{"vllm"}, argv: []string{"serve", "/model"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			gotScript, gotOK := opaqueShellCommand(test.entrypoint, test.argv)
			if gotScript != test.wantScript || gotOK != test.wantOK {
				t.Fatalf("opaqueShellCommand(%v, %v) = %q, %v; want %q, %v", test.entrypoint, test.argv, gotScript, gotOK, test.wantScript, test.wantOK)
			}
		})
	}
}

func TestStaticShellWordsResolveLiteralFragments(t *testing.T) {
	script := `# environment
VLLM_TEST=1

vllm serve /model --engram-confi'g' '{"cpu_offload":true,"dp_shared_memory":true}'`
	words, err := staticShellWords(script)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"VLLM_TEST=1", "vllm", "serve", "/model", "--engram-config", engramTestConfig}
	if !reflect.DeepEqual(words, want) {
		t.Fatalf("words = %#v, want %#v", words, want)
	}
	if !inspectEngramArguments(words).present {
		t.Fatal("statically reconstructed Engram option was not recognized")
	}
}

func TestStaticShellWordsSkipsHeredocData(t *testing.T) {
	script := "cat <<'JSON'\n{\"command\":\"vllm --engram-config\",\"dynamic\":\"$COMMAND\"}\nJSON\n" +
		"vllm serve /model --engram-confi'g' '" + engramTestConfig + "'"
	words, err := staticShellWords(script)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cat", "vllm", "serve", "/model", "--engram-config", engramTestConfig}
	if !reflect.DeepEqual(words, want) {
		t.Fatalf("words = %#v, want %#v", words, want)
	}
	if !inspectEngramArguments(words).shared {
		t.Fatal("the engine config after the heredoc was not recognized")
	}
}

func TestStaticShellWordsRejectDynamicShapes(t *testing.T) {
	for _, script := range []string{
		"vllm serve /model $ENGRAM_ARGS",
		"vllm serve /model `cat args`",
		"vllm serve /model; echo done",
		"vllm serve /model --engram-confi'g",
		"vllm serve /model --engram-confi{g,}",
		"cat <<'JSON'\n{\"unterminated\":true}\n",
	} {
		t.Run(script, func(t *testing.T) {
			if _, err := staticShellWords(script); err == nil {
				t.Fatalf("staticShellWords(%q) succeeded for a dynamic shape", script)
			}
		})
	}
}

func TestAscendCompatRejectsShellOptionAndEnvForms(t *testing.T) {
	for name, command := range map[string][]string{
		"sh cluster":       {"/bin/sh", "-ec", "vllm serve /model"},
		"bash cluster":     {"/bin/bash", "-euc", "vllm serve /model"},
		"env bash":         {"/usr/bin/env", "bash", "-c", "vllm serve /model"},
		"env with options": {"/usr/bin/env", "-i", "FOO=1", "/bin/bash", "-lc", "vllm serve /model"},
		"unknown shell":    {"/bin/sh", "--unsupported", "-c", "vllm serve /model"},
	} {
		t.Run(name, func(t *testing.T) {
			input := ascendCompatLaunchInput(proto.RuntimePreferenceUdocker)
			input.ContainerOverrides = ContainerOverrides{
				CommandMode: string(CommandManual), CmdDisplay: commandJSON(t, command),
				Env: map[string]string{"VLLM_USE_V2_MODEL_RUNNER": "1"},
			}
			_, err := ResolveLaunch(input)
			requireLaunchProductError(t, err, messages.ErrorAscendCompatManualUnsupported)
		})
	}
}

func TestEngramShellPreflightUsesStaticWordsAndFailsClosed(t *testing.T) {
	t.Run("literal quote concatenation", func(t *testing.T) {
		input := engramLaunchInput(proto.RuntimePreferenceDocker)
		input.ContainerOverrides = ContainerOverrides{
			CommandMode: string(CommandManual), CommandIncludesPreparation: true,
			CmdDisplay: `vllm serve /model --engram-confi'g' '{"cpu_offload":true,"dp_shared_memory":true}'`,
		}
		_, err := ResolveLaunch(input)
		requireLaunchProductError(t, err, messages.ErrorEngramManualUnsupported)
	})
	for name, command := range map[string][]string{
		"env wrapper":        {"/usr/bin/env", "bash", "-euc", `vllm serve /model --engram-confi'g' '{"cpu_offload":true,"dp_shared_memory":true}'`},
		"dynamic expansion":  {"/bin/sh", "-ec", "vllm serve /model $ENGRAM_ARGS"},
		"dynamic executable": {"/bin/sh", "-ec", "exec $COMMAND $ARGS"},
		"nested shell":       {"/bin/sh", "-ec", "bash -c 'vllm serve /model'"},
		"eval":               {"/bin/sh", "-ec", "eval 'vllm serve /model'"},
		"source":             {"/bin/sh", "-ec", ". /tmp/engine-args\nvllm serve /model"},
		"xargs":              {"/bin/sh", "-ec", "xargs vllm serve </tmp/engine-args"},
		"unknown wrapper":    {"/bin/sh", "--unsupported", "-c", "vllm serve /model"},
	} {
		t.Run(name, func(t *testing.T) {
			input := engramLaunchInput(proto.RuntimePreferenceDocker)
			input.ContainerOverrides = ContainerOverrides{CommandMode: string(CommandManual), CmdDisplay: commandJSON(t, command)}
			_, err := ResolveLaunch(input)
			requireLaunchProductError(t, err, messages.ErrorEngramManualUnsupported)
		})
	}
	t.Run("engine script file remains allowed", func(t *testing.T) {
		input := engramLaunchInput(proto.RuntimePreferenceDocker)
		input.Entrypoint = []string{"bash", "/opt/gpuworks/tensorfold/serve.sh"}
		if _, err := ResolveLaunch(input); err != nil {
			t.Fatalf("engine script file was mistaken for an inline manual command: %v", err)
		}
	})
	t.Run("static ordinary shell remains allowed", func(t *testing.T) {
		input := engramLaunchInput(proto.RuntimePreferenceDocker)
		input.ContainerOverrides = ContainerOverrides{
			CommandMode: string(CommandManual),
			CmdDisplay:  commandJSON(t, []string{"/usr/bin/env", "bash", "-euc", "# environment\nVLLM_TEST=1\nvllm serve /model"}),
		}
		if _, err := ResolveLaunch(input); err != nil {
			t.Fatalf("static ordinary shell was rejected: %v", err)
		}
	})
}
