package selfinstall

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"strings"
)

// userExists reports whether a local account exists. With CGO disabled
// os/user reads the passwd database directly, which is all we need: the
// service account must be a local one.
func userExists(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	_, err := user.Lookup(name)
	return err == nil
}

// ensureServiceUser creates the unprivileged account the unit runs as. An
// account that already exists (for instance the one the agent's installer
// created on a node that is also a control plane) is reused untouched.
func (i *Installer) ensureServiceUser(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || name == "root" {
		return nil
	}
	if i.deps.LookupUser != nil && i.deps.LookupUser(name) {
		return nil
	}
	shell := nologinShell()
	err := i.deps.Run.Run(ctx, Command{Path: "useradd", Sudo: true,
		Args: []string{"--system", "--home-dir", i.layout.Dir, "--no-create-home", "--shell", shell, name}})
	if err == nil {
		i.io.Say("created system user %s", name)
		return nil
	}
	// A busybox adduser with the long flag spelling is not a given (Alpine,
	// container base images), so try its short form before giving up.
	if busyboxErr := i.deps.Run.Run(ctx, Command{Path: "adduser", Sudo: true,
		Args: []string{"-S", "-D", "-H", "-s", shell, name}}); busyboxErr == nil {
		i.io.Say("created system user %s", name)
		return nil
	}
	return err
}

// nologinShell picks an existing non-interactive shell for the service
// account.
func nologinShell() string {
	for _, candidate := range []string{"/usr/sbin/nologin", "/sbin/nologin", "/bin/false"} {
		if fileExists(candidate) {
			return candidate
		}
	}
	return "/bin/false"
}

// chownTree shares every application directory with the service account.
func (i *Installer) chownTree(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || name == "root" {
		return nil
	}
	roots, err := i.permissionRoots()
	if err != nil {
		return err
	}
	run := i.deps.Run.Run
	group := i.serviceGroup(ctx, name)
	for _, root := range roots {
		if err := run(ctx, Command{
			Path: "chown", Args: []string{"-R", name + ":" + group, root}, Sudo: true,
		}); err != nil {
			return err
		}
		// Share files through the service group. Only directories get setgid;
		// setting it on an executable would change the group it runs as.
		if err := run(ctx, Command{Path: "chmod", Args: []string{"-R", "g+rwX", root}, Sudo: true}); err != nil {
			return err
		}
		if err := run(ctx, Command{Path: "find", Args: []string{root, "-type", "d", "-exec", "chmod", "g+s", "{}", "+"}, Sudo: true}); err != nil {
			return err
		}
	}
	return nil
}

// Validate every root before changing anything. Resolve symlinks before
// deduplicating: data inside the install tree may point to an external disk.
func (i *Installer) permissionRoots() ([]string, error) {
	var roots []string
	for _, dir := range []string{i.layout.Dir, i.layout.DataDir, i.layout.LogsDir} {
		if err := checkDedicatedDirectory(dir); err != nil {
			return nil, fmt.Errorf("refusing recursive permission changes for %s: %w", dir, err)
		}
		root, err := resolveUninstallPath(dir)
		if err != nil {
			return nil, fmt.Errorf("resolve permission directory %s: %w", dir, err)
		}
		if err := checkDedicatedDirectory(root); err != nil {
			return nil, fmt.Errorf("refusing recursive permission changes for %s: %w", dir, err)
		}
		roots = append(roots, root)
	}
	var unique []string
	for n, root := range roots {
		covered := false
		for other, parent := range roots {
			if n != other && pathWithin(parent, root) && (parent != root || other < n) {
				covered = true
				break
			}
		}
		if !covered {
			unique = append(unique, root)
		}
	}
	return unique, nil
}

// serviceGroup is the group the service account's files are shared through:
// the account's own group, which on Linux is its namesake and for a Mac user is
// staff. It falls back to the account name when `id` cannot answer, which is
// what the caller would have guessed anyway.
func (i *Installer) serviceGroup(ctx context.Context, name string) string {
	if g, err := i.deps.Run.Output(ctx, Command{Path: "id", Args: []string{"-gn", name}}); err == nil && strings.TrimSpace(g) != "" {
		return strings.TrimSpace(g)
	}
	return name
}

// addOperatorToGroup puts the operator into the service account's group, which
// is what the tree's group permissions (2775 directories, 0664 files) are for:
// afterwards installs, upgrades and password resets run without sudo, and only
// the systemctl calls still escalate. The current shell does not pick up a new
// group membership, so the returned hint tells the operator to open a new
// terminal (or run newgrp) — the hint is empty when nothing was added.
func (i *Installer) addOperatorToGroup(ctx context.Context, pf Preflight) string {
	if pf.Mode != ModeSystemd || pf.ServiceUser == "" || pf.ServiceUser == "root" {
		return ""
	}
	user := currentUserName()
	if user == "" || user == "root" {
		return ""
	}
	if groups, err := i.deps.Run.Output(ctx, Command{Path: "id", Args: []string{"-nG", user}}); err == nil {
		for _, g := range strings.Fields(groups) {
			if g == pf.ServiceUser {
				return ""
			}
		}
	}
	if err := i.deps.Run.Run(ctx, Command{Path: "usermod", Args: []string{"-aG", pf.ServiceUser, user}, Sudo: true}); err != nil {
		i.io.Warn("warning: could not add %s to the %s group: %v", user, pf.ServiceUser, err)
		return ""
	}
	return fmt.Sprintf("%s was added to the %s group; new terminals get the access, this one needs `newgrp %s` (or a re-login) first.", user, pf.ServiceUser, pf.ServiceUser)
}

// currentUserName is recorded in the audit trail. SUDO_USER names the human who
// escalated, which is what an audit wants; it falls back to the invoking user.
func currentUserName() string {
	if name := strings.TrimSpace(os.Getenv("SUDO_USER")); name != "" {
		return name
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "unknown"
}
