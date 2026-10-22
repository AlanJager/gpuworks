package runveilcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/udocker"
	"gpuworks.ai/gpuworks/test/ocifixture"
)

const image = "registry.example/team/runner:v1"

// deadPID is above any kernel's pid_max, so it never names a live process.
const deadPID = 1 << 30

// node is a runveil node on disk: one image and three containers -- one
// running (this test process stands in for it), one exited, one never started.
type node struct {
	driver *udocker.Driver
	state  string
	cli    *CLI
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	ran    [][]string
}

func newNode(t *testing.T) *node {
	t.Helper()
	t.Setenv("GPUWORKS_UDOCKER_LAUNCHER", "")
	base := t.TempDir()
	d := udocker.New(base)
	archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{RepoTags: []string{image}, LayerBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := d.LoadImage(ctx, image, bytes.NewReader(archive)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gpuworks-run1", "gpuworks-exit1", "other-new"} {
		if err := d.Prepare(ctx, runtime.WorkloadSpec{Name: name, Image: image}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	_, state := d.StateDirs()
	n := &node{driver: d, state: state, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	// runveil checks the command exists in the guest before launching it.
	udockerDir, _ := d.StateDirs()
	for _, bin := range []string{"bin/sh", "usr/bin/tool"} {
		path := filepath.Join(udockerDir, "containers", "gpuworks-run1", "ROOT", bin)
		must(os.MkdirAll(filepath.Dir(path), 0o755))
		must(os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755))
	}
	n.started("gpuworks-run1", os.Getpid(), "booting\nloading weights\nready\n")
	n.started("gpuworks-exit1", deadPID, "line1\nline2\n")
	now := time.Now()
	n.cli = &CLI{
		Driver: d, Stdin: strings.NewReader(""), Stdout: n.stdout, Stderr: n.stderr,
		Run: func(_ context.Context, argv []string) (int, error) {
			n.ran = append(n.ran, argv)
			return 7, nil
		},
		Now:  func() time.Time { return now.Add(3 * time.Hour) },
		Poll: 10 * time.Millisecond,
	}
	return n
}

// started records what the driver writes when it launches a container: the
// pid, the log and the workload spec the terminal reconstructs.
func (n *node) started(name string, pid int, log string) {
	must(os.WriteFile(filepath.Join(n.state, name+".pid"), []byte(fmt.Sprint(pid)), 0o644))
	must(os.WriteFile(n.driver.LogPath(name), []byte(log), 0o644))
	spec, _ := json.Marshal(runtime.WorkloadSpec{Name: name, Image: image, Env: map[string]string{"MODEL": "m"}})
	must(os.WriteFile(filepath.Join(n.state, name+".spec.json"), spec, 0o600))
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func (n *node) run(t *testing.T, args ...string) int {
	t.Helper()
	n.stdout.Reset()
	n.stderr.Reset()
	return n.cli.Execute(context.Background(), args)
}

func rows(out string) [][]string {
	var rows [][]string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		rows = append(rows, strings.Fields(line))
	}
	return rows
}

func TestPsShowsRunningByDefaultAndAllWithA(t *testing.T) {
	n := newNode(t)
	if code := n.run(t, "ps"); code != 0 {
		t.Fatalf("ps: %d %s", code, n.stderr)
	}
	got := rows(n.stdout.String())
	if len(got) != 2 || strings.Join(got[0], " ") != "NAME IMAGE STATUS PID CREATED" {
		t.Fatalf("ps:\n%s", n.stdout)
	}
	if want := []string{"gpuworks-run1", image, "running", fmt.Sprint(os.Getpid()), "3", "hours", "ago"}; strings.Join(got[1], " ") != strings.Join(want, " ") {
		t.Fatalf("running row = %v, want %v", got[1], want)
	}

	if code := n.run(t, "ps", "-a"); code != 0 {
		t.Fatalf("ps -a: %d %s", code, n.stderr)
	}
	states := map[string]string{}
	for _, row := range rows(n.stdout.String())[1:] {
		states[row[0]] = row[2] + " " + row[3]
	}
	want := map[string]string{
		"gpuworks-run1":  "running " + fmt.Sprint(os.Getpid()),
		"gpuworks-exit1": fmt.Sprintf("exited %d", deadPID),
		"other-new":      "created -",
	}
	for name, state := range want {
		if states[name] != state {
			t.Errorf("%s: %q, want %q", name, states[name], state)
		}
	}
	if len(states) != 3 {
		t.Errorf("ps -a rows: %v", states)
	}
}

func TestImagesListsReferenceIDAndStoredSize(t *testing.T) {
	n := newNode(t)
	must(n.driver.TagImage(context.Background(), image, "mirror.example/runner@sha256:"+strings.Repeat("a", 64)))
	// An image the old udocker CLI registered keeps no manifest.
	udockerDir, _ := n.driver.StateDirs()
	legacy := filepath.Join(udockerDir, "repos", "legacy", "app", "1.0")
	must(os.MkdirAll(legacy, 0o755))
	must(os.WriteFile(filepath.Join(legacy, "TAG"), []byte("legacy/app:1.0\n"), 0o644))
	if code := n.run(t, "images"); code != 0 {
		t.Fatalf("images: %d %s", code, n.stderr)
	}
	inspection, err := n.driver.InspectImage(context.Background(), image)
	if err != nil {
		t.Fatal(err)
	}
	got := rows(n.stdout.String())
	if len(got) != 4 || strings.Join(got[0], " ") != "REPOSITORY TAG IMAGE ID SIZE" {
		t.Fatalf("images:\n%s", n.stdout)
	}
	byRepo := map[string][]string{}
	for _, row := range got[1:] {
		byRepo[row[0]] = row[1:]
	}
	id := strings.TrimPrefix(inspection.ID, "sha256:")[:12]
	if r := byRepo["registry.example/team/runner"]; len(r) != 3 || r[0] != "v1" || r[1] != id || !strings.HasSuffix(r[2], "kB") {
		t.Errorf("tagged row = %v (id %s)", r, id)
	}
	if r := byRepo["mirror.example/runner"]; len(r) != 3 || r[0] != "sha256:"+strings.Repeat("a", 64) || r[1] != id {
		t.Errorf("digest row = %v", r)
	}
	if r := byRepo["legacy/app"]; strings.Join(r, " ") != "1.0 - -" {
		t.Errorf("legacy row = %v", r)
	}
}

func TestExecRunsTheWorkloadShellAndReturnsItsStatus(t *testing.T) {
	n := newNode(t)
	// A unique prefix names the container; the command follows.
	if code := n.run(t, "exec", "gpuworks-r", "--", "/usr/bin/tool", "-L"); code != 7 {
		t.Fatalf("exec status = %d, want the command's 7; stderr %s", code, n.stderr)
	}
	if len(n.ran) != 1 {
		t.Fatalf("ran %d commands", len(n.ran))
	}
	argv := strings.Join(n.ran[0], " ")
	udockerDir, _ := n.driver.StateDirs()
	for _, want := range []string{"proot", filepath.Join(udockerDir, "containers", "gpuworks-run1", "ROOT"), "MODEL=m", "/usr/bin/tool -L"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv lacks %q: %s", want, argv)
		}
	}
	// No command: the login shell the web terminal also opens.
	n.ran = nil
	n.run(t, "exec", "gpuworks-run1")
	if len(n.ran) != 1 || !strings.HasSuffix(strings.Join(n.ran[0], " "), "/bin/sh -l") {
		t.Fatalf("default command: %v", n.ran)
	}
}

func TestExecAndLogsRefuseUnknownOrAmbiguousNames(t *testing.T) {
	n := newNode(t)
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"exec", "nope"}, 1, `no container named "nope"`},
		{[]string{"exec", "gpuworks-"}, 1, "matches several containers: gpuworks-exit1, gpuworks-run1"},
		{[]string{"exec"}, 2, "exec needs a container name"},
		// Never started here: there is no spec to rebuild a shell from.
		{[]string{"exec", "other-new"}, 1, "workload spec"},
		{[]string{"logs", "other-new"}, 1, "never started"},
		{[]string{"logs"}, 2, "exactly one container name"},
		{[]string{"logs", "a", "b"}, 2, "exactly one container name"},
		{[]string{"ps", "extra"}, 2, "no arguments"},
		{[]string{"images", "extra"}, 2, "no arguments"},
		{[]string{"rmi"}, 2, "unknown command"},
	} {
		code := n.run(t, tc.args...)
		if code != tc.code || !strings.Contains(n.stderr.String(), tc.want) {
			t.Errorf("%v: code %d stderr %q, want %d containing %q", tc.args, code, n.stderr, tc.code, tc.want)
		}
	}
	if len(n.ran) != 0 {
		t.Fatalf("a refused exec ran %v", n.ran)
	}
}

func TestLogsPrintsAllOrTheLastLines(t *testing.T) {
	n := newNode(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"logs", "gpuworks-run1"}, "booting\nloading weights\nready\n"},
		{[]string{"logs", "-n", "2", "gpuworks-run1"}, "loading weights\nready\n"},
		// Flags may follow the name.
		{[]string{"logs", "gpuworks-run1", "--tail", "1"}, "ready\n"},
		{[]string{"logs", "-n", "0", "gpuworks-run1"}, ""},
		{[]string{"logs", "-n", "9", "gpuworks-exit1"}, "line1\nline2\n"},
	} {
		if code := n.run(t, tc.args...); code != 0 || n.stdout.String() != tc.want {
			t.Errorf("%v: code %d out %q, want %q (stderr %s)", tc.args, code, n.stdout, tc.want, n.stderr)
		}
	}
}

// Following an exited container prints what is there and returns.
func TestLogsFollowStopsWhenTheContainerIsNotRunning(t *testing.T) {
	n := newNode(t)
	done := make(chan int, 1)
	go func() { done <- n.cli.Execute(context.Background(), []string{"logs", "-f", "gpuworks-exit1"}) }()
	select {
	case code := <-done:
		if code != 0 || n.stdout.String() != "line1\nline2\n" {
			t.Fatalf("code %d out %q", code, n.stdout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("logs -f kept following an exited container")
	}
}

// Following a running container picks up appended output, survives the log
// being truncated, and stops when the caller cancels.
func TestLogsFollowStreamsAppendedOutputUntilCancelled(t *testing.T) {
	n := newNode(t)
	out := &syncBuffer{}
	n.cli.Stdout = out
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- n.cli.Execute(ctx, []string{"logs", "-f", "-n", "1", "gpuworks-run1"}) }()
	waitFor(t, out, "ready\n")
	path := n.driver.LogPath("gpuworks-run1")
	appendTo(t, path, "request 1\n")
	waitFor(t, out, "ready\nrequest 1\n")
	must(os.WriteFile(path, []byte("rotated\n"), 0o644))
	waitFor(t, out, "ready\nrequest 1\nrotated\n")
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("cancelled follow exited %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("logs -f ignored cancellation")
	}
}

func TestTailStartOnLongLogsAndEdgeCases(t *testing.T) {
	dir := t.TempDir()
	var long strings.Builder
	for i := 0; i < 20000; i++ { // several tail chunks
		fmt.Fprintf(&long, "line %05d\n", i)
	}
	for _, tc := range []struct {
		name, body string
		lines      int
		want       string
	}{
		{"long", long.String(), 2, "line 19998\nline 19999\n"},
		{"no final newline", "a\nb\nc", 2, "b\nc"},
		{"more lines than exist", "a\nb\n", 5, "a\nb\n"},
		{"empty", "", 3, ""},
		{"blank lines count", "a\n\n\n", 2, "\n\n"},
	} {
		path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-"))
		must(os.WriteFile(path, []byte(tc.body), 0o644))
		var out bytes.Buffer
		offset, err := printTail(&out, path, tc.lines)
		if err != nil || out.String() != tc.want || offset != int64(len(tc.body)) {
			t.Errorf("%s: %q offset %d err %v, want %q", tc.name, out.String(), offset, err, tc.want)
		}
	}
}

func TestStateDirectoryMistakesAreExplained(t *testing.T) {
	cli := &CLI{Driver: udocker.New(t.TempDir()), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	for _, cmd := range []string{"ps", "images", "logs", "exec"} {
		stderr := &bytes.Buffer{}
		cli.Stderr = stderr
		args := []string{cmd}
		if cmd == "logs" || cmd == "exec" {
			args = append(args, "x")
		}
		if code := cli.Execute(context.Background(), args); code != 1 || !strings.Contains(stderr.String(), "no runveil state") {
			t.Errorf("%s: code %d stderr %q", cmd, code, stderr)
		}
	}
}

func TestHumanSizeMatchesDocker(t *testing.T) {
	for n, want := range map[int64]string{0: "0B", 999: "999B", 1000: "1kB", 65_536: "65.5kB", 9_438_111_744: "9.44GB"} {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %s, want %s", n, got, want)
		}
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitFor(t *testing.T, out *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for out.String() != want {
		if time.Now().After(deadline) {
			t.Fatalf("output %q, want %q", out.String(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// A login user who is neither root nor the agent's user (an SSH install
// leaves the state directory to that user while the agent runs as root)
// inspects exactly what root sees, but may not open a shell in a container.
func TestOtherUsersMayInspectButNotExec(t *testing.T) {
	n := newNode(t)
	owner := os.Geteuid()
	outputs := map[string]string{}
	inspect := [][]string{{"ps"}, {"ps", "-a"}, {"images"}, {"logs", "gpuworks-run1"}}
	for _, euid := range []int{0, owner + 1} {
		n.cli.Geteuid = func() int { return euid }
		for _, args := range inspect {
			if code := n.run(t, args...); code != 0 {
				t.Fatalf("%v as uid %d: code %d stderr %q", args, euid, code, n.stderr)
			}
			key := strings.Join(args, " ")
			if euid == 0 {
				outputs[key] = n.stdout.String()
			} else if n.stdout.String() != outputs[key] {
				t.Errorf("%v differs for another user:\n%s\nroot saw:\n%s", args, n.stdout, outputs[key])
			}
		}
	}
	if !strings.Contains(outputs["ps"], "gpuworks-run1") {
		t.Fatalf("ps shows no running container:\n%s", outputs["ps"])
	}

	n.cli.Geteuid = func() int { return owner + 1 }
	n.ran = nil
	code := n.run(t, "exec", "gpuworks-run1")
	if code != 1 || !strings.Contains(n.stderr.String(), fmt.Sprintf("exec needs root (sudo) or the agent's user (uid %d)", owner)) || len(n.ran) != 0 {
		t.Fatalf("exec as another user: code %d stderr %q ran %v", code, n.stderr, n.ran)
	}
	for _, euid := range []int{0, owner} {
		n.cli.Geteuid = func() int { return euid }
		n.ran = nil
		if code := n.run(t, "exec", "gpuworks-run1"); code != 7 || len(n.ran) != 1 {
			t.Errorf("exec as uid %d: code %d stderr %q", euid, code, n.stderr)
		}
	}
}
