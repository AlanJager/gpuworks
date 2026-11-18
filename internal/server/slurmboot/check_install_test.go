package slurmboot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/server/installscript"
)

// agentStub is the agent binary the installer downloads: configure records
// what the real one would (the enrollment token from stdin, the node's Slurm
// name, no node token), run does nothing.
const agentStub = `#!/bin/sh
if [ "$1" = configure ]; then
  shift
  state=""; server=""; node=""
  while [ $# -gt 0 ]; do
    case "$1" in --state) state="$2"; shift 2 ;; --server) server="$2"; shift 2 ;; --slurm-node) node="$2"; shift 2 ;; *) shift ;; esac
  done
  read -r token
  if [ -n "$node" ]; then
    printf 'SERVER=%s\nTOKEN=\nSLURM_ENROLL_TOKEN=%s\nSLURM_NODE=%s\n' "$server" "$token" "$node" > "$state/gpuworks-agent.conf"
  else
    printf 'SERVER=%s\nTOKEN=%s\n' "$server" "$token" > "$state/gpuworks-agent.conf"
  fi
fi
exit 0
`

// realInstaller serves the server's own install.sh and an agent binary stub
// instead of the stand-in installer, so what the health check hands the
// installer is read by the script that really reads it. The unit the
// installer writes goes to a file, not /etc.
func (w *world) realInstaller() {
	w.t.Helper()
	w.write(filepath.Join(w.state, "real-installer"), installscript.Script)
	w.write(filepath.Join(w.state, "agent-binary"), agentStub)
	w.remove("tee")
	w.writeExec(filepath.Join(w.bin, "tee"), "#!/bin/sh\ncat > \"$GWKS_FAKE_STATE/unit\"\n")
}

// The health check and install.sh disagreed once about the names the
// enrollment travels under, and every install failed with "--token is
// required" while the check logged a success. Here the script that is served
// as /install.sh runs, and the agent's configuration ends up with the
// enrollment token and the Slurm node.
func TestCheckEnrollsThroughTheRealInstaller(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.gpuNode()
		w.knownNodes("cn000")
		w.realInstaller()
		if r := w.runCheck(); r.code != 0 {
			t.Fatalf("exit %d", r.code)
		}
		confPath := agentconf.Path(w.p.InstallDir)
		w.waitFor("the agent's configuration", func() bool { return w.exists(confPath) })
		conf, err := agentconf.Load(w.p.InstallDir)
		if err != nil {
			t.Fatal(err)
		}
		if conf.SlurmEnrollToken != w.p.Token || conf.SlurmNode != "cn000" || conf.Server != testServer || conf.Token != "" {
			t.Fatalf("configuration = %+v", conf)
		}
		// The service is up once the installer is done. (A host without systemd
		// starts the stub agent itself, which the fake systemctl does not know
		// about.)
		if _, err := os.Stat("/run/systemd/system"); err != nil {
			w.flag("agent-active", true)
		} else {
			w.waitFor("the service to start", func() bool { return w.exists(filepath.Join(w.state, "agent-active")) })
			unit := w.stateFile("unit")
			if !strings.Contains(unit, "ExecStart="+w.p.InstallDir+"/gpuworks-agent run --state "+w.p.InstallDir) || strings.Contains(unit, w.p.Token) {
				t.Fatalf("unit:\n%s", unit)
			}
		}
		w.waitFor("the worker to log", func() bool {
			return strings.Contains(w.read(filepath.Join(w.p.InstallDir, "slurm-check.log")), "the installer finished")
		})
		if log := w.read(filepath.Join(w.p.InstallDir, "slurm-check.log")); strings.Contains(log, w.p.Token) {
			t.Fatalf("the token is in the log:\n%s", log)
		}
		before := w.stateFile("curl")
		w.runCheck()
		if w.stateFile("curl") != before {
			t.Fatalf("a node whose agent runs asked the server again:\n%s", w.stateFile("curl"))
		}
	})
}

// A non-root agent: the same installer, told --run-as, writes the unit with
// User= and hands the directories over.
func TestCheckEnrollsANonRootAgentThroughTheRealInstaller(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		if _, err := os.Stat("/run/systemd/system"); err != nil {
			t.Skip("--run-as needs the systemd branch of the installer, which a host without systemd does not take")
		}
		w.gpuNode()
		w.knownNodes("cn000")
		w.p.RunAs = "gpuworks"
		w.realInstaller()
		// The installer asks the system whether the account exists.
		w.remove("id")
		w.writeExec(filepath.Join(w.bin, "id"), "#!/bin/sh\ncase \"$1\" in -u) if [ -n \"$2\" ]; then [ \"$2\" = gpuworks ] || exit 1; echo 1001; else echo 0; fi ;; -g) echo 0 ;; esac\n")
		w.runCheck()
		w.waitFor("the service to start", func() bool { return w.exists(filepath.Join(w.state, "agent-active")) })
		conf, err := agentconf.Load(w.p.InstallDir)
		if err != nil || conf.SlurmEnrollToken != w.p.Token || conf.SlurmNode != "cn000" {
			t.Fatalf("configuration = %+v (%v)", conf, err)
		}
		if unit := w.stateFile("unit"); !strings.Contains(unit, "User=gpuworks") {
			t.Fatalf("unit:\n%s", unit)
		}
		if !strings.Contains(w.stateFile("chown"), "-R gpuworks "+w.p.InstallDir) {
			t.Fatalf("chown calls: %q", w.stateFile("chown"))
		}
	})
}
