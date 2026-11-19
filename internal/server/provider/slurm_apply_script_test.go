package provider

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

const (
	applyTestURL = "https://gw.example:8800/slurm/setup.sh?token=tok-abc.123_X"
	gresReason   = "gres/gpu count reported lower than configured (0 < 1)"
)

// manager is a Slurm manager node in miniature: a gres.conf path, a scontrol
// that knows some nodes and prints them as `scontrol -o show node` does (one
// line each, E-HPC's dynamic nodes among them), a curl that serves a stand-in
// setup script, and the real flock.
type manager struct {
	t    *testing.T
	dir  string
	gres string
	env  []string
}

// newManager makes a manager node whose Slurm lists nodes; a node is its name,
// or "name=Gres" for the Gres= it is configured with (gpu:1 when none is
// given, "(null)" is a node without any).
func newManager(t *testing.T, nodes ...string) *manager {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	scontrol := `#!/bin/sh
echo "$*" >> "$FAKE_DIR/scontrol.log"
case "$*" in
  reconfigure) exit 0 ;;
  "update "*) exit "${FAKE_UPDATE_RC:-0}" ;;
  "show hostnames "*)
    # gpu[000-001] -> gpu000 gpu001 (only the shape the tests use)
    case "$3" in
      gpu\[000-001\]) printf 'gpu000\ngpu001\n' ;;
      *) printf '%s\n' "$3" ;;
    esac
    ;;
  "-o show node")
    [ ! -e "$FAKE_DIR/scontrol-fail" ] || { echo "slurm_load_node error: Unable to contact slurm controller (connect failure)" >&2; exit 1; }
    while IFS='=' read -r name gres; do
      [ -n "$name" ] || continue
      reason=$(sed -n "s/^$name=//p" "$FAKE_DIR/reasons" 2>/dev/null)
      [ -z "$reason" ] || reason=" Reason=$reason [root@2026-11-16T23:41:26]"
      echo "NodeName=$name Arch=x86_64  CoresPerSocket=8  CPUAlloc=0 CPUEfctv=16 CPUTot=16 CPULoad=0.74 AvailableFeatures=gpuFeature ActiveFeatures=gpuFeature Gres=${gres:-gpu:1} NodeAddr=172.30.5.40 NodeHostName=$name Version=22.05.8 OS=Linux 5.15.0-177-generic #187-Ubuntu SMP Sat Apr 11 22:54:33 UTC 2026  RealMemory=61898 AllocMem=0 FreeMem=58525 Sockets=1 Boards=1 State=IDLE+DRAIN+DYNAMIC_NORM+INVALID_REG ThreadsPerCore=2 TmpDisk=0 Weight=1 Owner=N/A MCS_label=N/A Partitions=gpu  BootTime=2026-11-16T23:39:16 SlurmdStartTime=2026-11-16T23:41:26 LastBusyTime=2026-11-16T23:41:26 CfgTRES=cpu=16,mem=61898M,billing=16 AllocTRES= CapWatts=n/a CurrentWatts=0 AveWatts=0 ExtSensorsJoules=n/s ExtSensorsWatts=0 ExtSensorsTemp=n/s$reason"
    done < "$FAKE_DIR/nodes"
    ;;
esac
`
	curl := `#!/bin/sh
echo "$*" >> "$FAKE_DIR/curl.args"
cat > "$FAKE_DIR/curl.stdin"
if [ -e "$FAKE_DIR/curl-fail" ]; then
  rc=$(cat "$FAKE_DIR/curl-fail"); rc=${rc:-7}
  case "$rc" in 28) echo "curl: (28) Connection timed out after 60001 milliseconds" >&2 ;; 6) echo "curl: (6) Could not resolve host: gw.example" >&2 ;; *) echo "curl: (7) Failed to connect to gw.example port 8800" >&2 ;; esac
  exit "$rc"
fi
out=''
while [ "$#" -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done
cp "$FAKE_DIR/setup.sh" "$out"
`
	for name, body := range map[string]string{"scontrol": scontrol, "curl": curl} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range []string{"awk", "grep", "sed", "tail", "mktemp", "rm", "cp", "cat", "chmod", "printf", "flock", "sh", "bash", "dash", "head", "tr", "sort"} {
		if p, err := exec.LookPath(tool); err == nil {
			_ = os.Symlink(p, filepath.Join(bin, tool))
		}
	}
	m := &manager{t: t, dir: dir, gres: filepath.Join(dir, "gres.conf")}
	m.env = []string{"PATH=" + bin, "FAKE_DIR=" + dir, "TMPDIR=" + dir, "HOME=" + dir, "LC_ALL=C"}
	m.file("nodes", strings.Join(nodes, "\n")+"\n")
	m.setup(0, "GWKS_SLURM step=precheck status=ok cluster=demo", "GWKS_SLURM step=check_script status=ok path=/opt/gpuworks-slurm/check.sh",
		"GWKS_SLURM step=shared status=yes fstype=nfs4", "GWKS_SLURM step=distribute_summary ok=0 failed=none skipped=shared",
		"GWKS_SLURM step=slurm_conf status=ok path=/etc/slurm/slurm.conf backup=none chained=none interval=300",
		"GWKS_SLURM step=configless value=yes", "GWKS_SLURM step=reconfigure status=ok", "GWKS_SLURM step=done status=ok")
	return m
}

func (m *manager) file(name, content string) {
	m.t.Helper()
	if err := os.WriteFile(filepath.Join(m.dir, name), []byte(content), 0o644); err != nil {
		m.t.Fatal(err)
	}
}

func (m *manager) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(m.dir, name))
	return string(b)
}

// setup makes the stand-in setup script print lines and exit with rc.
func (m *manager) setup(rc int, lines ...string) {
	m.t.Helper()
	body := "#!/bin/sh\necho \"args=$*\" >> \"$FAKE_DIR/setup.args\"\n"
	for _, l := range lines {
		body += "echo '" + l + "'\n"
	}
	body += "exit " + string(rune('0'+rc)) + "\n"
	m.file("setup.sh", body)
}

func (m *manager) conf() string {
	b, err := os.ReadFile(m.gres)
	if errors.Is(err, os.ErrNotExist) {
		return "<missing>"
	}
	if err != nil {
		m.t.Fatal(err)
	}
	return string(b)
}

// run runs the script in a real shell, as the cloud's command channel does,
// with gres.conf redirected into the test directory.
func (m *manager) run(shell string, req SlurmApplyRequest) string {
	m.t.Helper()
	script := strings.Replace(SlurmManagerScript(req), "'"+SlurmGRESConfPath+"'", "'"+m.gres+"'", 1)
	cmd := exec.Command(shell, "-c", script)
	cmd.Env = m.env
	out, _ := cmd.CombinedOutput()
	return string(out)
}

func eachShell(t *testing.T, fn func(t *testing.T, shell string)) {
	t.Helper()
	ran := 0
	for _, shell := range []string{"sh", "bash", "dash"} {
		if _, err := exec.LookPath(shell); err != nil {
			continue
		}
		ran++
		t.Run(shell, func(t *testing.T) { fn(t, shell) })
	}
	if ran == 0 {
		t.Fatal("no shell to test with")
	}
}

func setupReq(expectGPU ...string) SlurmApplyRequest {
	return SlurmApplyRequest{Mode: SlurmModeSetup, SetupURL: applyTestURL, ExpectGPU: expectGPU}
}

func TestSlurmApplyCreatesGresConfForNodesWithoutALine(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		// no cloud-side list at all: the nodes and their cards come from Slurm
		m := newManager(t, "gpu000", "gpu001=gpu:4", "gpu002=gpu:8")
		out := m.run(shell, setupReq())
		res, err := ParseSlurmApply(SlurmModeSetup, out, "")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		want := "NodeName=gpu000 Name=gpu File=/dev/nvidia0\nNodeName=gpu001 Name=gpu File=/dev/nvidia[0-3]\nNodeName=gpu002 Name=gpu File=/dev/nvidia[0-7]\n"
		if got := m.conf(); got != want {
			t.Fatalf("gres.conf = %q, want %q", got, want)
		}
		if info, err := os.Stat(m.gres); err != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("gres.conf mode = %v, %v; want 0644", info, err)
		}
		if strings.Join(res.Added, ",") != "gpu000,gpu001,gpu002" || len(res.Present) != 0 || len(res.NoGPUGres) != 0 {
			t.Fatalf("result = %+v", res)
		}
		if res.Partial() {
			t.Fatalf("a clean run is partial: %+v", res)
		}
		// gres.conf is written before slurm.conf and reconfigure (setup does that)
		if calls := m.read("scontrol.log"); strings.Contains(calls, "reconfigure") {
			t.Fatalf("the manager script reconfigured on its own; setup does: %q", calls)
		}
	})
}

// Whatever gres.conf already says about a node stays: an identical or a
// different line, a NodeName expression that names it, a line for every node.
func TestSlurmApplyNeverTouchesNodesThatAlreadyHaveALine(t *testing.T) {
	cases := []struct {
		name, conf string
	}{
		{"own line", "NodeName=gpu000 Name=gpu File=/dev/nvidia[0-3]\n"},
		{"own line, different shape", "NodeName=gpu000 Name=gpu Type=a100 File=/dev/nvidia0\n"},
		{"range expression", "NodeName=gpu[000-001] Name=gpu File=/dev/nvidia0\n"},
		{"line without a node name", "Name=gpu File=/dev/nvidia0\n"},
		{"autodetect", "AutoDetect=nvml\n"},
		{"after a comment, no final newline", "# c\n\nNodeName=gpu000 Name=gpu File=/dev/nvidia0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, shell string) {
				m := newManager(t, "gpu000")
				if err := os.WriteFile(m.gres, []byte(c.conf), 0o600); err != nil {
					t.Fatal(err)
				}
				out := m.run(shell, setupReq())
				res, err := ParseSlurmApply(SlurmModeSetup, out, "")
				if err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				if m.conf() != c.conf {
					t.Fatalf("gres.conf changed: %q -> %q", c.conf, m.conf())
				}
				if len(res.Added) != 0 || strings.Join(res.Present, ",") != "gpu000" {
					t.Fatalf("result = %+v", res)
				}
			})
		})
	}
}

func TestSlurmApplyKeepsOtherLinesAndEndsAnUnterminatedOne(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		m := newManager(t, "gpu000", "gpu0001")
		if err := os.WriteFile(m.gres, []byte("# written by E-HPC\nNodeName=gpu0001 Name=gpu File=/dev/nvidia[0-1]"), 0o600); err != nil {
			t.Fatal(err)
		}
		out := m.run(shell, setupReq())
		if _, err := ParseSlurmApply(SlurmModeSetup, out, ""); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		// gpu0001's line is not gpu000's: the node is matched by its exact name
		want := "# written by E-HPC\nNodeName=gpu0001 Name=gpu File=/dev/nvidia[0-1]\nNodeName=gpu000 Name=gpu File=/dev/nvidia0\n"
		if got := m.conf(); got != want {
			t.Fatalf("gres.conf = %q, want %q", got, want)
		}
	})
}

// What Slurm lists is what gets a line, whatever the cloud's list says: nodes
// the cloud never named (a cluster applied before any sync, names that do not
// fit "<cluster>_<host>") get theirs, and a name only the cloud knows gets
// nothing and is no warning.
func TestSlurmApplyTakesNodesAndCardsFromSlurmNotFromTheCloudsList(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		m := newManager(t, "gpu000", "someone-else.dc1=gpu:2")
		out := m.run(shell, setupReq("gpu000", "only-the-cloud-knows-me"))
		res, err := ParseSlurmApply(SlurmModeSetup, out, "")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		want := "NodeName=gpu000 Name=gpu File=/dev/nvidia0\nNodeName=someone-else.dc1 Name=gpu File=/dev/nvidia[0-1]\n"
		if got := m.conf(); got != want || strings.Join(res.Added, ",") != "gpu000,someone-else.dc1" {
			t.Fatalf("gres.conf %q, result %+v", got, res)
		}
		if res.Partial() || len(res.NoGPUGres) != 0 {
			t.Fatalf("a name Slurm does not list is flagged: %+v", res)
		}
	})
}

// Slurm writes the configured Gres= in several shapes; each gpu entry counts
// as many cards as it says, every other resource and "none" count for none.
func TestSlurmApplyReadsTheCardsOfEveryGresShape(t *testing.T) {
	cases := []struct {
		gres, files string // files "" = no line
	}{
		{"gpu:1", "/dev/nvidia0"},
		{"gpu:2", "/dev/nvidia[0-1]"},
		{"gpu:2(S:0)", "/dev/nvidia[0-1]"},
		{"gpu:4(S:0-1)", "/dev/nvidia[0-3]"},
		{"gpu:2(S:0,1)", "/dev/nvidia[0-1]"},
		{"gpu:a100:8(S:0-1)", "/dev/nvidia[0-7]"},
		{"gpu:a100:1", "/dev/nvidia0"},
		{"gpu", "/dev/nvidia0"},
		{"gpu:a100", "/dev/nvidia0"},
		{"gpu:a100:2,gpu:v100:1", "/dev/nvidia[0-2]"},
		{"gpu:2(S:0),mps:200", "/dev/nvidia[0-1]"},
		{"shard:8,gpu:1", "/dev/nvidia0"},
		{"mps:200", ""},
		{"shard:8", ""},
		{"gpus:2", ""},
		{"(null)", ""},
		{"gpu:0", ""},
	}
	for _, c := range cases {
		t.Run(c.gres, func(t *testing.T) {
			eachShell(t, func(t *testing.T, shell string) {
				m := newManager(t, "n1="+c.gres)
				out := m.run(shell, setupReq())
				if _, err := ParseSlurmApply(SlurmModeSetup, out, ""); err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				want := ""
				if c.files != "" {
					want = "NodeName=n1 Name=gpu File=" + c.files + "\n"
				}
				if got := m.conf(); got != want && !(want == "" && got == "<missing>") {
					t.Fatalf("gres.conf = %q, want %q", got, want)
				}
			})
		})
	}
}

// A realistic cluster: E-HPC's dynamic GPU nodes (still INVALID_REG, no gres
// line yet), a CPU node that has no Gres at all, a node whose line is
// already there. Setup and check say the same, and only setup writes.
func TestSlurmApplyOnAMixedClusterCheckAndSetupAgree(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		prep := func() *manager {
			m := newManager(t, "cpu000=(null)", "gpu000", "gpu001=gpu:2(S:0)", "gpu002=gpu:a100:4(S:0-1)", "gpu003")
			m.file("reasons", "gpu000="+gresReason+"\n")
			if err := os.WriteFile(m.gres, []byte("# E-HPC\nNodeName=gpu003 Name=gpu File=/dev/nvidia0\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			m.setup(0, checkPlanLines...)
			return m
		}
		wantLines := []SlurmGresLine{{"gpu000", "/dev/nvidia0"}, {"gpu001", "/dev/nvidia[0-1]"}, {"gpu002", "/dev/nvidia[0-3]"}}

		m := prep()
		out := m.run(shell, checkReq())
		res, err := ParseSlurmApply(SlurmModeCheck, out, "")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if got := m.conf(); got != "# E-HPC\nNodeName=gpu003 Name=gpu File=/dev/nvidia0\n" {
			t.Fatalf("a check wrote gres.conf: %q", got)
		}
		if strings.Join(res.Present, ",") != "gpu003" || len(res.Plan.GresAdd) != 3 || strings.Join(res.Plan.Resume, ",") != "gpu000" {
			t.Fatalf("check: %+v", res)
		}
		for i, l := range wantLines {
			if res.Plan.GresAdd[i] != l {
				t.Fatalf("check line %d = %+v, want %+v", i, res.Plan.GresAdd[i], l)
			}
		}

		m = prep()
		m.setup(0, "GWKS_SLURM step=precheck status=ok cluster=demo", "GWKS_SLURM step=check_script status=ok path=x", "GWKS_SLURM step=shared status=yes fstype=nfs4",
			"GWKS_SLURM step=distribute_summary ok=0 failed=none skipped=shared", "GWKS_SLURM step=slurm_conf status=ok path=x", "GWKS_SLURM step=reconfigure status=ok", "GWKS_SLURM step=done status=ok")
		out = m.run(shell, setupReq())
		got, err := ParseSlurmApply(SlurmModeSetup, out, "")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		want := "# E-HPC\nNodeName=gpu003 Name=gpu File=/dev/nvidia0\n"
		for _, l := range wantLines {
			want += SlurmGresLineText(l) + "\n"
		}
		if m.conf() != want || strings.Join(got.Added, ",") != "gpu000,gpu001,gpu002" || strings.Join(got.Present, ",") != "gpu003" || strings.Join(got.Resumed, ",") != "gpu000" {
			t.Fatalf("setup: gres.conf %q, result %+v", m.conf(), got)
		}
	})
}

// A Slurm that cannot be asked adds nothing and does not take the run down:
// the setup script's own precheck is what says slurmctld is unreachable.
func TestSlurmApplyWithASlurmThatDoesNotAnswerAddsNothing(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		m := newManager(t, "gpu000")
		m.file("scontrol-fail", "")
		out := m.run(shell, setupReq("gpu000"))
		res, err := ParseSlurmApply(SlurmModeSetup, out, "")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if m.conf() != "<missing>" || len(res.Added) != 0 || len(res.NoGPUGres) != 0 {
			t.Fatalf("gres.conf %q, result %+v", m.conf(), res)
		}
	})
}

func TestSlurmApplyFetchesTheSetupScriptWithoutTheTokenOnAnArgumentList(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		m := newManager(t, "gpu000")
		out := m.run(shell, setupReq())
		if _, err := ParseSlurmApply(SlurmModeSetup, out, ""); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if got := m.read("curl.stdin"); got != "url = \""+applyTestURL+"\"\n" {
			t.Fatalf("curl config on stdin = %q", got)
		}
		if args := m.read("curl.args"); strings.Contains(args, "tok-abc") || !strings.Contains(args, "-K -") {
			t.Fatalf("curl arguments = %q", args)
		}
		if got := m.read("setup.args"); got != "args=\n" {
			t.Fatalf("setup arguments = %q", got)
		}
	})
}

func TestSlurmApplyModesRunSetupWithTheirArgument(t *testing.T) {
	for mode, arg := range map[string]string{SlurmModeUninstallAgents: "--uninstall-agents", SlurmModeRemoveCheck: "--remove"} {
		t.Run(mode, func(t *testing.T) {
			eachShell(t, func(t *testing.T, shell string) {
				m := newManager(t, "gpu000")
				if mode == SlurmModeUninstallAgents {
					m.setup(0, "GWKS_SLURM step=precheck status=ok cluster=demo", "GWKS_SLURM step=uninstall_agents status=ok interval=60", "GWKS_SLURM step=done status=ok")
				} else {
					m.setup(0, "GWKS_SLURM step=precheck status=ok cluster=demo", "GWKS_SLURM step=remove status=ok path=/etc/slurm/slurm.conf backup=none", "GWKS_SLURM step=done status=ok")
				}
				out := m.run(shell, SlurmApplyRequest{Mode: mode, SetupURL: applyTestURL})
				res, err := ParseSlurmApply(mode, out, "")
				if err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				if got := m.read("setup.args"); got != "args="+arg+"\n" {
					t.Fatalf("setup arguments = %q, want %s", got, arg)
				}
				if m.conf() != "<missing>" || m.read("scontrol.log") != "" {
					t.Fatalf("uninstalling touched gres.conf or Slurm: %q %q", m.conf(), m.read("scontrol.log"))
				}
				if mode == SlurmModeUninstallAgents && res.IntervalSeconds != 60 {
					t.Fatalf("result = %+v", res)
				}
			})
		})
	}
}

func TestSlurmApplyResumesOnlyNodesHeldForTheMissingGPUs(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		m := newManager(t, "gpu000", "gpu001", "gpu002")
		m.file("reasons", "gpu000="+gresReason+"\ngpu001=maintenance by alice\n")
		out := m.run(shell, setupReq())
		res, err := ParseSlurmApply(SlurmModeSetup, out, "")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if strings.Join(res.Resumed, ",") != "gpu000" {
			t.Fatalf("resumed = %v; an administrator's drain must stay", res.Resumed)
		}
		if calls := m.read("scontrol.log"); strings.Count(calls, "update ") != 1 || !strings.Contains(calls, "update nodename=gpu000 state=resume") {
			t.Fatalf("scontrol calls = %q", calls)
		}
		if res.Steps[5] != (SlurmApplyStep{SlurmApplyIDResume, proto.CheckPass}) {
			t.Fatalf("steps = %+v", res.Steps)
		}
	})
}

func TestSlurmApplyDoesNotResumeWhenSetupFailed(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		m := newManager(t, "gpu000")
		m.file("reasons", "gpu000="+gresReason+"\n")
		m.setup(1, "ERROR: cannot read /etc/slurm/slurm.conf.", "GWKS_SLURM step=precheck status=failed reason=conf_unreadable", "GWKS_SLURM step=done status=failed")
		out := m.run(shell, setupReq())
		_, err := ParseSlurmApply(SlurmModeSetup, out, "")
		var failed *SlurmApplyFailedError
		if !errors.As(err, &failed) || failed.Step != messages.SlurmApplyStepSetup || failed.Reason != "conf_unreadable" || !strings.Contains(failed.Output, "cannot read /etc/slurm/slurm.conf") {
			t.Fatalf("err = %v\n%s", err, out)
		}
		if strings.Contains(m.read("scontrol.log"), "update") {
			t.Fatal("resumed a node although setup failed")
		}
	})
}

func TestSlurmApplyResumeFailureIsAWarning(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		m := newManager(t, "gpu000")
		m.file("reasons", "gpu000="+gresReason+"\n")
		m.env = append(m.env, "FAKE_UPDATE_RC=1")
		out := m.run(shell, setupReq())
		res, err := ParseSlurmApply(SlurmModeSetup, out, "")
		if err != nil || strings.Join(res.ResumeFailed, ",") != "gpu000" || !res.Partial() {
			t.Fatalf("result = %+v, %v", res, err)
		}
	})
}

func TestSlurmApplyFetchFailureIsServerUnreachable(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		m := newManager(t, "gpu000")
		m.file("curl-fail", "")
		out := m.run(shell, setupReq())
		_, err := ParseSlurmApply(SlurmModeSetup, out, "")
		var failed *SlurmApplyFailedError
		if !errors.As(err, &failed) || failed.Step != messages.SlurmApplyStepSetup || failed.Reason != "server_unreachable" || !strings.Contains(failed.Output, "Failed to connect") {
			t.Fatalf("err = %v\n%s", err, out)
		}
		if failed.Kind != "connect" {
			t.Fatalf("kind = %q, want connect (curl exit 7)", failed.Kind)
		}
	})
}

// curl's exit status says why the manager node could not fetch the script:
// the kinds are the words of a host's reachability answer.
func TestSlurmApplyFetchFailureSaysWhyFromCurlsExitStatus(t *testing.T) {
	for rc, kind := range map[string]string{"28": "timeout", "6": "dns", "7": "connect", "35": "tls", "22": "http", "99": "other"} {
		t.Run(kind+"/"+rc, func(t *testing.T) {
			m := newManager(t, "gpu000")
			m.file("curl-fail", rc)
			_, err := ParseSlurmApply(SlurmModeSetup, m.run("sh", setupReq()), "")
			var failed *SlurmApplyFailedError
			if !errors.As(err, &failed) || failed.Reason != "server_unreachable" || failed.Kind != kind {
				t.Fatalf("err = %#v, want kind %s", err, kind)
			}
		})
	}
	// A marker without a status (an older script, or no mktemp) is still
	// server_unreachable, with the catch-all kind.
	_, err := ParseSlurmApply(SlurmModeSetup, "GWKS_APPLY fetch=failed\n", "")
	var failed *SlurmApplyFailedError
	if !errors.As(err, &failed) || failed.Kind != "other" {
		t.Fatalf("err = %#v", err)
	}
}

func TestSlurmApplyUnreadableGresConfStopsBeforeAnything(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads anything")
	}
	eachShell(t, func(t *testing.T, shell string) {
		m := newManager(t, "gpu000")
		if err := os.WriteFile(m.gres, []byte("x"), 0); err != nil {
			t.Fatal(err)
		}
		out := m.run(shell, setupReq())
		_, err := ParseSlurmApply(SlurmModeSetup, out, "")
		var failed *SlurmApplyFailedError
		if !errors.As(err, &failed) || failed.Step != messages.SlurmApplyStepGres {
			t.Fatalf("err = %v\n%s", err, out)
		}
		if m.read("curl.args") != "" {
			t.Fatal("went on to setup with a gres.conf it cannot read")
		}
	})
}

func TestSlurmApplyRefusesASecondRunAtTheSameTime(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("no flock")
	}
	eachShell(t, func(t *testing.T, shell string) {
		m := newManager(t, "gpu000")
		lock := "/run/gpuworks-slurm-apply.lock"
		if f, err := os.OpenFile(lock, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
			lock = "/tmp/gpuworks-slurm-apply.lock"
		} else {
			f.Close()
		}
		holder := exec.Command("flock", lock, "sleep", "30")
		holder.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := holder.Start(); err != nil {
			t.Fatal(err)
		}
		// the whole group: flock's child sleeps on holding the lock
		defer func() { _ = syscall.Kill(-holder.Process.Pid, syscall.SIGKILL); _ = holder.Wait() }()
		// give flock a moment to take the lock
		for i := 0; i < 100 && exec.Command("flock", "-n", lock, "true").Run() == nil; i++ {
			exec.Command("sleep", "0.05").Run()
		}
		out := m.run(shell, setupReq())
		_, err := ParseSlurmApply(SlurmModeSetup, out, "")
		var failed *SlurmApplyFailedError
		if !errors.As(err, &failed) || failed.Step != messages.SlurmApplyStepRun || failed.Reason != "busy" {
			t.Fatalf("err = %v\n%s", err, out)
		}
		if m.conf() != "<missing>" {
			t.Fatal("a second run changed gres.conf")
		}
	})
}

func TestParseSlurmApplyStepsAndStatuses(t *testing.T) {
	ok := "GWKS_SLURM step=precheck status=ok cluster=demo\nGWKS_SLURM step=check_script status=ok path=/opt/g/check.sh\nGWKS_SLURM step=token_file status=ok path=/opt/g/token\n" +
		"GWKS_SLURM step=shared status=no fstype=ext4\nGWKS_SLURM step=distribute node=cn1 status=ok\nGWKS_SLURM step=distribute node=cn2 status=failed\n" +
		"GWKS_SLURM step=distribute_summary ok=1 failed=cn2\nGWKS_SLURM step=slurm_conf status=ok path=/etc/slurm/slurm.conf backup=none chained=none interval=300\n" +
		"GWKS_SLURM step=configless value=yes\nGWKS_SLURM step=reconfigure status=ok\nGWKS_SLURM step=done status=partial\nGWKS_APPLY setup_exit=0\n"
	res, err := ParseSlurmApply(SlurmModeSetup, "GWKS_APPLY gres=added node=gpu000\n"+ok, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []SlurmApplyStep{{"gres", "pass"}, {"check_script", "pass"}, {"distribute", "warn"}, {"slurm_conf", "pass"}, {"reconfigure", "pass"}, {"resume", "skip"}}
	if len(res.Steps) != len(want) {
		t.Fatalf("steps = %+v", res.Steps)
	}
	for i := range want {
		if res.Steps[i] != want[i] {
			t.Fatalf("step %d = %+v, want %+v", i, res.Steps[i], want[i])
		}
	}
	if res.Shared || res.DistributeOK != 1 || strings.Join(res.DistributeFailed, ",") != "cn2" || !res.Partial() || res.Done != "partial" {
		t.Fatalf("result = %+v", res)
	}
	// a non-configless cluster: nodes do not pick the change up by themselves
	res, err = ParseSlurmApply(SlurmModeSetup, strings.Replace(ok, "configless value=yes", "configless value=no", 1), "")
	if err != nil || res.Steps[4] != (SlurmApplyStep{"reconfigure", "warn"}) {
		t.Fatalf("not configless: %+v, %v", res.Steps, err)
	}
	// shared storage: nothing is copied
	res, err = ParseSlurmApply(SlurmModeSetup, "GWKS_SLURM step=precheck status=ok cluster=demo\nGWKS_SLURM step=check_script status=ok path=x\nGWKS_SLURM step=shared status=yes fstype=nfs4\n"+
		"GWKS_SLURM step=distribute_summary ok=0 failed=none skipped=shared\nGWKS_SLURM step=slurm_conf status=ok path=x\nGWKS_SLURM step=reconfigure status=ok\nGWKS_SLURM step=done status=ok\n", "")
	if err != nil || !res.Shared || res.Steps[2] != (SlurmApplyStep{"distribute", "skip"}) || res.Partial() {
		t.Fatalf("shared: %+v, %v", res, err)
	}
}

func TestParseSlurmApplyFailures(t *testing.T) {
	cases := []struct {
		name, mode, output, ended string
		step, reason              string
		outputHas                 string
	}{
		{"never ran", SlurmModeSetup, "sh: 1: scontrol: not found\n", "the command ended Failed with exit code 127", messages.SlurmApplyStepRun, "", "ended Failed with exit code 127"},
		{"empty", SlurmModeSetup, "", "", messages.SlurmApplyStepRun, "", ""},
		{"setup stopped halfway", SlurmModeSetup, "GWKS_SLURM step=precheck status=ok cluster=demo\nGWKS_SLURM step=check_script status=ok path=x\nkilled\n", "", messages.SlurmApplyStepRun, "", "killed"},
		{"reconfigure failed", SlurmModeSetup, "GWKS_SLURM step=precheck status=ok cluster=demo\nGWKS_SLURM step=reconfigure status=failed\nGWKS_SLURM step=done status=failed\n", "", messages.SlurmApplyStepSetup, "reconfigure_failed", ""},
		{"token file", SlurmModeSetup, "GWKS_SLURM step=precheck status=ok cluster=demo\nGWKS_SLURM step=token_file status=failed path=x\nGWKS_SLURM step=done status=failed\n", "", messages.SlurmApplyStepSetup, "token_file_failed", ""},
		// A plain fail names no step: no reason (the UI says unknown), never "_failed".
		{"failed without naming a step", SlurmModeSetup, "GWKS_SLURM step=precheck status=ok cluster=demo\nERROR: cannot write in /opt/gwks\nGWKS_SLURM step=done status=failed\n", "", messages.SlurmApplyStepSetup, "", "cannot write"},
		{"slurm.conf not written", SlurmModeSetup, "GWKS_SLURM step=precheck status=ok cluster=demo\nGWKS_SLURM step=slurm_conf status=failed path=/etc/slurm/slurm.conf\nGWKS_SLURM step=done status=failed\n", "", messages.SlurmApplyStepSetup, "slurm_conf_failed", ""},
		{"not root", SlurmModeSetup, "GWKS_SLURM step=precheck status=failed reason=not_root\nGWKS_SLURM step=done status=failed\n", "", messages.SlurmApplyStepSetup, "not_root", ""},
		{"gres error", SlurmModeSetup, "GWKS_APPLY gres=error\ncannot write /etc/slurm/gres.conf\n", "", messages.SlurmApplyStepGres, "", "cannot write"},
		{"uninstall without its marker", SlurmModeUninstallAgents, "GWKS_SLURM step=precheck status=ok cluster=demo\nGWKS_SLURM step=done status=ok\n", "", messages.SlurmApplyStepRun, "", ""},
		{"remove without its marker", SlurmModeRemoveCheck, "GWKS_SLURM step=precheck status=ok cluster=demo\nGWKS_SLURM step=done status=ok\n", "", messages.SlurmApplyStepRun, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseSlurmApply(c.mode, c.output, c.ended)
			var failed *SlurmApplyFailedError
			if !errors.As(err, &failed) || failed.Step != c.step || failed.Reason != c.reason || !strings.Contains(failed.Output, c.outputHas) {
				t.Fatalf("err = %v, want step %s reason %q", err, c.step, c.reason)
			}
		})
	}
	res, err := ParseSlurmApply(SlurmModeRemoveCheck, "GWKS_SLURM step=remove status=noop\nGWKS_SLURM step=done status=ok\n", "")
	if err != nil || !res.RemoveNoop {
		t.Fatalf("noop remove: %+v, %v", res, err)
	}
	// output that merely looks like a marker, or lies in plain text, counts for nothing
	if _, err := ParseSlurmApply(SlurmModeSetup, "done\nGWKS_SLURM step=done status=ok\nstep=reconfigure status=ok\n", ""); err == nil {
		t.Fatal("an incomplete run was read as done")
	}
}

func TestSlurmApplyRequestValidation(t *testing.T) {
	bad := []SlurmApplyRequest{
		{Mode: "reboot", SetupURL: applyTestURL},
		{Mode: SlurmModeSetup, SetupURL: "ftp://x/y"},
		{Mode: SlurmModeSetup, SetupURL: "https://x/y';reboot;'"},
		{Mode: SlurmModeSetup, SetupURL: "https://x/y z"},
		{Mode: SlurmModeSetup, SetupURL: applyTestURL, ExpectGPU: []string{"gpu000; reboot"}},
		{Mode: SlurmModeSetup, SetupURL: applyTestURL, ExpectGPU: []string{"gpu000", ""}},
		{Mode: SlurmModeSetup, SetupURL: applyTestURL, ExpectGPU: []string{"$(reboot)"}},
	}
	for i, r := range bad {
		if r.Validate() == nil {
			t.Errorf("request %d was accepted: %+v", i, r)
		}
	}
	if err := setupReq().Validate(); err != nil {
		t.Fatal(err)
	}
}

// A line covers a node only when it is a gpu line that names the node (the
// NodeName anywhere in the line, not only first) or names no node at all; a
// line for another resource, or for other nodes, covers nothing.
func TestSlurmApplyOnlyGPULinesThatNameTheNodeCoverIt(t *testing.T) {
	cases := []struct {
		name, conf string
		covered    bool
	}{
		{"NodeName after Name", "Name=gpu NodeName=gpu000 File=/dev/nvidia0\n", true},
		{"NodeName last, with a type", "Name=gpu Type=a100 File=/dev/nvidia[0-3] NodeName=gpu[000-001]\n", true},
		{"lower case keys", "name=gpu nodename=gpu000 file=/dev/nvidia0\n", true},
		{"a gpu line for another node", "NodeName=gpu001 Name=gpu File=/dev/nvidia0\n", false},
		{"another node, NodeName last", "Name=gpu File=/dev/nvidia0 NodeName=gpu001\n", false},
		{"a line of another resource without NodeName", "Name=mps Count=100\n", false},
		{"a line of another resource for the node", "NodeName=gpu000 Name=shard Count=8\n", false},
		{"a comment that names the node", "# NodeName=gpu000 Name=gpu File=/dev/nvidia0\n", false},
		{"a trailing comment that names the node", "NodeName=gpu001 Name=gpu File=/dev/nvidia0 # NodeName=gpu000\n", false},
		{"autodetect for every node", "AutoDetect=nvml\n", true},
		{"autodetect off", "AutoDetect=off\n", false},
		{"autodetect for another node", "NodeName=gpu001 AutoDetect=nvml\n", false},
		{"autodetect for this node, NodeName last", "AutoDetect=nvml NodeName=gpu000\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, shell string) {
				m := newManager(t, "gpu000")
				if err := os.WriteFile(m.gres, []byte(c.conf), 0o600); err != nil {
					t.Fatal(err)
				}
				out := m.run(shell, setupReq())
				res, err := ParseSlurmApply(SlurmModeSetup, out, "")
				if err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				if covered := strings.Join(res.Present, ",") == "gpu000"; covered != c.covered {
					t.Fatalf("covered = %v, want %v; result %+v\n%s", covered, c.covered, res, m.conf())
				}
				if c.covered && m.conf() != c.conf {
					t.Fatalf("gres.conf of a covered node changed: %q", m.conf())
				}
				if !c.covered && !strings.HasSuffix(m.conf(), "NodeName=gpu000 Name=gpu File=/dev/nvidia0\n") {
					t.Fatalf("an uncovered node got no line: %q", m.conf())
				}
			})
		})
	}
}

// A node the cloud says has a card while Slurm gives it no gpu Gres gets no
// line (slurmd would refuse a gpu line for a node slurm.conf gives none) and is
// reported apart; a node with no card on either side is just a CPU node.
func TestSlurmApplyAddsNoLineForANodeWithoutAGPUGres(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		m := newManager(t, "gpu000", "cpu000=(null)", "shard000=shard:8", "plain000=(null)")
		out := m.run(shell, setupReq("gpu000", "cpu000", "shard000", "gone000"))
		res, err := ParseSlurmApply(SlurmModeSetup, out, "")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if got := m.conf(); got != "NodeName=gpu000 Name=gpu File=/dev/nvidia0\n" {
			t.Fatalf("gres.conf = %q", got)
		}
		if strings.Join(res.Added, ",") != "gpu000" || strings.Join(res.NoGPUGres, ",") != "cpu000,shard000" {
			t.Fatalf("result = %+v", res)
		}
		if !res.Partial() || res.Steps[0] != (SlurmApplyStep{SlurmApplyIDGres, proto.CheckWarn}) {
			t.Fatalf("a node without a gpu Gres is not flagged: %+v", res)
		}
	})
	// With nothing to compare to, a CPU-only cluster is a clean run.
	eachShell(t, func(t *testing.T, shell string) {
		m := newManager(t, "cpu000=(null)", "cpu001=(null)")
		out := m.run(shell, setupReq())
		res, err := ParseSlurmApply(SlurmModeSetup, out, "")
		if err != nil || res.Partial() || m.conf() != "<missing>" || len(res.Added) != 0 {
			t.Fatalf("result = %+v, %v, gres.conf %q", res, err, m.conf())
		}
	})
}
