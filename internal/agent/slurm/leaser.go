package slurm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// HoldPrefix starts the name of every placeholder job: what tells the agent's
// own jobs from everybody else's, including after a restart.
const HoldPrefix = "gpuworks-hold-"

// Runner runs a Slurm CLI command and returns its standard output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs the command on the host.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return out, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(exit.Stderr)))
		}
		return out, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// Config is how the agent borrows cards on its node.
type Config struct {
	// Node is this machine's Slurm node name.
	Node string
	// SbatchArgs are added to every placeholder job (account, partition,
	// QOS, time limit, memory), e.g. ["-A", "inference", "--time=7-0"].
	SbatchArgs []string
	// PendingTimeout bounds how long a placeholder job may wait to start.
	PendingTimeout time.Duration
	// IdleReturn is how long a borrowed card stays with the platform after
	// its last instance went.
	IdleReturn time.Duration
	// AccountUser is the account the placeholder jobs are submitted as
	// (sbatch --uid), so squeue and accounting show that account rather than
	// the agent's. Only a root agent can; for another it is ignored. Empty
	// submits as the agent's own account.
	AccountUser string
	// SubmittersFile is where the accounts the agent has submitted placeholder
	// jobs as are kept (one per line), so that after a restart the jobs it
	// submitted under an account the entry has since changed are still its
	// own: found, counted and returned, not read as another user's jobs that
	// hold cards for good. Empty keeps none.
	SubmittersFile string
	// OrderFile is a file holding the output of `slurmd -G` that something
	// with the right to ask wrote (the health check, as root, for an agent
	// that is not): the node's GPU order is read from it before slurmd is
	// asked. Empty reads none.
	OrderFile string
}

const (
	// DefaultPendingTimeout and DefaultIdleReturn apply when the entry
	// gives none; they are the entry's own defaults too (store.SlurmDefault*,
	// tied by internal/integrationtest/slurm).
	DefaultPendingTimeout = 10 * time.Minute
	DefaultIdleReturn     = 60 * time.Second
	// adoptedGrace is how long after the agent starts a job it found is
	// kept although no instance seems to use its cards, until the first
	// Lease: the engine learns its instances from the server's first state
	// sync, which an unreachable or slow server delays, while their
	// containers keep running on those cards. Once it leases, the cards in
	// use are known, and an adopted job nobody uses is idle like any other
	// (it may hold the card a new borrow waits for).
	adoptedGrace = 10 * time.Minute
	pollInterval = 2 * time.Second
)

// Leaser borrows cards from Slurm with placeholder jobs (engine.CardLeaser)
// and reports other jobs' cards as held outside the platform.
type Leaser struct {
	// cfgMu guards cfg and submitters: an entry's settings change while the
	// leaser runs (SetSettings), and a borrow waits for its job holding mu.
	cfgMu      sync.Mutex
	cfg        Config
	node       string          // cfg.Node, which never changes, readable without cfgMu
	submitters map[string]bool // accounts other than self whose placeholder jobs are this agent's: every one it submitted as
	// submittersWritten are the accounts the submitters file holds.
	submittersWritten map[string]bool
	root              bool // this agent may submit as another account
	run               Runner
	probe             func(context.Context) ([]proto.GPUInfo, error)
	procRoot          string
	log               *slog.Logger
	now               func() time.Time
	poll              time.Duration

	// self is the user the agent itself runs as (squeue's %u).
	self string

	mu sync.Mutex // serializes borrowing and sweeping; guards jobs
	// jobs are the placeholder jobs this agent knows, by job id.
	jobs map[string]*holdJob
	// startedAt is when this leaser first listed its jobs: jobs found then
	// were submitted before a restart (adopted).
	startedAt time.Time
	// leased: Lease was called. The engine leases cards for the instances
	// the server's state sync names, so from then on the cards in use are
	// known and an adopted job no longer needs the grace.
	leased bool

	// cardMu guards the card naming below. It is apart from mu so the
	// ledger keeps reporting while a borrow waits for its job to start.
	cardMu sync.Mutex
	// identAt is the card at each Slurm GPU position of this node.
	identAt map[int]string
	// uuidOfIdent is each card's UUID, which the holders map is keyed by.
	uuidOfIdent map[string]string

	// state is the node's last Slurm state (node_state.go).
	state nodeStateMemory

	detailMu sync.Mutex
	// details caches other jobs' cards: a running job's allocation does not
	// change, and the ledger reads the node every report round.
	details map[string]jobCards
}

type holdJob struct {
	id       string
	instance string
	cards    []string // card idents, set once the job runs
	running  bool
	idle     time.Time // when its cards were last seen unused; zero while used
	// adopted: the job was found running, submitted before this agent
	// started, so the instance on its cards may not be known yet.
	adopted bool
	// exclusive: the job was borrowed for an instance that takes whole
	// cards, so no other instance shares them; unknown (false) for a job
	// found at start.
	exclusive bool
	// waiting: a borrow waits for the job to start, without holding mu, so
	// the sweep leaves it alone (it is not "pending with nobody waiting").
	waiting bool
}

type jobCards struct {
	owner string
	cards []string
}

// New is a leaser for the node in cfg. probe lists the node's cards (the
// agent's prober); procRoot is where the host's /proc is ("/proc").
func New(cfg Config, run Runner, probe func(context.Context) ([]proto.GPUInfo, error), procRoot string, log *slog.Logger) *Leaser {
	if cfg.PendingTimeout <= 0 {
		cfg.PendingTimeout = DefaultPendingTimeout
	}
	if cfg.IdleReturn <= 0 {
		cfg.IdleReturn = DefaultIdleReturn
	}
	if run == nil {
		run = ExecRunner
	}
	if log == nil {
		log = slog.Default()
	}
	self := ""
	if u, err := user.Current(); err == nil {
		self = u.Username
	}
	l := &Leaser{
		cfg: cfg, node: cfg.Node, run: run, probe: probe, procRoot: procRoot, log: log, self: self,
		root: os.Geteuid() == 0,
		now:  time.Now, poll: pollInterval,
		jobs: map[string]*holdJob{}, details: map[string]jobCards{}, submitters: map[string]bool{}, submittersWritten: map[string]bool{},
	}
	l.resetOwnAccounts()
	return l
}

// Detect says whether this machine is a Slurm compute node whose GPUs Slurm
// allocates: scontrol knows the node and the node has gpu GRES. A node
// Slurm does not hand GPUs out on leaves the cards to the platform.
func Detect(ctx context.Context, run Runner, node string) (bool, error) {
	out, err := run(ctx, "scontrol", "-o", "show", "node", node)
	if err != nil {
		return false, err
	}
	f := fields(strings.TrimSpace(string(out)))
	if f["NodeName"] != node {
		return false, fmt.Errorf("scontrol does not know node %q", node)
	}
	return strings.Contains(f["Gres"], "gpu"), nil
}

// cardIdents refreshes the card at each Slurm GPU position.
func (l *Leaser) cardIdents(ctx context.Context) (map[int]string, error) {
	if l.identAt != nil {
		return l.identAt, nil
	}
	gpus, err := l.probe(ctx)
	if err != nil {
		return nil, err
	}
	byBDF := map[string]string{}
	uuids := map[string]string{}
	for _, g := range gpus {
		ident := accelerator.CardIdent(g.UUID, g.PCIeBDF)
		if bdf := accelerator.NormalizeBDF(g.PCIeBDF); bdf != "" {
			byBDF[bdf] = ident
		}
		uuids[ident] = g.UUID
	}
	at := map[int]string{}
	for pos, bdf := range l.positionBDFs(ctx) {
		if ident, ok := byBDF[bdf]; ok {
			at[pos] = ident
		}
	}
	if len(at) == 0 {
		return nil, errors.New("no Slurm GPU position of this node could be matched to a card")
	}
	l.identAt, l.uuidOfIdent = at, uuids
	return at, nil
}

// cardsAt names the cards at Slurm positions; an unknown position fails, as
// a card that cannot be named cannot be kept apart from the others.
func (l *Leaser) cardsAt(ctx context.Context, idx []int) ([]string, error) {
	l.cardMu.Lock()
	defer l.cardMu.Unlock()
	at, err := l.cardIdents(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(idx))
	for _, i := range idx {
		ident, ok := at[i]
		if !ok {
			l.identAt = nil // the cards may have changed; read them again next time
			return nil, fmt.Errorf("Slurm GPU %d of node %s matches no card of this node", i, l.node)
		}
		out = append(out, ident)
	}
	return out, nil
}

// nodeJob is one job squeue lists on this node.
type nodeJob struct {
	id, name, user, state string
}

// nodeJobs lists the jobs Slurm has on this node, and this agent's
// placeholder jobs wherever they are (a pending one is on no node yet).
func (l *Leaser) nodeJobs(ctx context.Context) ([]nodeJob, error) {
	out, err := l.run(ctx, "squeue", "-h", "-a", "-w", l.node, "-t", "CONFIGURING,RUNNING,COMPLETING", "-o", "%A|%j|%u|%T")
	if err != nil {
		return nil, err
	}
	return parseQueue(out), nil
}

func (l *Leaser) ownJobs(ctx context.Context) ([]nodeJob, error) {
	// The agent's own account alone: squeue --me. A root agent that submits as
	// another account lists the jobs of each account it has submitted as.
	// Every agent of a cluster submits as the same accounts, so a job is
	// this agent's only when it asked for this node (sbatch -w, squeue's
	// %n), pending or running; another node's agent returns its own.
	who := []string{"--me"}
	if accounts := l.ownAccounts(); len(accounts) > 1 {
		who = []string{"-u", strings.Join(accounts, ",")}
	}
	args := append([]string{"-h"}, who...)
	out, err := l.run(ctx, "squeue", append(args, "-o", "%A|%j|%u|%T|%n")...)
	if err != nil {
		return nil, err
	}
	var own []nodeJob
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.Split(strings.TrimSpace(line), "|")
		if len(parts) != 5 || parts[0] == "" || !strings.HasPrefix(parts[1], HoldPrefix) || parts[4] != l.node {
			continue
		}
		own = append(own, nodeJob{id: parts[0], name: parts[1], user: parts[2], state: parts[3]})
	}
	return own, nil
}

func parseQueue(out []byte) []nodeJob {
	var jobs []nodeJob
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.Split(strings.TrimSpace(line), "|")
		if len(parts) != 4 || parts[0] == "" {
			continue
		}
		jobs = append(jobs, nodeJob{id: parts[0], name: parts[1], user: parts[2], state: parts[3]})
	}
	return jobs
}

// jobRecord is one job's `scontrol -d -o show job` line.
func (l *Leaser) jobRecord(ctx context.Context, id string) (string, error) {
	out, err := l.run(ctx, "scontrol", "-d", "-o", "show", "job", id)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// jobCardsOnNode is the cards a job holds on this node.
func (l *Leaser) jobCardsOnNode(ctx context.Context, record string) ([]string, error) {
	idx, err := gpuIDX(nodeGRES(record, l.node))
	if err != nil {
		return nil, err
	}
	return l.cardsAt(ctx, idx)
}

// Lease borrows the cards an instance needs (engine.CardLeaser). Cards this
// agent already holds for the instance are used as they are, and so are
// cards held for another instance when both share them (dGPU, shared); the
// rest are asked for in one placeholder job of that many cards, and the
// cards Slurm gives take the places of the ones placement chose. An
// instance that takes whole cards never runs on another instance's cards:
// Slurm may have given those to the other one in place of the cards
// placement chose for it, and two instances would end up on one card.
func (l *Leaser) Lease(ctx context.Context, instance string, want []string, shared bool, progress func(string)) ([]string, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.leased = true
	if err := l.refreshOwnLocked(ctx); err != nil {
		return nil, false, fmt.Errorf("list this agent's Slurm jobs: %w", err)
	}
	held := map[string]bool{}
	var mine *holdJob
	for _, j := range l.jobs {
		if !j.running {
			continue
		}
		if j.instance == instance {
			mine = j
		}
		if j.instance == instance || (shared && !j.exclusive) {
			for _, c := range j.cards {
				held[c] = true
			}
		}
	}
	if mine != nil && len(mine.cards) == len(want) {
		// The instance's own job is what its container runs on. The spec
		// may still name the cards placement chose: the server learns of
		// the rebinding from the report that follows, and a reconnect can
		// replay the old spec before it has.
		return inOrderOf(mine.cards, want), false, nil
	}
	out := make([]string, len(want))
	missing := 0
	for i, c := range want {
		if held[c] {
			out[i] = c
		} else {
			missing++
		}
	}
	if missing == 0 {
		return out, false, nil
	}
	if mine != nil && !overlaps(mine.cards, want) {
		// The instance's own job holds cards it does not want any more
		// (it was placed again); swap it for one of the right size.
		l.cancelLocked(ctx, mine.id, "placed again")
	}
	job, err := l.borrowLocked(ctx, instance, missing, progress)
	if err != nil {
		return nil, false, err
	}
	job.exclusive = !shared
	next := 0
	for i := range out {
		if out[i] == "" {
			out[i] = job.cards[next]
			next++
		}
	}
	return out, true, nil
}

// inOrderOf is cards with each card that want names in want's position,
// the others filling the remaining positions in order.
func inOrderOf(cards, want []string) []string {
	out := make([]string, len(want))
	used := map[string]bool{}
	for i, c := range want {
		if slices.Contains(cards, c) {
			out[i], used[c] = c, true
		}
	}
	next := 0
	for i := range out {
		if out[i] != "" {
			continue
		}
		for used[cards[next]] {
			next++
		}
		out[i], used[cards[next]] = cards[next], true
	}
	return out
}

// borrowLocked submits a placeholder job for n cards and waits for it. It is
// called with mu held and holds it again when it returns, but lets it go
// while Slurm is asked and while it waits: the cards a pending job waits for
// may be the ones an idle job of this agent holds, which only the sweep
// returns (seen on E-HPC, 2026-11-18: a retried instance waited the whole
// pending timeout for its own agent's idle job).
func (l *Leaser) borrowLocked(ctx context.Context, instance string, n int, progress func(string)) (*holdJob, error) {
	cfg := l.settings()
	args := []string{"--parsable", "-J", HoldPrefix + instance, "-w", cfg.Node, "-N", "1", "-n", "1",
		"--gres=gpu:" + strconv.Itoa(n), "-o", "/dev/null", "-e", "/dev/null"}
	l.cfgMu.Lock()
	submitAs := l.submitAs()
	l.cfgMu.Unlock()
	if submitAs != "" {
		args = append(args, "--uid="+submitAs)
	}
	if !hasFlag(cfg.SbatchArgs, "-c", "--cpus-per-task") {
		args = append(args, "-c", "1")
	}
	if !hasFlag(cfg.SbatchArgs, "--mem", "--mem-per-cpu", "--mem-per-gpu") {
		args = append(args, "--mem=1G")
	}
	if !hasFlag(cfg.SbatchArgs, "--requeue", "--no-requeue") {
		// A placeholder job lives and dies with its instance's start: one
		// Slurm requeued after its node failed would pend for that node with
		// no agent left to cancel it (seen on E-HPC when a spot node was
		// reclaimed), and start on whichever node takes its name.
		args = append(args, "--no-requeue")
	}
	if !hasFlag(cfg.SbatchArgs, "-p", "--partition") {
		// A job goes to the default partition unless it names one, and the
		// node is often not in it (E-HPC puts each queue in a partition of
		// its own and defaults to "comp"): the job would pend on
		// PartitionConfig forever. The node's own partitions always fit.
		if parts, err := l.nodePartitions(ctx); err == nil && parts != "" {
			args = append(args, "-p", parts)
		}
	}
	args = append(args, cfg.SbatchArgs...)
	args = append(args, "--wrap", "exec sleep infinity")
	out, err := l.run(ctx, "sbatch", args...)
	if err != nil {
		return nil, fmt.Errorf("submit the Slurm job for %d card(s): %w", n, err)
	}
	id, _, _ := strings.Cut(strings.TrimSpace(string(out)), ";") // --parsable: "id[;cluster]"
	if id == "" {
		return nil, errors.New("sbatch printed no job id")
	}
	job := &holdJob{id: id, instance: instance, waiting: true}
	l.jobs[id] = job
	defer func() { job.waiting = false }()
	l.log.Info("submitted Slurm placeholder job", "job", id, "instance", instance, "gpus", n)
	deadline := l.now().Add(cfg.PendingTimeout)
	lastReason := ""
	for {
		l.mu.Unlock()
		record, err := l.jobRecord(ctx, id)
		l.mu.Lock()
		if err == nil {
			f := fields(record)
			switch state := f["JobState"]; state {
			case "RUNNING":
				l.mu.Unlock()
				cards, err := l.jobCardsOnNode(ctx, record)
				l.mu.Lock()
				if err == nil && len(cards) != n {
					err = fmt.Errorf("Slurm job %s holds %d card(s) on %s, %d were asked for", id, len(cards), l.node, n)
				}
				if err != nil {
					l.cancelLocked(ctx, id, "cards not identified")
					return nil, err
				}
				job.cards, job.running = cards, true
				return job, nil
			case "PENDING", "CONFIGURING":
				if reason := f["Reason"]; reason != lastReason && progress != nil {
					lastReason = reason
					progress(fmt.Sprintf("waiting for Slurm job %s: %s", id, reason))
				}
			default:
				delete(l.jobs, id)
				return nil, fmt.Errorf("Slurm job %s ended before it started (%s, %s)", id, state, f["Reason"])
			}
		}
		if l.now().After(deadline) {
			l.cancelLocked(ctx, id, "pending timeout")
			return nil, fmt.Errorf("Slurm did not start job %s within %s (%s)", id, cfg.PendingTimeout, lastReason)
		}
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			l.mu.Lock()
			l.cancelLocked(context.Background(), id, "start cancelled")
			return nil, ctx.Err()
		case <-time.After(l.poll):
		}
		l.mu.Lock()
	}
}

// nodePartitions is the partitions the node is in, as scontrol lists them
// ("gpu" or "gpu,debug").
func (l *Leaser) nodePartitions(ctx context.Context) (string, error) {
	out, err := l.run(ctx, "scontrol", "-o", "show", "node", l.node)
	if err != nil {
		return "", err
	}
	parts := fields(strings.TrimSpace(string(out)))["Partitions"]
	if parts == "(null)" {
		return "", nil
	}
	return parts, nil
}

func hasFlag(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a == n || strings.HasPrefix(a, n+"=") {
				return true
			}
		}
	}
	return false
}

// refreshOwnLocked learns the placeholder jobs Slurm has for this agent,
// including ones submitted before a restart, and forgets those that ended.
func (l *Leaser) refreshOwnLocked(ctx context.Context) error {
	own, err := l.ownJobs(ctx)
	if err != nil {
		return err
	}
	starting := l.startedAt.IsZero()
	if starting {
		l.startedAt = l.now()
	}
	live := map[string]bool{}
	for _, j := range own {
		live[j.id] = true
		known, ok := l.jobs[j.id]
		if !ok {
			known = &holdJob{id: j.id, instance: strings.TrimPrefix(j.name, HoldPrefix), adopted: starting}
			l.jobs[j.id] = known
		}
		if j.state == "RUNNING" && !known.running {
			record, err := l.jobRecord(ctx, j.id)
			if err != nil {
				return err
			}
			cards, err := l.jobCardsOnNode(ctx, record)
			if err != nil {
				return err
			}
			known.cards, known.running = cards, true
		}
	}
	for id := range l.jobs {
		if !live[id] {
			delete(l.jobs, id)
		}
	}
	return nil
}

func (l *Leaser) cancelLocked(ctx context.Context, id, why string) {
	if _, err := l.run(ctx, "scancel", id); err != nil {
		l.log.Warn("could not cancel Slurm placeholder job", "job", id, "reason", why, "err", err)
		return
	}
	delete(l.jobs, id)
	l.log.Info("returned Slurm placeholder job", "job", id, "reason", why)
}
