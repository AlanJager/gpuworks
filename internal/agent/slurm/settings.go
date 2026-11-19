package slurm

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// What a Slurm cluster entry hands an agent (Enroll, Reporter) and the
// leaser's use of it: settings that change while the leaser runs, and the
// accounts whose placeholder jobs are the agent's own.

// config is base with the settings applied; a timeout of zero is no setting
// and leaves the one base has.
func (s Settings) config(base Config) Config {
	base.SbatchArgs = strings.Fields(s.SbatchArgs)
	if s.PendingTimeoutSeconds > 0 {
		base.PendingTimeout = time.Duration(s.PendingTimeoutSeconds) * time.Second
	}
	if s.IdleReturnSeconds > 0 {
		base.IdleReturn = time.Duration(s.IdleReturnSeconds) * time.Second
	}
	base.AccountUser = strings.TrimSpace(s.AccountUser)
	return base
}

// ConfigWith is base with the entry's settings applied: the sbatch arguments,
// the pending timeout, the idle return and the accounting user.
func ConfigWith(base Config, s Settings) Config { return s.config(base) }

// resetOwnAccounts starts the accounts other than the agent's own whose
// placeholder jobs are its own with the ones it submitted as before (the
// submitters file) and the one it is configured to submit as.
func (l *Leaser) resetOwnAccounts() {
	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	l.submitters = map[string]bool{}
	for _, u := range l.readSubmitters() {
		l.submitters[u] = true
	}
	if u := l.submitAs(); u != "" {
		l.rememberSubmitterLocked(u)
	}
}

// submitterName is what an account the agent submitted as looks like: a login
// name, as the entry validates it.
var submitterName = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)

// readSubmitters reads the accounts the agent submitted as before it
// restarted; a file that is missing or holds anything else gives none.
func (l *Leaser) readSubmitters() []string {
	if l.cfg.SubmittersFile == "" {
		return nil
	}
	data, err := os.ReadFile(l.cfg.SubmittersFile)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); submitterName.MatchString(line) {
			out = append(out, line)
		}
	}
	return out
}

// rememberSubmitterLocked makes u an account whose placeholder jobs are the
// agent's own, for good: it is written to the submitters file, replaced as a
// whole, before a job is submitted as it. Callers hold cfgMu.
func (l *Leaser) rememberSubmitterLocked(u string) {
	if l.submitters[u] && l.submittersWritten[u] {
		return
	}
	l.submitters[u] = true
	if l.cfg.SubmittersFile == "" {
		return
	}
	accounts := make([]string, 0, len(l.submitters))
	for a := range l.submitters {
		accounts = append(accounts, a)
	}
	slices.Sort(accounts)
	tmp := l.cfg.SubmittersFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(accounts, "\n")+"\n"), 0o644); err == nil {
		err = os.Rename(tmp, l.cfg.SubmittersFile)
		if err == nil {
			for _, a := range accounts {
				l.submittersWritten[a] = true
			}
			return
		}
		_ = os.Remove(tmp)
	}
	l.log.Warn("could not record the accounts placeholder jobs are submitted as; a restart may not find jobs of an earlier account", "file", l.cfg.SubmittersFile)
}

// submitAs is the account the next placeholder job is submitted as, or ""
// for the agent's own: the configured account, when this agent is root and
// the account is another one. Callers hold cfgMu or are sole owners.
func (l *Leaser) submitAs() string {
	if l.root && l.cfg.AccountUser != "" && l.cfg.AccountUser != l.self {
		return l.cfg.AccountUser
	}
	return ""
}

// settings is a snapshot of the configuration a borrow or a sweep reads.
func (l *Leaser) settings() Config {
	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	cfg := l.cfg
	cfg.SbatchArgs = append([]string(nil), cfg.SbatchArgs...)
	return cfg
}

// SetSettings applies an edit of the cluster entry's settings to jobs
// submitted from now on, and to the idle return and pending timeout of those
// running. Jobs already submitted keep the account they were submitted as,
// and stay this agent's.
func (l *Leaser) SetSettings(s Settings) {
	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	next := s.config(l.cfg)
	if next.PendingTimeout <= 0 {
		next.PendingTimeout = defaultPendingTimeout
	}
	if next.IdleReturn <= 0 {
		next.IdleReturn = defaultIdleReturn
	}
	l.cfg = next
	if u := l.submitAs(); u != "" {
		l.rememberSubmitterLocked(u)
	}
}

// ownUser says whether a job of this account is a placeholder job of this
// agent's when its name says so.
func (l *Leaser) ownUser(account string) bool {
	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	return account == l.self || l.submitters[account]
}

// ownAccounts are the accounts squeue is asked for this agent's jobs of: the
// agent's own and every other one it submitted as, sorted.
func (l *Leaser) ownAccounts() []string {
	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	out := []string{l.self}
	for u := range l.submitters {
		out = append(out, u)
	}
	slices.Sort(out)
	return out
}
