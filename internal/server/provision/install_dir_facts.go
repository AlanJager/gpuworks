package provision

import (
	"fmt"
	"strconv"
	"strings"
)

// installDirFacts is the parsed probe output.
type installDirFacts struct {
	requested, exists, probed string
	uid, user                 string
	mount, fstype, options    string
	statfs                    string
	totalKiB, availKiB        int64
	haveSpace                 bool
	container, sudo           bool
	writable, privWritable    bool
	chown, chownErr           string
	// ancestors is the verdict on the directories above the install directory
	// for the dedicated runtime user: ok, blocked (the ancestor* fields name
	// the first one), unknown or skipped; runtimeUID is that user's uid, -1
	// while the account does not exist.
	ancestors                 string
	runtimeUID                string
	ancestorDir, ancestorMode string
	ancestorUID, ancestorGID  string
	// ancestorACL says whether the blocking directory carries an ACL (yes,
	// no, unknown), which the mode bits do not show.
	ancestorACL     string
	names, namesErr string
	// docker and nerdctl are runtimes that answer for the agent's privileges;
	// arch is `uname -m`; useradd says the agent can create the dedicated
	// account; runtimeDriver is the runtime the install names ("" = the agent
	// picks).
	docker, nerdctl bool
	useradd         bool
	arch            string
	runtimeDriver   string
	// mac is a Mac install; macAdmin means its installer runs with
	// administrator rights, which may come from sudo with the SSH password
	// rather than the passwordless sudo the probe itself can use.
	mac, macAdmin bool
}

func parseInstallDirFacts(out string) (installDirFacts, error) {
	var f installDirFacts
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !ok {
			continue
		}
		seen[key] = true
		switch key {
		case "requested":
			f.requested = value
		case "exists":
			f.exists = value
		case "probed":
			f.probed = value
		case "uid":
			f.uid = strings.TrimSpace(value)
		case "user":
			f.user = strings.TrimSpace(value)
		case "mount":
			f.mount = value
		case "fstype":
			f.fstype = strings.TrimSpace(value)
		case "options":
			f.options = strings.TrimSpace(value)
		case "statfs":
			f.statfs = strings.TrimSpace(value)
		case "total_kib":
			f.totalKiB, _ = strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		case "avail_kib":
			n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			f.availKiB, f.haveSpace = n, err == nil
		case "container":
			f.container = value == "yes"
		case "sudo":
			f.sudo = value == "yes"
		case "writable":
			f.writable = value == "yes"
		case "priv_writable":
			f.privWritable = value == "yes"
		case "chown":
			f.chown = value
		case "nerdctl":
			f.nerdctl = value == "yes"
		case "useradd":
			f.useradd = value == "yes"
		case "arch":
			f.arch = strings.TrimSpace(value)
		case "ancestors":
			f.ancestors = strings.TrimSpace(value)
		case "runtime_uid":
			f.runtimeUID = strings.TrimSpace(value)
		case "ancestor_dir":
			f.ancestorDir = value
		case "ancestor_mode":
			f.ancestorMode = strings.TrimSpace(value)
		case "ancestor_uid":
			f.ancestorUID = strings.TrimSpace(value)
		case "ancestor_gid":
			f.ancestorGID = strings.TrimSpace(value)
		case "ancestor_acl":
			f.ancestorACL = strings.TrimSpace(value)
		case "chown_err":
			f.chownErr = strings.TrimSpace(value)
		case "names":
			f.names = value
		case "names_err":
			f.namesErr = strings.TrimSpace(value)
		case "docker":
			f.docker = value == "yes"
		}
	}
	if !seen["probed"] || !seen["chown"] {
		return f, fmt.Errorf("incomplete probe output: %q", sanitizeProbeOutput(out))
	}
	return f, nil
}

func (f installDirFacts) root() bool { return f.uid == "0" }

// filesystem is the mount's own type name (nfs4 rather than the nfs that
// statfs(2) reports for both NFS versions), falling back to statfs.
func (f installDirFacts) filesystem() string {
	if f.fstype != "" {
		return f.fstype
	}
	return f.statfs
}

func (f installDirFacts) noexec() bool {
	for _, opt := range strings.Split(f.options, ",") {
		if opt == "noexec" {
			return true
		}
	}
	return false
}

func (f installDirFacts) onNetworkFilesystem() bool {
	switch fs := f.filesystem(); fs {
	case "nfs", "nfs4", "cifs", "smb3", "smbfs", "ceph", "glusterfs", "lustre", "beegfs", "gpfs", "afs", "9p", "virtiofs", "fuse":
		return true
	default:
		// fuse.overlayfs is a local layered filesystem; every other FUSE
		// filesystem (sshfs, s3fs, juicefs, rclone...) is remote or
		// userspace-backed.
		return strings.HasPrefix(fs, "fuse.") && fs != "fuse.overlayfs"
	}
}

func (f installDirFacts) onOverlay() bool {
	fs := f.filesystem()
	return fs == "overlay" || fs == "overlayfs" || fs == "fuse.overlayfs"
}

func (f installDirFacts) onMemory() bool {
	fs := f.filesystem()
	return fs == "tmpfs" || fs == "ramfs"
}
