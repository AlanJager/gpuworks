package slurm

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A root agent that is told an account submits its placeholder jobs as it
// (squeue and accounting show that account), and still finds, reports around
// and returns them as its own: after a restart too.
func TestLeaseSubmitsAsTheAccountUserAndStillOwnsTheJob(t *testing.T) {
	f := newFakeSlurm()
	l, clock := newTestLeaser(t, f, Config{AccountUser: "svc", IdleReturn: time.Minute})
	l.root = true
	l.resetOwnAccounts()
	held, _, err := l.Lease(context.Background(), "inst-a", []string{card0}, false, nil)
	if err != nil || len(held) != 1 {
		t.Fatalf("Lease = %v, %v", held, err)
	}
	if submit := f.lastCall("sbatch"); !strings.Contains(submit, "--uid=svc") {
		t.Fatalf("sbatch call %q does not submit as the account", submit)
	}
	var job *fakeJob
	for _, j := range f.jobs {
		job = j
	}
	if job.user != "svc" {
		t.Fatalf("the job belongs to %q, want svc", job.user)
	}
	// Other jobs' cards are the ledger's; the agent's own job is not one of them.
	holders, err := l.ExternalCardHolders(context.Background())
	if err != nil || len(holders) != 0 {
		t.Fatalf("holders = %v, %v; the placeholder job is the agent's own", holders, err)
	}
	if got := f.lastCall("squeue -h -u"); !strings.HasPrefix(got, "squeue -h -u root,svc ") {
		t.Fatalf("own jobs were asked for as %q", got)
	}
	// A restarted agent (a new leaser) finds the job again by its name and account.
	l2, _ := newTestLeaser(t, f, Config{AccountUser: "svc", IdleReturn: time.Minute})
	l2.root = true
	l2.resetOwnAccounts()
	if err := l2.Sweep(context.Background(), map[string][]string{"inst-a": held}, func(string, string) { t.Fatal("the job was reported lost") }); err != nil {
		t.Fatal(err)
	}
	if got := f.called("scancel"); got != 0 {
		t.Fatalf("a restarted agent cancelled its own job (%d scancel calls)", got)
	}
	// Unused for the idle return, the job goes.
	for i := 0; i < 2; i++ {
		if err := l.Sweep(context.Background(), map[string][]string{}, func(string, string) {}); err != nil {
			t.Fatal(err)
		}
		clock.advance(2 * time.Minute)
	}
	if got := f.called("scancel"); got == 0 {
		t.Fatal("the idle placeholder job of the account was not returned")
	}
}

// Only root can submit as another account: any other agent, and one told its
// own account, submits as itself.
func TestLeaseSubmitsAsTheAgentUnlessItIsRootWithAnotherAccount(t *testing.T) {
	for _, tc := range []struct {
		name    string
		root    bool
		account string
		self    string
		want    bool
	}{
		{"root with another account", true, "svc", "root", true},
		{"not root", false, "svc", "gpusvc", false},
		{"its own account", true, "root", "root", false},
		{"no account", true, "", "root", false},
	} {
		f := newFakeSlurm()
		f.self = tc.self
		l, _ := newTestLeaser(t, f, Config{AccountUser: tc.account})
		l.root = tc.root
		l.resetOwnAccounts()
		if _, _, err := l.Lease(context.Background(), "inst-a", []string{card0}, false, nil); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := strings.Contains(f.lastCall("sbatch"), "--uid="); got != tc.want {
			t.Errorf("%s: --uid given = %v, want %v (%s)", tc.name, got, tc.want, f.lastCall("sbatch"))
		}
		if got := f.lastCall("squeue -h"); !tc.want && !strings.Contains(got, "--me") {
			t.Errorf("%s: own jobs asked for as %q, want --me", tc.name, got)
		}
	}
}

// An edit of the entry's settings reaches the next job, and a job submitted
// as the old account stays the agent's own.
func TestSetSettingsAppliesToTheNextJobAndKeepsEarlierOnesOwn(t *testing.T) {
	f := newFakeSlurm()
	l, _ := newTestLeaser(t, f, Config{SbatchArgs: []string{"-A", "old"}})
	l.root = true
	l.resetOwnAccounts()
	if _, _, err := l.Lease(context.Background(), "inst-a", []string{card0}, false, nil); err != nil {
		t.Fatal(err)
	}
	if submit := f.lastCall("sbatch"); !strings.Contains(submit, "-A old") || strings.Contains(submit, "--uid=") {
		t.Fatalf("first job %q", submit)
	}
	l.SetSettings(Settings{SbatchArgs: "-A new --time=1-0", IdleReturnSeconds: 300, PendingTimeoutSeconds: 90, AccountUser: "svc"})
	if cfg := l.settings(); !slices.Equal(cfg.SbatchArgs, []string{"-A", "new", "--time=1-0"}) || cfg.IdleReturn != 300*time.Second || cfg.PendingTimeout != 90*time.Second || cfg.Node != f.node {
		t.Fatalf("settings = %+v", cfg)
	}
	if _, _, err := l.Lease(context.Background(), "inst-b", []string{card1}, false, nil); err != nil {
		t.Fatal(err)
	}
	if submit := f.lastCall("sbatch"); !strings.Contains(submit, "-A new --time=1-0") || strings.Contains(submit, "-A old") || !strings.Contains(submit, "--uid=svc") {
		t.Fatalf("second job %q", submit)
	}
	// The job submitted as root and the one submitted as svc are both ours.
	if err := l.Sweep(context.Background(), map[string][]string{"inst-a": {card0}, "inst-b": {card1}}, func(i, m string) { t.Fatalf("lost %s: %s", i, m) }); err != nil {
		t.Fatal(err)
	}
	if got := f.called("scancel"); got != 0 {
		t.Fatalf("an edit made the agent cancel its own jobs (%d)", got)
	}
	// A zero timeout is no setting: an entry never turns the timeouts off, so
	// the agent keeps the ones it has; arguments and account do clear.
	l.SetSettings(Settings{})
	if cfg := l.settings(); cfg.PendingTimeout != 90*time.Second || cfg.IdleReturn != 300*time.Second || len(cfg.SbatchArgs) != 0 || cfg.AccountUser != "" {
		t.Fatalf("empty settings = %+v", cfg)
	}
}

// The health check writes `slurmd -G` for an agent that cannot run it: the
// positions come from that file, and an unusable file falls through to slurmd.
func TestPositionsReadTheOrderFileFirst(t *testing.T) {
	order := "slurmd: Gres Name=gpu Type=t4 Count=1 Index=1 ID=7696487 File=/dev/nvidia1 Links=(null) Flags=HAS_FILE\n" +
		"slurmd: Gres Name=gpu Type=t4 Count=1 Index=0 ID=7696487 File=/dev/nvidia0 Links=(null) Flags=HAS_FILE\n"
	file := filepath.Join(t.TempDir(), "slurm-gres-devices")
	if err := os.WriteFile(file, []byte(order), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newFakeSlurm() // slurmd itself would give no list: the file is the only source
	f.add("20", "holder", "alice", 0)
	l, _ := newTestLeaser(t, f, Config{OrderFile: file})
	holders, err := l.ExternalCardHolders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(holders["GPU-bbbb"]) != 1 {
		t.Fatalf("holders = %+v; want Slurm position 0 to be /dev/nvidia1 (GPU-bbbb) from the file", holders)
	}
	if got := f.called("sh"); got != 0 {
		t.Fatalf("slurmd was asked %d time(s) although the file had the list", got)
	}

	// A file with no device list (the health check never writes one, but a
	// half-written or foreign file must not decide) is ignored.
	for name, body := range map[string]string{"error text": "slurmd: error: Invalid user id\n", "empty": "", "missing": ""} {
		path := file
		if name == "missing" {
			path = filepath.Join(t.TempDir(), "absent")
		} else if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		f := newFakeSlurm()
		f.slurmdG = order
		f.add("20", "holder", "alice", 0)
		l, _ := newTestLeaser(t, f, Config{OrderFile: path})
		holders, err := l.ExternalCardHolders(context.Background())
		if err != nil || len(holders["GPU-bbbb"]) != 1 {
			t.Fatalf("%s: holders = %+v, %v; want slurmd's list to decide", name, holders, err)
		}
		if f.called("sh") == 0 {
			t.Fatalf("%s: slurmd was not asked", name)
		}
	}
}

func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// A job submitted as an account the entry has since changed is still the
// agent's own after the agent restarts: the accounts it submitted as are kept
// in its state directory. Without that file the old job would be read as
// another user's, hold its card for good and be neither refreshed nor returned.
func TestRestartedAgentStillOwnsJobsOfAnEarlierAccountUser(t *testing.T) {
	file := filepath.Join(t.TempDir(), "slurm-submitters")
	f := newFakeSlurm()
	l, _ := newTestLeaser(t, f, Config{AccountUser: "svc-old", IdleReturn: time.Minute, SubmittersFile: file})
	l.root = true
	l.resetOwnAccounts()
	held, _, err := l.Lease(context.Background(), "inst-a", []string{card0}, false, nil)
	if err != nil || len(held) != 1 {
		t.Fatalf("Lease = %v, %v", held, err)
	}
	if got, _ := os.ReadFile(file); string(got) != "svc-old\n" {
		t.Fatalf("submitters file = %q", got)
	}
	// The entry was edited while the agent was down: the restarted agent
	// submits as svc-new and has to know svc-old's job is its own.
	l2, _ := newTestLeaser(t, f, Config{AccountUser: "svc-new", IdleReturn: time.Minute, SubmittersFile: file})
	l2.root = true
	l2.resetOwnAccounts()
	if !l2.ownUser("svc-old") || !l2.ownUser("svc-new") || l2.ownUser("someone") {
		t.Fatalf("own accounts = %v", l2.ownAccounts())
	}
	if got := l2.ownAccounts(); len(got) != 3 { // the agent's own, svc-new and svc-old
		t.Fatalf("own accounts = %v", got)
	}
	holders, err := l2.ExternalCardHolders(context.Background())
	if err != nil || len(holders) != 0 {
		t.Fatalf("the earlier account's job is another user's: %v, %v", holders, err)
	}
	if err := l2.Sweep(context.Background(), map[string][]string{"inst-a": held}, func(string, string) { t.Fatal("the job was reported lost") }); err != nil {
		t.Fatal(err)
	}
	if got := f.called("scancel"); got != 0 {
		t.Fatalf("the earlier job was cancelled (%d)", got)
	}
	if got, _ := os.ReadFile(file); string(got) != "svc-new\nsvc-old\n" {
		t.Fatalf("submitters file after the new account = %q", got)
	}
	// Without the file the same restart does not know the old account.
	l3, _ := newTestLeaser(t, f, Config{AccountUser: "svc-new", IdleReturn: time.Minute})
	l3.root = true
	l3.resetOwnAccounts()
	if l3.ownUser("svc-old") {
		t.Fatal("an agent with no submitters file knows an earlier account")
	}
}

// What a submitters file holds is only ever login names: anything else in it
// is not an account to trust.
func TestSubmittersFileOnlyHoldsLoginNames(t *testing.T) {
	file := filepath.Join(t.TempDir(), "slurm-submitters")
	if err := os.WriteFile(file, []byte("svc\n\nBad User\n-x\nroot;rm\nok_1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, _ := newTestLeaser(t, newFakeSlurm(), Config{SubmittersFile: file})
	l.resetOwnAccounts()
	if !l.ownUser("svc") || !l.ownUser("ok_1") || l.ownUser("Bad User") || l.ownUser("-x") || l.ownUser("root;rm") {
		t.Fatalf("own accounts = %v", l.ownAccounts())
	}
	// A missing or unwritable file costs nothing but the memory.
	l2, _ := newTestLeaser(t, newFakeSlurm(), Config{AccountUser: "svc", SubmittersFile: filepath.Join(t.TempDir(), "no", "such", "dir", "f")})
	l2.root = true
	l2.resetOwnAccounts()
	if !l2.ownUser("svc") {
		t.Fatal("an unwritable submitters file lost the current account")
	}
}
