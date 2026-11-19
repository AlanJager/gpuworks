package deploy

import (
	"strings"
	"testing"
)

// A manual command written as a shell script is held to the GPUs placement
// assigned by the flags the script states, as an argv command is: a script
// that states a smaller world would leave assigned cards idle (GWKS-415).
// Only what the script states unambiguously is judged.
func TestManualScriptTopologyIsHeldToAssignedGPUs(t *testing.T) {
	sglang := "exec python -m sglang.launch_server --model-path /model --tp-size %TP% --host 127.0.0.1 --port 30000"
	vllm := "exec vllm serve /model --tensor-parallel-size %TP% --port 8000"
	script := func(template, tp string) string { return strings.ReplaceAll(template, "%TP%", tp) }
	for _, test := range []struct {
		name    string
		engine  Engine
		mode    string
		command string
		entry   []string
		wantErr string
	}{
		{name: "sglang script stating one card of two", engine: sglangEngine{}, mode: "manual", command: script(sglang, "1"),
			wantErr: "sglang topology: tp-size 1 × pp-size 1 requires 1 GPUs, but placement assigned 2"},
		{name: "vllm script stating one card of two", engine: vLLMEngine{}, mode: "manual", command: script(vllm, "1"),
			wantErr: "vllm topology: tensor parallel 1 × pipeline parallel 1 requires 1 GPUs, but placement assigned 2"},
		{name: "sglang script stating more cards than assigned", engine: sglangEngine{}, mode: "manual", command: script(sglang, "4"),
			wantErr: "requires 4 GPUs, but placement assigned 2"},
		{name: "sglang argv stating one card of two", engine: sglangEngine{}, mode: "manual", command: `["python","-m","sglang.launch_server","--model-path","/model","--tp-size","1"]`,
			wantErr: "requires 1 GPUs, but placement assigned 2"},
		{name: "sglang script using both cards", engine: sglangEngine{}, mode: "manual", command: script(sglang, "2")},
		{name: "vllm script using both cards", engine: vLLMEngine{}, mode: "manual", command: script(vllm, "2")},
		{name: "environment and comment block before a consistent command", engine: sglangEngine{}, mode: "manual",
			command: "# single card fallback: --tp-size 1\nHF_HUB_OFFLINE=1\n  # --tp-size 1\n" + script(sglang, "2")},
		{name: "degree the shell still expands", engine: sglangEngine{}, mode: "manual", command: script(sglang, "$TP")},
		{name: "branches stating different degrees", engine: sglangEngine{}, mode: "manual",
			command: "if [ -n \"$SMALL\" ]; then\n  " + script(sglang, "1") + "\nelse\n  " + script(sglang, "2") + "\nfi"},
		{name: "degree that is not a number", engine: sglangEngine{}, mode: "manual", command: script(sglang, "auto")},
		{name: "flag inside quoted text", engine: sglangEngine{}, mode: "manual", command: "echo 'starting with --tp-size 1'\n" + script(sglang, "2")},
		{name: "script that does not split into words", engine: sglangEngine{}, mode: "manual", command: "echo 'unterminated --tp-size 1"},
		{name: "script stating no degree", engine: sglangEngine{}, mode: "manual", command: "exec python3 custom_server.py --port 30000"},
		{name: "template shell entrypoint is not a manual command", engine: sglangEngine{}, entry: []string{"/bin/sh", "-c", script(sglang, "1")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ResolveLaunch(ResolveLaunchInput{
				Engine: test.engine, Params: Params{ModelPath: "/model", AssignedGPUs: 2, TensorParallel: 2}, Entrypoint: test.entry,
				ContainerOverrides: ContainerOverrides{CommandMode: test.mode, CmdDisplay: test.command},
			})
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("ResolveLaunch() = %v, want accepted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ResolveLaunch() = %v, want error containing %q", err, test.wantErr)
			}
		})
	}
}

// The launch of a deployment as stored runs a manual script as written, so a
// deployment stored before scripts were judged keeps starting; an argv command
// was always judged and still is.
func TestStoredLaunchRunsAManualScriptAsWritten(t *testing.T) {
	input := func(command string) ResolveLaunchInput {
		return ResolveLaunchInput{
			Engine: sglangEngine{}, Params: Params{ModelPath: "/model", AssignedGPUs: 2, TensorParallel: 2}, StoredLaunch: true,
			ContainerOverrides: ContainerOverrides{CommandMode: "manual", CmdDisplay: command},
		}
	}
	script := "exec python -m sglang.launch_server --model-path /model --tp-size 1"
	launch, err := ResolveLaunch(input(script))
	if err != nil {
		t.Fatalf("ResolveLaunch() = %v, want the stored script launched", err)
	}
	if len(launch.Argv) != 2 || launch.Argv[1] != "set -a\n"+script {
		t.Fatalf("argv = %q", launch.Argv)
	}
	_, err = ResolveLaunch(input(`["python","-m","sglang.launch_server","--model-path","/model","--tp-size","1"]`))
	if err == nil || !strings.Contains(err.Error(), "requires 1 GPUs, but placement assigned 2") {
		t.Fatalf("ResolveLaunch() = %v, want the argv mismatch", err)
	}
}

// Only the one command that starts the engine is read from a manual script
// (GWKS-415): text that is not the engine's own command line can neither
// refuse a deployment nor hide a mismatch the engine's command states.
func TestManualScriptJudgesOnlyTheEngineCommand(t *testing.T) {
	const (
		vllmMismatch   = "vllm topology: tensor parallel 1 × pipeline parallel 1 requires 1 GPUs, but placement assigned 2"
		sglangMismatch = "sglang topology: tp-size 1 × pp-size 1 requires 1 GPUs, but placement assigned 2"
	)
	two := Params{ModelPath: "/model", AssignedGPUs: 2, TensorParallel: 2}
	for _, test := range []struct {
		name    string
		engine  Engine
		params  Params
		command string
		wantErr string
	}{
		// Left to the script: nothing here is the engine's single command line.
		{name: "one engine per card in the background", engine: vLLMEngine{}, params: two,
			command: "CUDA_VISIBLE_DEVICES=0 vllm serve /model --tensor-parallel-size 1 --port 8001 &\nCUDA_VISIBLE_DEVICES=1 vllm serve /model --tensor-parallel-size 1 --port 8002 &\nwait"},
		{name: "another program's flag of the same name", engine: sglangEngine{}, params: two,
			command: "ray start --head --num-gpus 1\nexec python -m sglang.launch_server --model-path /model --tp-size 2"},
		{name: "a custom server's own tp flag", engine: vLLMEngine{}, params: two, command: "exec python my_server.py --tp 1 --port 8000"},
		{name: "unquoted prose in an echo", engine: sglangEngine{}, params: two, command: "echo use --tp-size 1 for debugging\nexec ./start.sh"},
		{name: "heredoc body naming the flag", engine: sglangEngine{}, params: two,
			command: "cat <<EOF > /tmp/notes\nrun with --tp-size 1 on small boxes\nEOF\nexec python -m sglang.launch_server --model-path /model --port 30000"},
		{name: "tab-stripped quoted heredoc", engine: sglangEngine{}, params: two,
			command: "cat <<-'EOF'\n\tpython -m sglang.launch_server --tp-size 1\n\tEOF\nexec python -m sglang.launch_server --model-path /model --tp-size 2"},
		{name: "trailing comment naming another degree", engine: sglangEngine{}, params: two,
			command: "exec python -m sglang.launch_server --model-path /model --tp-size 2 # was --tp-size 1"},
		{name: "fallback to a second engine command", engine: sglangEngine{}, params: two,
			command: "python -m sglang.launch_server --model-path /model --tp-size 2 || python -m sglang.launch_server --model-path /model --tp-size 2"},
		{name: "wrapper call before a consistent engine command", engine: sglangEngine{}, params: two,
			command: "./prepare.sh --tp 1\nexec python -m sglang.launch_server --model-path /model --tp 2"},
		{name: "value from a command substitution", engine: sglangEngine{}, params: two,
			command: "exec python -m sglang.launch_server --model-path /model --tp-size `nproc`"},
		{name: "unterminated heredoc", engine: sglangEngine{}, params: two,
			command: "cat <<EOF\nexec python -m sglang.launch_server --model-path /model --tp-size 1"},

		// Judged: the engine's command states its world, however it is laid out.
		{name: "continuation lines without indentation", engine: sglangEngine{}, params: two,
			command: "python -m sglang.launch_server \\\n--model-path /model \\\n--tp-size 1 \\\n--port 30000", wantErr: sglangMismatch},
		{name: "continuation lines with indentation", engine: sglangEngine{}, params: two,
			command: "python -m sglang.launch_server \\\n  --model-path /model \\\n  --tp-size 1 \\\n  --port 30000", wantErr: sglangMismatch},
		{name: "value followed by a semicolon", engine: sglangEngine{}, params: two,
			command: "python -m sglang.launch_server --model-path /model --tp-size 1; echo done", wantErr: sglangMismatch},
		{name: "value followed by a pipe", engine: vLLMEngine{}, params: two,
			command: "vllm serve /model --tensor-parallel-size 1|tee /tmp/log", wantErr: vllmMismatch},
		{name: "value closing a subshell", engine: vLLMEngine{}, params: two,
			command: "(vllm serve /model --tensor-parallel-size 1)", wantErr: vllmMismatch},
		{name: "engine command in a conditional body", engine: vLLMEngine{}, params: two,
			command: "if [ -d /model ]; then exec vllm serve /model --tensor-parallel-size 1; fi", wantErr: vllmMismatch},
		{name: "engine command after assignments and by path", engine: vLLMEngine{}, params: two,
			command: "HF_HUB_OFFLINE=1 exec /opt/venv/bin/vllm serve /model --tensor-parallel-size=1", wantErr: vllmMismatch},
		{name: "python module entrypoint", engine: vLLMEngine{}, params: two,
			command: "exec python3 -m vllm.entrypoints.openai.api_server --model /model --tensor-parallel-size 1", wantErr: vllmMismatch},
		{name: "quoted value", engine: sglangEngine{}, params: two,
			command: "exec python -m sglang.launch_server --model-path /model --tp-size '1'", wantErr: sglangMismatch},

		// Consistent worlds stay accepted.
		{name: "vllm data parallel over both cards", engine: vLLMEngine{}, params: two,
			command: "exec vllm serve /model --tensor-parallel-size 1 --data-parallel-size 2"},
		{name: "sglang data parallel over both cards", engine: sglangEngine{}, params: two,
			command: "exec python -m sglang.launch_server --model-path /model --tp-size 1 --dp-size 2"},
		{name: "two nodes of two cards", engine: sglangEngine{}, params: Params{ModelPath: "/model", AssignedGPUs: 2, TensorParallel: 4, NNodes: 2},
			command: "exec python -m sglang.launch_server --model-path /model --tp-size 4 --nnodes 2"},
		{name: "one card", engine: vLLMEngine{}, params: Params{ModelPath: "/model", AssignedGPUs: 1, TensorParallel: 1},
			command: "exec vllm serve /model --tensor-parallel-size 1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ResolveLaunch(ResolveLaunchInput{
				Engine: test.engine, Params: test.params,
				ContainerOverrides: ContainerOverrides{CommandMode: "manual", CmdDisplay: test.command},
			})
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("ResolveLaunch() = %v, want accepted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ResolveLaunch() = %v, want error containing %q", err, test.wantErr)
			}
		})
	}
}

func TestShellSimpleCommands(t *testing.T) {
	commands, ok := shellSimpleCommands("A=1 run \"a b\" 'c d' e\\ f # note\nx && y || z; w <<EOF | v\nbody --flag\nEOF\nlast \\\nline")
	want := [][]string{{"A=1", "run", "a b", "c d", "e f"}, {"x"}, {"y"}, {"z"}, {"w"}, {"v"}, {"last", "line"}}
	if !ok || len(commands) != len(want) {
		t.Fatalf("commands = %q ok=%v", commands, ok)
	}
	for i := range want {
		if strings.Join(commands[i], "\x00") != strings.Join(want[i], "\x00") {
			t.Fatalf("command %d = %q, want %q", i, commands[i], want[i])
		}
	}
	for _, unreadable := range []string{"echo 'open", "echo \"open", "cat <<EOF\nbody", "trailing \\"} {
		if _, ok := shellSimpleCommands(unreadable); ok {
			t.Fatalf("%q was read", unreadable)
		}
	}
}
