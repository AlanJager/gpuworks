package launchd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Host is what installing the job touches: launchctl, the plist files and the
// accounts. NewHost is the real Mac; tests give a fake launchctl and a root
// directory the plists are written under.
type Host struct {
	// Launchctl runs launchctl with args and returns its combined output.
	Launchctl func(ctx context.Context, args ...string) (string, error)
	// Root prefixes every plist path; "" on a real Mac.
	Root string
	// Admin is true when running as root: the job is then a launch daemon.
	Admin bool
	// Self is the account running the install, and the one a launch agent
	// belongs to.
	Self Account
	// Lookup finds the account a launch daemon runs as.
	Lookup func(name string) (Account, error)
	// Chown changes a path's owner (os.Lchown).
	Chown func(path string, uid, gid int) error
	// Out takes the install's messages to the person running it.
	Out io.Writer
	// Wait is how long launchd gets to let go of an old job and to keep the
	// new one running; Tick is how often it is asked. Stable is how long the
	// new agent must keep one pid to count as running: longer than the plist's
	// ThrottleInterval, so a crash loop is not mistaken for it.
	Wait, Tick, Stable time.Duration
}

// Account is a macOS user.
type Account struct {
	Name     string
	UID, GID int
	Home     string
}

// ErrNoSession reports a launch agent that cannot start: its user has no
// login session on the Mac.
var ErrNoSession = errors.New("no login session")

type target struct {
	domain string // system or gui/<uid>
	plist  string // without Root
	owner  Account
}

func (h *Host) target(job Job) (target, error) {
	if !h.Admin {
		return target{domain: "gui/" + strconv.Itoa(h.Self.UID), plist: AgentPlist(h.Self.Home), owner: h.Self}, nil
	}
	if job.User == "" || job.User == "root" {
		// The agent starts engines and downloads models; the Metal GPU needs
		// no privileges, so it runs as the person who installed it.
		return target{}, fmt.Errorf("the agent never runs as root on a Mac: install from the account it should run as (with sudo)")
	}
	owner, err := h.Lookup(job.User)
	if err != nil {
		return target{}, fmt.Errorf("look up %s: %w", job.User, err)
	}
	return target{domain: "system", plist: DaemonPlist(), owner: owner}, nil
}

func (h *Host) path(p string) string { return filepath.Join(h.Root, p) }

func (h *Host) say(format string, args ...any) {
	if h.Out != nil {
		fmt.Fprintf(h.Out, "gpuworks: "+format+"\n", args...)
	}
}

// Install registers job with launchd and waits until launchd keeps it
// running. It replaces the job an earlier install registered, for this server
// or another (and says which). When launchd refuses the new job, the previous
// plist is put back and started, so a failed reinstall does not leave the Mac
// without an agent.
func (h *Host) Install(ctx context.Context, job Job) (pid int, err error) {
	t, err := h.target(job)
	if err != nil {
		return 0, err
	}
	if !h.Admin {
		job.User = ""
		if _, err := h.Launchctl(ctx, "print", t.domain); err != nil {
			return 0, fmt.Errorf("%w: %s is not logged in on this Mac, so a launch agent cannot start; log in on the Mac, or install with administrator rights to run the agent from boot", ErrNoSession, t.owner.Name)
		}
		if _, err := os.Stat(h.path(DaemonPlist())); err == nil {
			// Beside it a launch agent would be a second agent on the same GPU.
			return 0, fmt.Errorf("a gpuworks launch daemon is installed on this Mac, and replacing it needs administrator rights: run the install with sudo, or remove it first (sudo gpuworks-agent uninstall)")
		}
	}
	plist := h.path(t.plist)
	previous, _ := os.ReadFile(plist)
	if old, err := ReadJob(previous); err == nil && trimServer(old.Server) != "" && trimServer(old.Server) != trimServer(job.Server) {
		h.say("WARNING: replacing the gpuworks agent installed for another server (%s); that server loses this Mac", old.Server)
	}
	if h.Admin {
		// The label lives in one domain at a time: a daemon replaces the
		// launch agent an install without administrator rights left.
		h.removeAgentOf(ctx, t.owner)
		for _, dir := range []string{filepath.Dir(job.Binary), job.StateDir} {
			if err := h.adopt(dir, t.owner); err != nil {
				return 0, err
			}
		}
	}
	if err := h.writePlist(plist, job.Plist()); err != nil {
		return 0, err
	}
	if err := h.load(ctx, t); err != nil {
		if len(previous) == 0 {
			return 0, err
		}
		if restoreErr := h.writePlist(plist, previous); restoreErr == nil && h.load(ctx, t) == nil {
			return 0, fmt.Errorf("%w; the previous agent job was restored", err)
		}
		return 0, fmt.Errorf("%w; the previous agent job could not be restored either", err)
	}
	return h.awaitRunning(ctx, t, job.StateDir)
}

// load replaces whatever runs under the label with the plist's job.
func (h *Host) load(ctx context.Context, t target) error {
	service := t.domain + "/" + AgentLabel
	_, _ = h.Launchctl(ctx, "bootout", service)
	// bootout returns before launchd has let go of a running job, and a
	// bootstrap before then fails with "5: Input/output error".
	for deadline := time.Now().Add(h.Wait); time.Now().Before(deadline); {
		if _, err := h.Launchctl(ctx, "print", service); err != nil {
			break
		}
		if err := h.sleep(ctx); err != nil {
			return err
		}
	}
	var out string
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if out, err = h.Launchctl(ctx, "bootstrap", t.domain, h.path(t.plist)); err == nil {
			return nil
		}
		if err := h.sleep(ctx); err != nil {
			return err
		}
	}
	return fmt.Errorf("launchd refused %s: %s", t.plist, strings.TrimSpace(out))
}

var (
	printState = regexp.MustCompile(`(?m)^\s*state = (\S+)`)
	printPID   = regexp.MustCompile(`(?m)^\s*pid = (\d+)`)
)

// RunningPID reads the output of `launchctl print <domain>/<label>`: the pid
// of a job launchd reports running, or 0.
func RunningPID(out string) int {
	state := printState.FindStringSubmatch(out)
	pid := printPID.FindStringSubmatch(out)
	if state == nil || state[1] != "running" || pid == nil {
		return 0
	}
	n, _ := strconv.Atoi(pid[1])
	return n
}

// awaitRunning waits for launchd to report the job running under one pid
// for Stable: KeepAlive restarts an agent that exits at once, which a single
// look would take for a running one.
func (h *Host) awaitRunning(ctx context.Context, t target, stateDir string) (int, error) {
	service := t.domain + "/" + AgentLabel
	last, since := 0, time.Time{}
	for deadline := time.Now().Add(h.Wait + h.Stable); time.Now().Before(deadline); {
		out, err := h.Launchctl(ctx, "print", service)
		pid := 0
		if err == nil {
			pid = RunningPID(out)
		}
		if pid != last {
			last, since = pid, time.Now()
		}
		if pid > 0 && time.Since(since) >= h.Stable {
			return pid, nil
		}
		if err := h.sleep(ctx); err != nil {
			return 0, err
		}
	}
	stderrLog := filepath.Join(stateDir, StderrLog)
	detail := ""
	if data, err := os.ReadFile(stderrLog); err == nil {
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) > 3 {
			lines = lines[len(lines)-3:]
		}
		detail = ": " + strings.Join(lines, " | ")
	}
	return 0, fmt.Errorf("launchd does not keep the agent running; see %s%s", stderrLog, detail)
}

func (h *Host) sleep(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(h.Tick):
		return nil
	}
}

// removeAgentOf removes the launch agent of owner's session, if any.
func (h *Host) removeAgentOf(ctx context.Context, owner Account) {
	plist := h.path(AgentPlist(owner.Home))
	if _, err := os.Stat(plist); err != nil {
		return
	}
	_, _ = h.Launchctl(ctx, "bootout", "gui/"+strconv.Itoa(owner.UID)+"/"+AgentLabel)
	if err := os.Remove(plist); err == nil {
		h.say("removed %s's launch agent: the launch daemon replaces it", owner.Name)
	}
}

// writePlist puts data in place atomically. The plist carries the node token,
// so it is readable by its owner only; a daemon's belongs to root, as launchd
// requires.
func (h *Host) writePlist(plist string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(plist), "."+AgentLabel+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), plist)
}

// adopt gives owner what a previous install left in dir under another account
// (root, or the user of an earlier install): the agent must be able to write
// its state and models. Only a directory not already owner's is walked.
func (h *Host) adopt(dir string, owner Account) error {
	if dir == "" {
		return nil
	}
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if uid, ok := fileUID(info); ok && uid == owner.UID {
		return nil
	}
	return filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := h.Chown(p, owner.UID, owner.GID); err != nil {
			return fmt.Errorf("give %s to %s: %w", p, owner.Name, err)
		}
		return nil
	})
}

// Uninstall removes the job from both domains it may be in: the launch daemon
// and the launch agent of the user (the one sudo was run from, as root). It
// returns the state directories the removed jobs named. Engines the agent
// started are process groups of their own and keep running.
func (h *Host) Uninstall(ctx context.Context) ([]string, error) {
	type job struct {
		domain, plist string
	}
	jobs := []job{{"system", DaemonPlist()}, {"gui/" + strconv.Itoa(h.Self.UID), AgentPlist(h.Self.Home)}}
	var stateDirs []string
	var errs []error
	for _, j := range jobs {
		plist := h.path(j.plist)
		data, err := os.ReadFile(plist)
		if os.IsNotExist(err) {
			continue
		}
		if j.domain == "system" && !h.Admin {
			errs = append(errs, fmt.Errorf("removing the launch daemon needs administrator rights: sudo gpuworks-agent uninstall"))
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if parsed, err := ReadJob(data); err == nil && parsed.StateDir != "" {
			stateDirs = append(stateDirs, parsed.StateDir)
		}
		// A job that is not loaded (or a session that is gone) has nothing to
		// stop; once its plist is removed launchd will not load it again.
		if out, err := h.Launchctl(ctx, "bootout", j.domain+"/"+AgentLabel); err != nil && strings.Contains(out, "not permitted") {
			errs = append(errs, fmt.Errorf("launchctl bootout %s/%s: %s", j.domain, AgentLabel, strings.TrimSpace(out)))
			continue
		}
		if err := os.Remove(plist); err != nil {
			errs = append(errs, err)
			continue
		}
		h.say("removed %s", j.plist)
	}
	return stateDirs, errors.Join(errs...)
}
