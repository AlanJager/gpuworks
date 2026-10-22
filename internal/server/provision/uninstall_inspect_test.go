package provision

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Inspect the real shell script against proc files, without depending on the
// developer's running agents, process count, or installed service definitions.
func uninstallInspectFixtureScript(script, root string) string {
	script = strings.ReplaceAll(script, "/proc/", root+"/proc/")
	return strings.ReplaceAll(script, "'/Library/LaunchDaemons/", "'"+root+"/Library/LaunchDaemons/")
}

func writeUninstallProc(t *testing.T, root string, pid int, comm string, argv ...string) {
	t.Helper()
	dir := filepath.Join(root, "proc", fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmdline := ""
	if len(argv) > 0 {
		cmdline = strings.Join(argv, "\x00") + "\x00"
	}
	for name, data := range map[string]string{"comm": comm + "\n", "cmdline": cmdline} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func runUninstallInspectFixture(t *testing.T, root, server string, token ...string) (string, string, error) {
	t.Helper()
	toolsDir := filepath.Join(root, "tools")
	if err := os.MkdirAll(toolsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(toolsDir, "systemctl"), "exit 1")
	writeExecutable(t, filepath.Join(toolsDir, "uname"), "echo Linux")
	for _, tool := range []string{"tr", "sed", "awk", "grep"} {
		binary, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		writeExecutable(t, filepath.Join(toolsDir, tool), "printf '%s\\n' "+shellQuote(tool)+" >> "+shellQuote(filepath.Join(root, "parser-calls"))+"\nexec "+shellQuote(binary)+" \"$@\"")
	}
	cmd := exec.CommandContext(t.Context(), "sh", "-s", "--", root)
	cmd.Env = append(os.Environ(), "HOME="+root, "PATH="+toolsDir+":"+os.Getenv("PATH"))
	cmd.Stdin = strings.NewReader(uninstallInspectFixtureScript(agentUninstallInspectCommand(server, strings.Join(token, "")), root))
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func TestUninstallInspectProcessScanUsesBoundedParserProcesses(t *testing.T) {
	root := t.TempDir()
	for pid := 100; pid < 164; pid++ {
		writeUninstallProc(t, root, pid, "unrelated", "/bin/unrelated", "--token", "secret-fixture-token")
	}
	out, stderr, err := runUninstallInspectFixture(t, root, "http://control")
	if err != nil || out != "absent\n" || stderr != "" {
		t.Fatalf("inspect: stdout=%q stderr=%q err=%v", out, stderr, err)
	}
	calls, err := os.ReadFile(filepath.Join(root, "parser-calls"))
	if err != nil {
		t.Fatal(err)
	}
	count := strings.Count(string(calls), "\n")
	t.Logf("64 unrelated proc entries required %d parser commands", count)
	if count > 2 {
		t.Fatalf("process scan spawned %d parser commands for 64 unrelated processes; want at most 2", count)
	}
}

func TestUninstallInspectProcessOwnership(t *testing.T) {
	const server = "http://control"
	const binary = "/custom path/gpuworks-agent"
	const state = "/custom state"
	for _, scenario := range []string{"spaces", "equals", "renamed-comm", "lookalike", "argument-lookalike", "foreign", "not-run", "duplicate", "multiple", "missing-state", "disappeared", "read-failure", "empty"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			argv := []string{binary, "run", "--server", server + "/", "--token", "secret-fixture-token", "--state", state}
			comm := "gpuworks-agent"
			want := binary + "\n" + state + "\n" + server + "/\n"
			wantErr := ""
			switch scenario {
			case "equals":
				argv = []string{binary, "run", "--server=" + server + "/", "--state=" + state, "--token=secret-fixture-token"}
			case "renamed-comm":
				comm = "custom-name"
			case "lookalike":
				argv[0] = "/bin/not-gpuworks-agent"
				want = "absent\n"
			case "argument-lookalike":
				argv[0] = "/bin/unrelated"
				argv = append(argv, "--description", "/custom path/gpuworks-agent")
				want = "absent\n"
			case "foreign":
				argv[3] = "http://other"
				want = "absent\n"
			case "not-run":
				argv[1] = "doctor"
				want = "absent\n"
			case "duplicate":
				writeUninstallProc(t, root, 101, comm, argv...)
			case "multiple":
				other := append([]string(nil), argv...)
				other[7] = "/other state"
				writeUninstallProc(t, root, 101, comm, other...)
				wantErr = "multiple agent installations match this server"
			case "missing-state":
				argv = argv[:6]
				wantErr = "cannot determine the running agent state directory"
			case "empty":
				argv = nil
				want = "absent\n"
			}
			writeUninstallProc(t, root, 100, comm, argv...)
			if scenario == "disappeared" || scenario == "read-failure" {
				file := filepath.Join(root, "proc", "100", "cmdline")
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if scenario == "read-failure" {
					if err := os.Mkdir(file, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				want = "absent\n"
			}
			out, stderr, err := runUninstallInspectFixture(t, root, server)
			if strings.Contains(out+stderr, "secret-fixture-token") {
				t.Fatal("inspect exposed node token")
			}
			if wantErr != "" {
				if err == nil || !strings.Contains(stderr, wantErr) || out != "" {
					t.Fatalf("want %q: stdout=%q stderr=%q err=%v", wantErr, out, stderr, err)
				}
			} else if err != nil || out != want {
				t.Fatalf("want %q: stdout=%q stderr=%q err=%v", want, out, stderr, err)
			}
		})
	}
}

func TestUninstallInspectScanErrors(t *testing.T) {
	for _, status := range []int{0, 1, 2, 126, 127} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			root := t.TempDir()
			overrides := filepath.Join(root, "overrides")
			if err := os.Mkdir(overrides, 0o700); err != nil {
				t.Fatal(err)
			}
			command := fmt.Sprintf("exit %d", status)
			want := "absent\n"
			if status == 0 {
				// grep can emit a candidate that disappears before argv is read.
				command = "printf '%s\\n' " + shellQuote(filepath.Join(root, "proc", "100", "cmdline"))
			}
			if status == 2 {
				// A disappearing/unreadable proc entry must not discard another
				// process that grep successfully found in the same scan.
				writeUninstallProc(t, root, 100, "renamed", "/bin/gpuworks-agent", "run", "--server", "http://control", "--state", "/state")
				command = "printf '%s\\n' " + shellQuote(filepath.Join(root, "proc", "100", "cmdline")) + "\n" + command
				want = "/bin/gpuworks-agent\n/state\nhttp://control\n"
			}
			writeExecutable(t, filepath.Join(overrides, "grep"), command)
			t.Setenv("PATH", overrides+":"+os.Getenv("PATH"))
			out, stderr, err := runUninstallInspectFixture(t, root, "http://control")
			if status <= 2 {
				if err != nil || out != want {
					t.Fatalf("scan exit %d: stdout=%q stderr=%q err=%v", status, out, stderr, err)
				}
			} else if err == nil || out != "" || !strings.Contains(stderr, "cannot inspect running agent processes") {
				t.Fatalf("tool failure reported as successful inspection: stdout=%q stderr=%q err=%v", out, stderr, err)
			}
		})
	}
}

// Two agents of one server on a host (one per node, each from its own
// directory): the node's token picks its own, where the server alone cannot.
func TestUninstallInspectPicksTheNodesAgentByToken(t *testing.T) {
	root := t.TempDir()
	writeUninstallProc(t, root, 100, "gpuworks-agent", "/opt/gpuworks/gpuworks-agent", "run", "--server", "http://control", "--token", "token-a", "--state", "/opt/gpuworks")
	writeUninstallProc(t, root, 101, "gpuworks-agent", "/data/b/gpuworks-agent", "run", "--server", "http://control", "--token", "token-b", "--state", "/data/b")
	if out, _, err := runUninstallInspectFixture(t, root, "http://control"); err == nil {
		t.Fatalf("without a token the server matches both agents, got %q", out)
	}
	out, stderr, err := runUninstallInspectFixture(t, root, "http://control", "token-b")
	if err != nil || out != "/data/b/gpuworks-agent\n/data/b\nhttp://control\n" {
		t.Fatalf("inspect node B: stdout=%q stderr=%q err=%v", out, stderr, err)
	}
	if out, _, err := runUninstallInspectFixture(t, root, "http://control", "token-c"); err != nil || out != "absent\n" {
		t.Fatalf("a deleted node's token matches nothing: %q %v", out, err)
	}
}

// A node the server dials has no server: its agent, configured by the file
// in its state directory and started as `run --state`, is found by the token
// in that file, beside another agent on the host (two agents on one Vast
// instance, 2026-10-19).
func TestUninstallInspectFindsAServerDialAgentByItsConfiguration(t *testing.T) {
	root := t.TempDir()
	for _, a := range []struct{ dir, token string }{{"opt/gpuworks", "token-a"}, {"opt/gpuworks-offline", "token-b"}} {
		state := filepath.Join(root, a.dir)
		if err := os.MkdirAll(state, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "gpuworks-agent.conf"), []byte("SERVER=\nTOKEN="+a.token+"\nCONNECT_MODE=server_dial\nLISTEN=0.0.0.0:10100\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeUninstallProc(t, root, 100, "gpuworks-agent", "/opt/gpuworks/gpuworks-agent", "run", "--state", filepath.Join(root, "opt/gpuworks"))
	writeUninstallProc(t, root, 101, "gpuworks-agent", "/opt/gpuworks-offline/gpuworks-agent", "run", "--state", filepath.Join(root, "opt/gpuworks-offline"))
	out, stderr, err := runUninstallInspectFixture(t, root, "", "token-b")
	if err != nil || out != "/opt/gpuworks-offline/gpuworks-agent\n"+filepath.Join(root, "opt/gpuworks-offline")+"\n-\n" {
		t.Fatalf("inspect: stdout=%q stderr=%q err=%v", out, stderr, err)
	}
	if strings.Contains(out+stderr, "token-") {
		t.Fatal("inspect exposed a node token")
	}
	installation, err := parseAgentUninstallInstallation(out, "")
	if err != nil || installation.binary != "/opt/gpuworks-offline/gpuworks-agent" {
		t.Fatalf("parse = %+v, %v", installation, err)
	}
	if _, err := parseAgentUninstallInstallation(out, "http://other:8800"); err == nil {
		t.Fatal("a server_dial agent was attributed to a server")
	}
}
