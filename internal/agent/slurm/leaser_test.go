package slurm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// fakeSlurm answers the CLI calls the leaser makes the way Slurm 24.05 does
// on a single-node cluster with two T4s (see the research logs of
// 2026-11-13): one-line records of `scontrol -o`, `squeue -o` with the
// leaser's format, `sbatch --parsable`. The allocator hands out the lowest
// free GPU positions, or the ones a test pins with nextIDX.
type fakeSlurm struct {
	mu      sync.Mutex
	node    string
	gres    string
	self    string
	nextID  int
	jobs    map[string]*fakeJob
	order   []string
	calls   []string
	nextIDX []int // positions the next job gets, instead of the lowest free
	pending string
	failCmd string
	slurmdG string
	// configlessG is what `slurmd -G` prints given configlessArgs, the
	// running slurmd's own options (a configless node).
	configlessG    string
	configlessArgs []string
	// configlessRefused is what slurmd prints when slurmctld will not give
	// the caller the node's configuration (an agent that is not root).
	configlessRefused string
	// slurmdHangs makes `slurmd -G` never return until its context ends, as
	// Slurm 25.05 does on a configless node asked without the daemon's
	// options (it retries the DNS SRV lookup every ten seconds).
	slurmdHangs bool
	// partitions is the node's (scontrol's Partitions=); defaultPartition
	// is where a job naming none goes. A job whose partitions do not hold
	// the node pends on PartitionConfig, as Slurm's does.
	partitions       string
	defaultPartition string
	gpuCount         int
	// state and reason are the node's State= and Reason= (State=IDLE and no
	// Reason= when empty).
	state, reason string
	noIDX         map[string]bool // jobs whose record does not name their GPUs yet
}

type fakeJob struct {
	id, name, user, state, reason string
	idx                           []int
	// reqNode is the node the job asked for (sbatch -w), squeue's %n;
	// empty for a job that asked for none, which add makes this node's.
	reqNode string
}

func newFakeSlurm() *fakeSlurm {
	return &fakeSlurm{node: "gpuworks-test-0d8dedba7f17abe9", gres: "gpu:t4:2", self: "root", nextID: 20, jobs: map[string]*fakeJob{}, gpuCount: 2,
		partitions: "gpu", defaultPartition: "gpu"}
}

func (f *fakeSlurm) add(id, name, user string, idx ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[id] = &fakeJob{id: id, name: name, user: user, state: "RUNNING", idx: idx, reqNode: f.node}
	f.order = append(f.order, id)
}

func (f *fakeSlurm) used() map[int]bool {
	used := map[int]bool{}
	for _, j := range f.jobs {
		if j.state == "RUNNING" {
			for _, i := range j.idx {
				used[i] = true
			}
		}
	}
	return used
}

func idxString(idx []int) string {
	if len(idx) == 0 {
		return "N/A"
	}
	parts := make([]string, len(idx))
	for i, v := range idx {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, ",")
}

func (f *fakeSlurm) record(j *fakeJob) string {
	uid := "0"
	if j.user != "root" {
		uid = "1001"
	}
	line := fmt.Sprintf("JobId=%s JobName=%s UserId=%s(%s) GroupId=%s(%s) MCS_label=N/A Priority=1 Nice=0 Account=inference QOS=normal JobState=%s Reason=%s Dependency=(null) Requeue=1 Restarts=0 BatchFlag=1 Reboot=0 ExitCode=0:0 RunTime=00:00:05 TimeLimit=UNLIMITED Partition=gpu NodeList=", j.id, j.name, j.user, uid, j.user, uid, j.state, j.reason)
	if j.state == "RUNNING" && f.noIDX[j.id] {
		line += fmt.Sprintf("%s BatchHost=%s NumNodes=1 NumCPUs=1 TRES=cpu=1,mem=1G,node=1,billing=1,gres/gpu=1 JOB_GRES=(null) Nodes=%s CPU_IDs=0 Mem=1024 MinCPUsNode=1 TresPerNode=gres/gpu:1", f.node, f.node, f.node)
	} else if j.state == "RUNNING" {
		line += fmt.Sprintf("%s BatchHost=%s NumNodes=1 NumCPUs=1 TRES=cpu=1,mem=1G,node=1,billing=1,gres/gpu=%d JOB_GRES=gpu:t4:%d Nodes=%s CPU_IDs=0 Mem=1024 GRES=gpu:t4:%d(IDX:%s) MinCPUsNode=1 TresPerNode=gres/gpu:%d",
			f.node, f.node, len(j.idx), len(j.idx), f.node, len(j.idx), idxString(j.idx), len(j.idx))
	} else {
		line += "(null) NumNodes=1 TresPerNode=gres/gpu:1"
	}
	return line
}

func (f *fakeSlurm) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if f.slurmdHangs && name == "sh" && len(args) > 1 && args[1] == `exec slurmd -G "$@" 2>&1` {
		<-ctx.Done()
		return []byte("slurmd: error: fetch_config: DNS SRV lookup failed\n"), ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if f.failCmd != "" && strings.HasPrefix(call, f.failCmd) {
		return nil, errors.New("slurm_load_jobs error: Unable to contact slurm controller (connect failure)")
	}
	switch {
	case name == "sh" && slices.Equal(args, []string{"-c", `exec slurmd -G "$@" 2>&1`, "sh"}):
		if f.slurmdG == "" {
			return nil, errors.New("slurmd: not found")
		}
		return []byte(f.slurmdG), nil
	case name == "sh" && len(args) > 2 && args[0] == "-c" && args[1] == `exec slurmd -G "$@" 2>&1`:
		if f.configlessRefused != "" {
			return []byte(f.configlessRefused), errors.New("exit status 1")
		}
		if f.configlessG == "" || !slices.Equal(args[3:], f.configlessArgs) {
			return []byte("slurmd: fatal: Unable to determine this slurmd's NodeName\n"), errors.New("exit status 1")
		}
		return []byte(f.configlessG), nil
	case name == "scontrol" && slices.Equal(args[:3], []string{"-o", "show", "node"}):
		if args[3] != f.node {
			return nil, fmt.Errorf("Node %s not found", args[3])
		}
		state, tail := f.state, ""
		if state == "" {
			state = "IDLE"
		}
		if f.reason != "" {
			tail = " Reason=" + f.reason
		}
		return []byte(fmt.Sprintf("NodeName=%s Arch=x86_64 CoresPerSocket=24 CPUAlloc=0 CPUTot=48 Gres=%s NodeAddr=%s State=%s Partitions=%s%s\n", f.node, f.gres, f.node, state, f.partitions, tail)), nil
	case name == "scontrol" && slices.Equal(args[:4], []string{"-d", "-o", "show", "job"}):
		j, ok := f.jobs[args[4]]
		if !ok {
			return nil, errors.New("slurm_load_jobs error: Invalid job id specified")
		}
		return []byte(f.record(j) + "\n"), nil
	case name == "squeue":
		var b strings.Builder
		me := slices.Contains(args, "--me")
		var users []string
		for i, a := range args {
			if a == "-u" && i+1 < len(args) {
				users = strings.Split(args[i+1], ",")
			}
		}
		for _, id := range f.order {
			j, ok := f.jobs[id]
			if !ok {
				continue
			}
			if me && j.user != f.self {
				continue
			}
			if users != nil && !slices.Contains(users, j.user) {
				continue
			}
			if !me && users == nil && j.state != "RUNNING" {
				continue
			}
			if format := args[len(args)-1]; strings.HasSuffix(format, "|%n") {
				fmt.Fprintf(&b, "%s|%s|%s|%s|%s\n", j.id, j.name, j.user, j.state, j.reqNode)
			} else {
				fmt.Fprintf(&b, "%s|%s|%s|%s\n", j.id, j.name, j.user, j.state)
			}
		}
		return []byte(b.String()), nil
	case name == "sbatch":
		var n int
		var jobName string
		partition := f.defaultPartition
		for i, a := range args {
			if v, ok := strings.CutPrefix(a, "--gres=gpu:"); ok {
				fmt.Sscan(v, &n)
			}
			if a == "-J" {
				jobName = args[i+1]
			}
			if a == "-p" {
				partition = args[i+1]
			}
			if v, ok := strings.CutPrefix(a, "--partition="); ok {
				partition = v
			}
		}
		submitter := f.self
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "--uid="); ok {
				// Slurm lets only root (or SlurmUser) submit as another account.
				if f.self != "root" {
					return nil, errors.New("sbatch: error: Invalid user id: " + v)
				}
				submitter = v
			}
		}
		id := fmt.Sprint(f.nextID)
		f.nextID++
		j := &fakeJob{id: id, name: jobName, user: submitter, state: "RUNNING", reason: "None"}
		for i, a := range args {
			if a == "-w" && i+1 < len(args) {
				j.reqNode = args[i+1]
			}
		}
		if !slices.ContainsFunc(strings.Split(partition, ","), func(p string) bool { return slices.Contains(strings.Split(f.partitions, ","), p) }) {
			j.state, j.reason = "PENDING", "PartitionConfig"
		} else if f.pending != "" {
			j.state, j.reason = "PENDING", f.pending
		} else if f.nextIDX != nil {
			j.idx, f.nextIDX = f.nextIDX, nil
		} else {
			used := f.used()
			for i := 0; i < f.gpuCount && len(j.idx) < n; i++ {
				if !used[i] {
					j.idx = append(j.idx, i)
				}
			}
		}
		f.jobs[id] = j
		f.order = append(f.order, id)
		return []byte(id + "\n"), nil
	case name == "scancel":
		delete(f.jobs, args[0])
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected command %q", call)
}

func (f *fakeSlurm) called(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeSlurm) lastCall(prefix string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if strings.HasPrefix(f.calls[i], prefix) {
			return f.calls[i]
		}
	}
	return ""
}

// The two T4s of the test machine: /proc names them by device minor, and
// nvidia-smi by an eight-digit domain.
var t4s = []proto.GPUInfo{
	{Index: 0, UUID: "GPU-aaaa", PCIeBDF: "00000000:00:07.0", Name: "Tesla T4", MemoryTotalBytes: 15 << 30},
	{Index: 1, UUID: "GPU-bbbb", PCIeBDF: "00000000:00:08.0", Name: "Tesla T4", MemoryTotalBytes: 15 << 30},
}

const (
	card0 = "pci:0000:00:07.0"
	card1 = "pci:0000:00:08.0"
)

func procRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for minor, bdf := range []string{"0000:00:07.0", "0000:00:08.0"} {
		dir := filepath.Join(root, "driver", "nvidia", "gpus", bdf)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		info := fmt.Sprintf("Model: \t\t Tesla T4\nIRQ:   \t\t 34\nGPU UUID: \t\t GPU-x\nVideo BIOS: \t\t 90.04.38.00.03\nBus Type: \t\t PCIe\nDMA Size: \t\t 47 bits\nDMA Mask: \t\t 0x7fffffffffff\nBus Location: \t\t %s\nDevice Minor: \t\t %d\n", bdf, minor)
		if err := os.WriteFile(filepath.Join(dir, "information"), []byte(info), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// fakeClock is safe for concurrent use: a borrow waits for its job without
// holding the leaser, while the sweep runs.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func newTestLeaser(t *testing.T, f *fakeSlurm, cfg Config) (*Leaser, *fakeClock) {
	t.Helper()
	cfg.Node = f.node
	l := New(cfg, f.run, func(context.Context) ([]proto.GPUInfo, error) { return t4s, nil }, procRoot(t), nil)
	clock := &fakeClock{t: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)}
	l.now = clock.now
	l.poll = time.Millisecond
	l.self = f.self
	return l, clock
}

func TestDetect(t *testing.T) {
	f := newFakeSlurm()
	ok, err := Detect(context.Background(), f.run, f.node)
	if err != nil || !ok {
		t.Fatalf("Detect = %v, %v; want a Slurm GPU node", ok, err)
	}
	f.gres = "(null)"
	if ok, err := Detect(context.Background(), f.run, f.node); err != nil || ok {
		t.Fatalf("node without GRES: Detect = %v, %v; want false, nil", ok, err)
	}
	if _, err := Detect(context.Background(), f.run, "other"); err == nil {
		t.Fatal("unknown node: Detect returned no error")
	}
}

func TestExternalHoldersNameOtherJobsAndSkipOwn(t *testing.T) {
	f := newFakeSlurm()
	f.add("20", "holder", "alice", 1)
	f.add("21", HoldPrefix+"inst-a", "root", 0)
	l, _ := newTestLeaser(t, f, Config{})
	holders, err := l.ExternalCardHolders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 1 || len(holders["GPU-bbbb"]) != 1 || holders["GPU-bbbb"][0].Name != "Slurm job 20 (alice)" {
		t.Fatalf("holders = %+v; want card 1 held by Slurm job 20 (alice) only", holders)
	}
	// The job's cards are read once; the next round asks squeue only.
	before := f.called("scontrol -d -o show job")
	if _, err := l.ExternalCardHolders(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.called("scontrol -d -o show job"); got != before {
		t.Fatalf("job record read again: %d calls, want %d", got, before)
	}
	// An ended job stops being reported.
	f.mu.Lock()
	delete(f.jobs, "20")
	f.mu.Unlock()
	if holders, _ := l.ExternalCardHolders(context.Background()); len(holders) != 0 {
		t.Fatalf("holders after the job ended = %+v; want none", holders)
	}
}

// A record that does not name the GPUs a job asked for is read again next
// round, not kept for the life of the job.
func TestExternalHoldersRereadAJobWhoseGPUsAreNotKnownYet(t *testing.T) {
	f := newFakeSlurm()
	f.add("20", "holder", "alice") // running, its GRES not in the record yet
	l, _ := newTestLeaser(t, f, Config{})
	f.mu.Lock()
	f.noIDX = map[string]bool{"20": true}
	f.mu.Unlock()
	if holders, err := l.ExternalCardHolders(context.Background()); err != nil || len(holders) != 0 {
		t.Fatalf("first round = %+v, %v", holders, err)
	}
	f.mu.Lock()
	f.jobs["20"].idx = []int{0}
	f.noIDX = nil
	f.mu.Unlock()
	holders, err := l.ExternalCardHolders(context.Background())
	if err != nil || len(holders["GPU-aaaa"]) != 1 {
		t.Fatalf("second round = %+v, %v; want card 0 held by job 20", holders, err)
	}
}

func TestExternalHoldersFailWhenSlurmCannotBeAsked(t *testing.T) {
	f := newFakeSlurm()
	f.failCmd = "squeue"
	l, _ := newTestLeaser(t, f, Config{})
	if _, err := l.ExternalCardHolders(context.Background()); err == nil {
		t.Fatal("no error when squeue failed; the caller would read every card as free")
	}
}

func TestLeaseTakesWhatSlurmGives(t *testing.T) {
	f := newFakeSlurm()
	f.add("19", "holder", "alice", 0) // Slurm will not give card 0
	l, _ := newTestLeaser(t, f, Config{SbatchArgs: []string{"-A", "inference"}})
	held, fresh, err := l.Lease(context.Background(), "inst-a", []string{card0}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh || !slices.Equal(held, []string{card1}) {
		t.Fatalf("Lease = %v fresh=%v; want the card Slurm gave (%s), fresh", held, fresh, card1)
	}
	submit := f.lastCall("sbatch")
	for _, want := range []string{"--parsable", "-J " + HoldPrefix + "inst-a", "-w " + f.node, "--gres=gpu:1", "-c 1", "--mem=1G", "-A inference", "--wrap exec sleep infinity"} {
		if !strings.Contains(submit, want) {
			t.Errorf("sbatch call %q lacks %q", submit, want)
		}
	}
}

func TestLeaseKeepsConfiguredResources(t *testing.T) {
	f := newFakeSlurm()
	l, _ := newTestLeaser(t, f, Config{SbatchArgs: []string{"--mem=64G", "-c", "8"}})
	if _, _, err := l.Lease(context.Background(), "inst-a", []string{card0}, false, nil); err != nil {
		t.Fatal(err)
	}
	submit := f.lastCall("sbatch")
	if strings.Contains(submit, "--mem=1G") || strings.Contains(submit, "-c 1 ") {
		t.Fatalf("sbatch call %q overrides the configured CPUs or memory", submit)
	}
}

func TestLeaseReusesCardsAlreadyHeld(t *testing.T) {
	f := newFakeSlurm()
	f.add("30", HoldPrefix+"inst-a", "root", 0)
	l, _ := newTestLeaser(t, f, Config{})
	// An instance sharing card 0 with inst-a (dGPU) borrows nothing.
	held, fresh, err := l.Lease(context.Background(), "inst-b", []string{card0}, true, nil)
	if err != nil || fresh || !slices.Equal(held, []string{card0}) {
		t.Fatalf("Lease = %v fresh=%v err=%v; want card 0 as held", held, fresh, err)
	}
	if f.called("sbatch") != 0 {
		t.Fatal("submitted a job for a card the agent already holds")
	}
	// After a restart, inst-a finds its own job: not fresh, so its running
	// container is adopted.
	held, fresh, err = l.Lease(context.Background(), "inst-a", []string{card0}, false, nil)
	if err != nil || fresh || !slices.Equal(held, []string{card0}) {
		t.Fatalf("restart: Lease = %v fresh=%v err=%v; want its own card, not fresh", held, fresh, err)
	}
}

// After a rebinding the server may replay the spec with the cards placement
// chose before it has taken the agent's report: the instance's own job is
// what its container runs on, and it is kept (seen on the Aliyun Slurm node,
// where the job was swapped and the serving container recreated).
func TestLeaseKeepsTheInstancesOwnJobWhenTheSpecLags(t *testing.T) {
	f := newFakeSlurm()
	f.add("30", HoldPrefix+"inst-a", "root", 0)
	l, _ := newTestLeaser(t, f, Config{})
	held, fresh, err := l.Lease(context.Background(), "inst-a", []string{card1}, false, nil)
	if err != nil || fresh || !slices.Equal(held, []string{card0}) {
		t.Fatalf("Lease = %v fresh=%v err=%v; want its own job's card 0, not fresh", held, fresh, err)
	}
	if f.called("scancel") != 0 || f.called("sbatch") != 0 {
		t.Fatalf("calls %v; the instance's own job was swapped", f.calls)
	}
}

func TestInOrderOf(t *testing.T) {
	if got := inOrderOf([]string{"a", "b"}, []string{"b", "x"}); !slices.Equal(got, []string{"b", "a"}) {
		t.Fatalf("inOrderOf = %v", got)
	}
}

func TestLeaseMixesHeldAndBorrowedCards(t *testing.T) {
	f := newFakeSlurm()
	f.gpuCount = 2
	f.add("30", HoldPrefix+"inst-a", "root", 1)
	l, _ := newTestLeaser(t, f, Config{})
	held, fresh, err := l.Lease(context.Background(), "inst-b", []string{card1, card0}, true, nil)
	if err != nil || !fresh || !slices.Equal(held, []string{card1, card0}) {
		t.Fatalf("Lease = %v fresh=%v err=%v; want [%s %s]", held, fresh, err, card1, card0)
	}
	if submit := f.lastCall("sbatch"); !strings.Contains(submit, "--gres=gpu:1") {
		t.Fatalf("sbatch call %q; want one card asked for", submit)
	}
}

func TestLeasePendingReportsReasonAndGivesUp(t *testing.T) {
	f := newFakeSlurm()
	f.pending = "AssocGrpGRES"
	l, clock := newTestLeaser(t, f, Config{PendingTimeout: time.Minute})
	var progress []string
	calls := 0
	l.now = func() time.Time {
		calls++
		if calls > 3 {
			clock.advance(2 * time.Minute)
		}
		return clock.now()
	}
	_, _, err := l.Lease(context.Background(), "inst-a", []string{card0}, false, func(m string) { progress = append(progress, m) })
	if err == nil || !strings.Contains(err.Error(), "AssocGrpGRES") || !strings.Contains(err.Error(), "within 1m0s") {
		t.Fatalf("err = %v; want a timeout naming the pending reason", err)
	}
	if len(progress) != 1 || progress[0] != "waiting for Slurm job 20: AssocGrpGRES" {
		t.Fatalf("progress = %q; want the reason once", progress)
	}
	if f.called("scancel 20") != 1 {
		t.Fatal("the pending job was not cancelled")
	}
}

func TestLeaseCancelledWhileWaitingReturnsTheJob(t *testing.T) {
	f := newFakeSlurm()
	f.pending = "Resources"
	l, _ := newTestLeaser(t, f, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	_, _, err := l.Lease(ctx, "inst-a", []string{card0}, false, func(string) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled", err)
	}
	if f.called("scancel 20") != 1 {
		t.Fatal("the waiting job was not cancelled")
	}
}

func TestLeaseSubmitFailure(t *testing.T) {
	f := newFakeSlurm()
	f.failCmd = "sbatch"
	l, _ := newTestLeaser(t, f, Config{})
	if _, _, err := l.Lease(context.Background(), "inst-a", []string{card0}, false, nil); err == nil || !strings.Contains(err.Error(), "submit the Slurm job") {
		t.Fatalf("err = %v; want the submit failure", err)
	}
}

func TestLeaseRejectsUnknownPosition(t *testing.T) {
	f := newFakeSlurm()
	f.nextIDX = []int{5}
	l, _ := newTestLeaser(t, f, Config{})
	_, _, err := l.Lease(context.Background(), "inst-a", []string{card0}, false, nil)
	if err == nil || !strings.Contains(err.Error(), "matches no card") {
		t.Fatalf("err = %v; want an unmatched position", err)
	}
	if f.called("scancel 20") != 1 {
		t.Fatal("a job whose cards cannot be named was kept")
	}
}

func TestSweepReturnsIdleCardsAndReportsLostJobs(t *testing.T) {
	f := newFakeSlurm()
	l, clock := newTestLeaser(t, f, Config{IdleReturn: time.Minute})
	ctx := context.Background()
	if _, _, err := l.Lease(ctx, "inst-a", []string{card0}, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Lease(ctx, "inst-b", []string{card1}, false, nil); err != nil {
		t.Fatal(err)
	}
	var lost []string
	onLost := func(instance, message string) { lost = append(lost, instance+": "+message) }
	inUse := map[string][]string{"inst-a": {card0}, "inst-b": {card1}}
	if err := l.Sweep(ctx, inUse, onLost); err != nil {
		t.Fatal(err)
	}
	// inst-a went away: its card is kept for IdleReturn, then returned.
	delete(inUse, "inst-a")
	_ = l.Sweep(ctx, inUse, onLost)
	clock.advance(30 * time.Second)
	_ = l.Sweep(ctx, inUse, onLost)
	if f.called("scancel") != 0 {
		t.Fatal("returned a card before its idle time")
	}
	clock.advance(31 * time.Second)
	_ = l.Sweep(ctx, inUse, onLost)
	if f.called("scancel 20") != 1 || f.called("scancel 21") != 0 {
		t.Fatalf("scancel calls %v; want only inst-a's job 20 returned", f.calls)
	}
	// Somebody cancels inst-b's job: the instance is stopped.
	f.mu.Lock()
	delete(f.jobs, "21")
	f.mu.Unlock()
	_ = l.Sweep(ctx, inUse, onLost)
	if len(lost) != 1 || !strings.HasPrefix(lost[0], "inst-b: Slurm job 21") {
		t.Fatalf("lost = %q; want inst-b's job reported", lost)
	}
}

func TestSweepCancelsOrphanPendingJobs(t *testing.T) {
	f := newFakeSlurm()
	f.mu.Lock()
	f.jobs["40"] = &fakeJob{id: "40", name: HoldPrefix + "gone", user: "root", state: "PENDING", reason: "Priority", reqNode: f.node}
	f.order = append(f.order, "40")
	f.mu.Unlock()
	l, _ := newTestLeaser(t, f, Config{})
	if err := l.Sweep(context.Background(), nil, func(string, string) {}); err != nil {
		t.Fatal(err)
	}
	if f.called("scancel 40") != 1 {
		t.Fatal("a pending placeholder nobody waits for was kept")
	}
}

func TestSweepKeepsJobsWhenSlurmCannotBeAsked(t *testing.T) {
	f := newFakeSlurm()
	l, _ := newTestLeaser(t, f, Config{})
	if _, _, err := l.Lease(context.Background(), "inst-a", []string{card0}, false, nil); err != nil {
		t.Fatal(err)
	}
	f.failCmd = "squeue"
	lost := 0
	if err := l.Sweep(context.Background(), map[string][]string{"inst-a": {card0}}, func(string, string) { lost++ }); err == nil {
		t.Fatal("no error when squeue failed")
	}
	if lost != 0 || f.called("scancel") != 0 {
		t.Fatal("an unanswered squeue was read as the job having ended")
	}
}

func TestPositionsFollowSlurmdDeviceList(t *testing.T) {
	f := newFakeSlurm()
	// gres.conf lists the cards in the other order.
	f.slurmdG = "slurmd: Gres Name=gpu Type=t4 Count=1 Index=1 ID=7696487 File=/dev/nvidia1 Links=(null) Flags=HAS_FILE,HAS_TYPE,ENV_NVML\n" +
		"slurmd: Gres Name=gpu Type=t4 Count=1 Index=0 ID=7696487 File=/dev/nvidia0 Links=(null) Flags=HAS_FILE,HAS_TYPE,ENV_NVML\n"
	f.add("20", "holder", "alice", 0)
	l, _ := newTestLeaser(t, f, Config{})
	holders, err := l.ExternalCardHolders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(holders["GPU-bbbb"]) != 1 {
		t.Fatalf("holders = %+v; want Slurm position 0 to be /dev/nvidia1 (GPU-bbbb)", holders)
	}
}

// writeProc adds a process to a fake /proc: its comm and NUL-separated
// command line.
func writeProc(t *testing.T, root, pid, comm string, argv ...string) {
	t.Helper()
	dir := filepath.Join(root, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// An E-HPC compute node runs slurmd configless, as a dynamic node: plain
// `slurmd -G` cannot find its configuration (as on a real node: "Unable to
// determine this slurmd's NodeName"), so the positions come from slurmd -G
// with the running daemon's own options.
func TestPositionsOfAConfiglessNodeUseTheRunningSlurmdsOptions(t *testing.T) {
	f := newFakeSlurm()
	f.configlessArgs = []string{"-Z", "--conf", "Gres=gpu:2 Feature=gpuFeature", "--conf-server", "manager:6817"}
	// The device list is in the other order than the minors, so only the
	// configless call can place the job on /dev/nvidia1.
	f.configlessG = "slurmd: Gres Name=gpu Type=(null) Count=1 Index=1 ID=7696487 File=/dev/nvidia1 (null)\n" +
		"slurmd: Gres Name=gpu Type=(null) Count=1 Index=0 ID=7696487 File=/dev/nvidia0 (null)\n"
	f.add("20", "holder", "alice", 0)
	l, _ := newTestLeaser(t, f, Config{})
	writeProc(t, l.procRoot, "88", "bash", "/bin/bash")
	writeProc(t, l.procRoot, "7122", "slurmd", append([]string{"/usr/sbin/slurmd", "-D", "-s"}, f.configlessArgs...)...)
	holders, err := l.ExternalCardHolders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(holders["GPU-bbbb"]) != 1 {
		t.Fatalf("holders = %+v; want Slurm position 0 to be /dev/nvidia1 (GPU-bbbb)", holders)
	}
}

// An agent that is not root on a configless node: slurmctld gives it no
// configuration (as on an E-HPC node run by a cluster user, 2026-11-16), so
// the positions follow the device minors, and the agent says so.
func TestPositionsOfAConfiglessNodeRefusedToAUserFollowTheMinors(t *testing.T) {
	f := newFakeSlurm()
	f.configlessArgs = []string{"-Z", "--conf", "Gres=gpu:2 Feature=gpuFeature", "--conf-server", "manager:6817"}
	f.configlessRefused = "slurmd: error: _fetch_child: failed to fetch remote configs: Invalid user id\n" +
		"slurmd: error: _establish_configuration: failed to load configs\n" +
		"slurmd: error: slurmd initialization failed\n"
	f.add("20", "holder", "alice", 1)
	l, _ := newTestLeaser(t, f, Config{})
	var logged bytes.Buffer
	l.log = slog.New(slog.NewTextHandler(&logged, nil))
	writeProc(t, l.procRoot, "7122", "slurmd", append([]string{"/usr/sbin/slurmd", "-D", "-s"}, f.configlessArgs...)...)
	holders, err := l.ExternalCardHolders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(holders["GPU-bbbb"]) != 1 || len(holders["GPU-aaaa"]) != 0 {
		t.Fatalf("holders = %+v; want Slurm position 1 to be /dev/nvidia1 (GPU-bbbb)", holders)
	}
	if !strings.Contains(logged.String(), "device minor order") || !strings.Contains(logged.String(), "slurmd initialization failed") {
		t.Fatalf("log = %q; want the fallback and slurmd's error", logged.String())
	}
}

// On a configless node, slurmd -G of Slurm 25.05 never gives up (seen on an
// E-HPC node, 2026-11-18: a deployment waited on "waiting for Slurm job 1"
// for 20 minutes while the placeholder job ran). Asking it is bounded, so
// the positions fall back to the device minors instead of hanging the lease.
func TestASlurmdThatNeverAnswersDoesNotHangThePositions(t *testing.T) {
	old := slurmdListTimeout
	slurmdListTimeout = 50 * time.Millisecond
	t.Cleanup(func() { slurmdListTimeout = old })
	f := newFakeSlurm()
	f.configlessArgs = []string{"-Z", "--conf", "Gres=gpu:2 Feature=gpuFeature", "--conf-server", "manager:6817"}
	f.slurmdHangs = true
	f.add("20", "holder", "alice", 1)
	l, _ := newTestLeaser(t, f, Config{})
	writeProc(t, l.procRoot, "7122", "slurmd", append([]string{"/usr/sbin/slurmd", "-D", "-s"}, f.configlessArgs...)...)
	done := make(chan map[string][]proto.CardProcess, 1)
	go func() {
		holders, err := l.ExternalCardHolders(context.Background())
		if err != nil {
			t.Error(err)
		}
		done <- holders
	}()
	select {
	case holders := <-done:
		if len(holders["GPU-bbbb"]) != 1 {
			t.Fatalf("holders = %+v; want Slurm position 1 to be /dev/nvidia1 by the minors", holders)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("positions still waiting on slurmd -G")
	}
	// A configless node is asked only with the running daemon's options:
	// the bare form cannot find its configuration there.
	for _, c := range f.calls {
		if c == "sh -c exec slurmd -G \"$@\" 2>&1 sh" {
			t.Fatalf("asked the bare slurmd -G on a configless node: %q", f.calls)
		}
	}
}

func TestSlurmdConfigArgs(t *testing.T) {
	root := t.TempDir()
	if got := slurmdConfigArgs(root); got != nil {
		t.Fatalf("no slurmd: %q", got)
	}
	// A slurmd started from its own slurm.conf names no configuration: plain slurmd -G is enough.
	writeProc(t, root, "10", "slurmd", "/usr/sbin/slurmd", "-D", "-s", "-v")
	if got := slurmdConfigArgs(root); got != nil {
		t.Fatalf("slurmd without config options: %q", got)
	}
	writeProc(t, root, "11", "slurmd", "slurmd", "-D", "--conf-server=ctl:6817", "-N", "gpu7", "-f", "/etc/slurm/alt.conf", "--conf")
	got := slurmdConfigArgs(root)
	if strings.Join(got, "|") != "--conf-server=ctl:6817|-N|gpu7|-f|/etc/slurm/alt.conf" {
		t.Fatalf("args = %q", got)
	}
}

// The node is not in the default partition (E-HPC defaults to "comp" and
// puts each queue in a partition of its own): the placeholder job names the
// node's partitions, or it would pend on PartitionConfig; a partition the
// operator configured is kept.
func TestLeaseNamesTheNodesPartitions(t *testing.T) {
	f := newFakeSlurm()
	f.partitions, f.defaultPartition = "gpu,debug", "comp"
	l, _ := newTestLeaser(t, f, Config{})
	if _, _, err := l.Lease(context.Background(), "inst-a", []string{card0}, false, nil); err != nil {
		t.Fatal(err)
	}
	if submit := f.lastCall("sbatch"); !strings.Contains(submit, "-p gpu,debug") {
		t.Fatalf("sbatch call %q names no partition of the node", submit)
	}

	f2 := newFakeSlurm()
	f2.partitions, f2.defaultPartition = "gpu,serving", "comp"
	l2, _ := newTestLeaser(t, f2, Config{SbatchArgs: []string{"--partition=serving"}})
	if _, _, err := l2.Lease(context.Background(), "inst-a", []string{card0}, false, nil); err != nil {
		t.Fatal(err)
	}
	if submit := f2.lastCall("sbatch"); strings.Contains(submit, "-p ") || !strings.Contains(submit, "--partition=serving") {
		t.Fatalf("sbatch call %q overrides the configured partition", submit)
	}
}

// Every agent of a cluster submits as the same account, so squeue --me lists
// the placeholder jobs of every node: an agent keeps to the ones that asked
// for its node, and neither returns another node's running job as idle nor
// cancels its pending one, nor uses its cards (2026-11-18 review).
func TestAnotherNodesPlaceholderJobsAreLeftToItsAgent(t *testing.T) {
	f := newFakeSlurm()
	f.mu.Lock()
	f.jobs["30"] = &fakeJob{id: "30", name: HoldPrefix + "inst-b", user: "root", state: "RUNNING", idx: []int{0}, reqNode: "gpu-other"}
	f.jobs["31"] = &fakeJob{id: "31", name: HoldPrefix + "inst-c", user: "root", state: "PENDING", reason: "Resources", reqNode: "gpu-other"}
	f.order = append(f.order, "30", "31")
	f.mu.Unlock()
	l, clock := newTestLeaser(t, f, Config{IdleReturn: time.Minute})
	clock.advance(adoptedGrace + time.Hour)
	for i := 0; i < 3; i++ {
		if err := l.Sweep(context.Background(), nil, func(string, string) {}); err != nil {
			t.Fatal(err)
		}
		clock.advance(2 * time.Minute)
	}
	if f.called("scancel 30") != 0 || f.called("scancel 31") != 0 {
		t.Fatalf("another node's placeholder jobs were cancelled: %q", f.calls)
	}
	if len(l.jobs) != 0 {
		t.Fatalf("this agent took another node's jobs as its own: %+v", l.jobs)
	}
}

// After a restart the engine knows its instances only from the server's
// first state sync; until then a job found running is not returned as
// idle, or the cards go back to Slurm under containers still using them.
func TestAJobFoundAtStartIsKeptForAGraceBeforeIdleReturn(t *testing.T) {
	f := newFakeSlurm()
	f.add("20", HoldPrefix+"inst-a", "root", 0)
	l, clock := newTestLeaser(t, f, Config{IdleReturn: time.Minute})
	sweep := func() {
		t.Helper()
		if err := l.Sweep(context.Background(), nil, func(string, string) {}); err != nil {
			t.Fatal(err)
		}
	}
	for elapsed := time.Duration(0); elapsed < adoptedGrace-2*time.Minute; elapsed += 2 * time.Minute {
		sweep()
		clock.advance(2 * time.Minute)
	}
	if f.called("scancel 20") != 0 {
		t.Fatal("a job found at start was returned within the grace")
	}
	clock.advance(2 * time.Minute)
	sweep()
	clock.advance(2 * time.Minute)
	sweep()
	if f.called("scancel 20") != 1 {
		t.Fatalf("after the grace the unused job was not returned: %q", f.calls)
	}
}

// Two instances that take whole cards, placed on cards 0 and 1: inst-b
// borrows first and Slurm gives it card 0. inst-a, placed on card 0, must
// not run on inst-b's card; it borrows its own, and Slurm gives it card 1
// (2026-11-18 review: both ran on card 0).
func TestAnInstanceTakingWholeCardsNeverRunsOnAnotherInstancesCards(t *testing.T) {
	f := newFakeSlurm()
	f.gpuCount = 2
	l, _ := newTestLeaser(t, f, Config{})
	b, _, err := l.Lease(context.Background(), "inst-b", []string{card1}, false, nil)
	if err != nil || !slices.Equal(b, []string{card0}) {
		t.Fatalf("inst-b = %v %v; want card 0, the one Slurm gives first", b, err)
	}
	a, fresh, err := l.Lease(context.Background(), "inst-a", []string{card0}, false, nil)
	if err != nil || !fresh || !slices.Equal(a, []string{card1}) {
		t.Fatalf("inst-a = %v fresh=%v %v; want a job of its own, on card 1", a, fresh, err)
	}
	if f.called("sbatch") != 2 {
		t.Fatalf("%d jobs submitted; want one per instance", f.called("sbatch"))
	}
}

// A borrow waits for its pending job without holding the leaser: the sweep
// returns an idle job of this agent meanwhile, whose card the pending job
// waits for, and the borrow then gets it (E-HPC, 2026-11-18: a retried
// instance waited the whole pending timeout for its own agent's idle job).
func TestABorrowWaitingForItsJobLetsTheSweepReturnAnIdleJob(t *testing.T) {
	f := newFakeSlurm()
	f.gpuCount = 1
	l, clock := newTestLeaser(t, f, Config{IdleReturn: time.Minute, PendingTimeout: time.Hour})
	// inst-old's start failed: its job holds the card, and no instance uses it.
	if _, _, err := l.Lease(context.Background(), "inst-old", []string{card0}, false, nil); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.pending = "Resources" // the card is taken: the next job pends
	f.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, _, err := l.Lease(context.Background(), "inst-new", []string{card0}, false, nil)
		done <- err
	}()
	// The sweep runs while the borrow waits, sees inst-old's job idle, and returns it.
	deadline := time.Now().Add(5 * time.Second)
	for f.called("scancel 20") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the sweep could not return the idle job while a borrow waited")
		}
		if err := l.Sweep(context.Background(), nil, func(string, string) {}); err != nil {
			t.Fatal(err)
		}
		clock.advance(2 * time.Minute)
		time.Sleep(5 * time.Millisecond)
	}
	// The card is free: Slurm starts the pending job.
	f.mu.Lock()
	f.pending = ""
	for _, j := range f.jobs {
		if j.state == "PENDING" {
			j.state, j.idx = "RUNNING", []int{0}
		}
	}
	f.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("borrow = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the borrow did not get the card once it was free")
	}
	if n := f.called("scancel 21"); n != 0 {
		t.Fatalf("the sweep cancelled the job a borrow waited for (%d)", n)
	}
}

// The grace for a job found at start ends with the first Lease: the engine
// leases for the instances the server's sync named, so the cards in use are
// known. An adopted job nobody uses is then idle, and returned, or a new
// borrow waits behind it (E-HPC, 2026-11-18: after an upgrade the agent kept
// its failed attempt's job while the retry pended behind it).
func TestTheGraceForJobsFoundAtStartEndsWithTheFirstLease(t *testing.T) {
	f := newFakeSlurm()
	f.gpuCount = 2
	f.add("30", HoldPrefix+"inst-old", "root", 0)
	l, clock := newTestLeaser(t, f, Config{IdleReturn: time.Minute})
	if err := l.Sweep(context.Background(), nil, func(string, string) {}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Lease(context.Background(), "inst-new", []string{card1}, false, nil); err != nil {
		t.Fatal(err)
	}
	newCards := map[string][]string{"inst-new": {card1}}
	for i := 0; i < 3; i++ {
		clock.advance(time.Minute)
		if err := l.Sweep(context.Background(), newCards, func(string, string) {}); err != nil {
			t.Fatal(err)
		}
	}
	if f.called("scancel 30") != 1 {
		t.Fatalf("after the first lease the unused adopted job was kept: %q", f.calls)
	}
}
