package provision

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInstallDirUntraversable: a directory above the install directory is
// closed to the dedicated runtime user, and the agent will run as root with
// the runveil runtime and that user, so no workload could ever launch there.
// The install stops on it, as it does on a noexec directory. Errors of this
// kind are *InstallDirUntraversableError.
var ErrInstallDirUntraversable = errors.New("the dedicated runtime user cannot reach the install directory")

// InstallDirUntraversableError is the install stopped by a closed ancestor.
// It carries the facts the API turns into a stable, localizable error code.
type InstallDirUntraversableError struct {
	// InstallDir is the directory the agent would have been installed into.
	InstallDir string
	// Ancestor is the closed directory above it and Mode its permission bits.
	Ancestor, Mode string
	// Detail is the check's own text, with the repair.
	Detail string
}

func (e *InstallDirUntraversableError) Error() string {
	return fmt.Sprintf("%s: %s: %s", ErrInstallDirUntraversable, checkInstallDirTraversal, e.Detail)
}

// FixCommand is the narrowest repair: search for others on the ancestor, which
// opens no listing and no writing.
func (e *InstallDirUntraversableError) FixCommand() string {
	return "chmod o+x " + shellWord(e.Ancestor)
}

// Unwrap lets errors.Is(err, ErrInstallDirUntraversable) find it.
func (e *InstallDirUntraversableError) Unwrap() error { return ErrInstallDirUntraversable }

// shellWord quotes a path for a command shown to the operator only when it
// needs it.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+=:@%") == "" {
		return s
	}
	return shellQuote(s)
}

// traversalVerdict is what the closed-ancestor probe means for this install.
// blocks is set only when every link of the chain is established; otherwise a
// closed ancestor is a warning that says which link is missing.
type traversalVerdict struct {
	blocks bool
	status string
	detail string
}

// installDirTraversalCheck reports a directory above the install directory
// that the dedicated runtime user cannot search. A root agent launches
// runveil/udocker workloads as that user, and the runtime's own binaries (the
// patched PRoot) live under the install directory, which the installer opens
// itself (0711) -- but a 0700 ancestor such as /root or a private home hides
// all of it, and every launch then fails with `fork/exec .../proot-...:
// permission denied` while nothing else says why. The agent only logs a
// warning at run time, so the install is where an operator can still choose
// another directory; the node's doctor reports the same condition afterwards.
func installDirTraversalCheck(f installDirFacts) CheckResult {
	v := installDirTraversalVerdict(f)
	return CheckResult{Name: checkInstallDirTraversal, Status: v.status, Detail: v.detail}
}

// installDirTraversalVerdict decides the check and whether it stops the
// install. A closed ancestor is an error that stops the install only when the
// probe can establish every link of what the agent does (see
// docs/how-to/connect-nodes.md for the operator's view):
//
//   - the agent runs as root: the SSH user is root. A non-root user with
//     passwordless sudo gets a root agent only under systemd; a nohup install
//     runs as that user, and the probe cannot tell which one the host will
//     get;
//   - it picks the runveil runtime: the install names udocker, or names
//     nothing and neither docker nor nerdctl answers for root (the agent's
//     order is docker, nerdctl, runveil; an answering one can still be passed
//     over for runveil on an NVIDIA node without the container toolkit, so
//     that case is a warning, never an error) on an architecture with the
//     embedded PRoot;
//   - it uses the dedicated user rather than falling back to its own: the
//     filesystem accepts chown, and the account exists or useradd can create
//     it;
//   - the directory really is closed to that user: its mode says so and it
//     has no ACL that could say otherwise.
//
// Anything less is a warning that names what is not certain.
func installDirTraversalVerdict(f installDirFacts) traversalVerdict {
	ok := func(detail string) traversalVerdict { return traversalVerdict{status: "ok", detail: detail} }
	if f.mac {
		return ok("not needed on a Mac: engines run as the agent's own user")
	}
	switch f.ancestors {
	case "ok":
		return ok(fmt.Sprintf("every directory above %s can be searched by the dedicated runtime user", f.requested))
	case "blocked":
		return blockedTraversalVerdict(f)
	case "unknown":
		return ok(fmt.Sprintf("not checked: the permissions of the directories above %s could not be read", f.requested))
	}
	if f.chown == "refused" {
		return ok("not needed: workloads run as the agent's own user (see install directory ownership)")
	}
	return ok(fmt.Sprintf("not needed: %s is not root and has no passwordless sudo, so workloads run as that user", f.userLabel()))
}

func blockedTraversalVerdict(f installDirFacts) traversalVerdict {
	account := "the dedicated runtime user (udocker, created at install)"
	if f.runtimeUID != "" && f.runtimeUID != "-1" {
		account = "the dedicated runtime user (udocker, uid " + f.runtimeUID + ")"
	}
	detail := fmt.Sprintf("%s (mode %s, owner %s:%s) cannot be searched by %s, so runveil workloads cannot reach the agent's files under %s and every launch fails with permission denied; run `%s` (search only: it opens no listing and no writing) or choose an install directory outside it, such as /opt/gpuworks",
		f.ancestorDir, f.ancestorMode, f.ancestorUID, f.ancestorGID, account, f.requested, (&InstallDirUntraversableError{Ancestor: f.ancestorDir}).FixCommand())
	if reason := runveilNotCertain(f); reason != "" {
		return traversalVerdict{status: "warn", detail: detail + "; not stopping the install: " + reason}
	}
	return traversalVerdict{blocks: true, status: "error", detail: detail}
}

// runveilNotCertain is why the agent might not launch workloads as the
// dedicated user through runveil on this node, or why the directory might not
// be closed to it after all; "" when every link is established.
func runveilNotCertain(f installDirFacts) string {
	if !f.root() {
		return fmt.Sprintf("%s is not root; the agent runs as root only when systemd supervises it, and a nohup install runs as %s, which runs workloads as itself", f.userLabel(), f.userLabel())
	}
	switch f.runtimeDriver {
	case "", "auto", "udocker":
	default:
		return fmt.Sprintf("the install selects the %s runtime, which does not need it, but switching the node to runveil later would fail", f.runtimeDriver)
	}
	if f.runtimeDriver != "udocker" {
		switch {
		case f.docker:
			return "this node has docker, which the agent uses first (it falls back to runveil only where docker cannot give containers the GPUs)"
		case f.nerdctl:
			return "this node has nerdctl, which the agent uses when docker is absent (it falls back to runveil only where nerdctl cannot give containers the GPUs)"
		}
		if arch := strings.ToLower(f.arch); arch != "" && arch != "x86_64" && arch != "amd64" && arch != "aarch64" && arch != "arm64" {
			return "runveil does not support the " + f.arch + " architecture"
		}
	}
	if f.chown != "ok" {
		return "the ownership change the dedicated user needs was not tested here, so the agent may run workloads as itself"
	}
	if (f.runtimeUID == "" || f.runtimeUID == "-1") && !f.useradd {
		return "the udocker account does not exist and there is no useradd to create it, so the agent will run workloads as itself"
	}
	if f.ancestorACL != "no" {
		return "the directory may carry an ACL that lets the dedicated user in, which its mode bits do not show"
	}
	return ""
}
