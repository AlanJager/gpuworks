package provision

import (
	"fmt"
	"strings"
)

// shellWord quotes a path for a command shown to the operator only when it
// needs it.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+=:@%") == "" {
		return s
	}
	return shellQuote(s)
}

// installDirTraversalCheck reports a directory above the install directory
// that the dedicated runtime user cannot search. A root agent launches
// runveil/udocker workloads as that user, and the runtime's own binaries (the
// patched PRoot) live under the install directory, which the installer opens
// itself (0711) -- but a 0700 ancestor such as /root or a private home hides
// all of it, and every launch then fails with `fork/exec .../proot-...:
// permission denied` while nothing else says why. The agent only logs a
// warning at run time, so the precheck is where an operator can still choose
// another directory; the node's doctor reports the same condition afterwards.
//
// Like the file-name check, it is an error on a node without docker, which
// would otherwise run its workloads from the runveil runtime, and a warning on
// one with docker, which keeps its images with dockerd.
func installDirTraversalCheck(f installDirFacts) CheckResult {
	if f.mac {
		return CheckResult{Name: checkInstallDirTraversal, Status: "ok", Detail: "not needed on a Mac: engines run as the agent's own user"}
	}
	switch f.ancestors {
	case "ok":
		return CheckResult{Name: checkInstallDirTraversal, Status: "ok", Detail: fmt.Sprintf("every directory above %s can be searched by the dedicated runtime user", f.requested)}
	case "blocked":
		account := "the dedicated runtime user (udocker, created at install)"
		if f.runtimeUID != "" && f.runtimeUID != "-1" {
			account = "the dedicated runtime user (udocker, uid " + f.runtimeUID + ")"
		}
		detail := fmt.Sprintf("%s (mode %s, owner %s:%s) cannot be searched by %s, so runveil workloads cannot reach the agent's files under %s and every launch fails with permission denied; run `chmod o+x %s` (search only: it opens no listing and no writing) or choose an install directory outside it, such as /opt/gpuworks",
			f.ancestorDir, f.ancestorMode, f.ancestorUID, f.ancestorGID, account, f.requested, shellWord(f.ancestorDir))
		if f.docker {
			return CheckResult{Name: checkInstallDirTraversal, Status: "warn", Detail: detail + "; this node has docker, which does not need it, but the runveil runtime could not run from here"}
		}
		return CheckResult{Name: checkInstallDirTraversal, Status: "error", Detail: detail}
	case "unknown":
		return CheckResult{Name: checkInstallDirTraversal, Status: "ok", Detail: fmt.Sprintf("not checked: the permissions of the directories above %s could not be read", f.requested)}
	default:
		if f.chown == "refused" {
			return CheckResult{Name: checkInstallDirTraversal, Status: "ok", Detail: "not needed: workloads run as the agent's own user (see install directory ownership)"}
		}
		return CheckResult{Name: checkInstallDirTraversal, Status: "ok", Detail: fmt.Sprintf("not needed: %s is not root and has no passwordless sudo, so workloads run as that user", f.userLabel())}
	}
}
