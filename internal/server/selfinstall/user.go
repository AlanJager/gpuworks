package selfinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/securefile"
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
	if err := i.ensureCredentialGroup(ctx); err != nil {
		return err
	}
	if i.deps.LookupUser != nil && i.deps.LookupUser(name) {
		return nil
	}
	shell := nologinShell()
	err := i.deps.Run.Run(ctx, Command{Path: "useradd", Sudo: true,
		Args: []string{"--system", "--gid", DefaultCredentialGroup, "--home-dir", i.layout.Dir, "--no-create-home", "--shell", shell, name}})
	if err == nil {
		i.io.Say("created system user %s", name)
		return nil
	}
	// A busybox adduser with the long flag spelling is not a given (Alpine,
	// container base images), so try its short form before giving up.
	if busyboxErr := i.deps.Run.Run(ctx, Command{Path: "adduser", Sudo: true,
		Args: []string{"-S", "-D", "-H", "-G", DefaultCredentialGroup, "-s", shell, name}}); busyboxErr == nil {
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

// chownTree shares Linux installations only through the explicit platform
// operator group; a service account's unrelated primary group is not trusted.
func (i *Installer) chownTree(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if runtime.GOOS == "darwin" || i.deps.launchdRunning() {
		return i.chownOwnerOnly(ctx, name)
	}
	if name == "" || name == "root" {
		return i.hardenSensitiveFiles(ctx, securefile.Policy{})
	}
	roots, err := i.permissionRoots()
	if err != nil {
		return err
	}
	if err := i.checkGroupSharingACLs(roots); err != nil {
		return err
	}
	run := i.deps.Run.Run
	if err := i.ensureCredentialGroup(ctx); err != nil {
		return err
	}
	policy, err := i.credentialGroupPolicy()
	if err != nil {
		return err
	}
	group := DefaultCredentialGroup
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
	return i.hardenSensitiveFiles(ctx, policy)
}

// serviceGroup is explicit platform administration authority, independent of
// the service account's primary group.
func (i *Installer) serviceGroup(ctx context.Context, name string) string {
	return DefaultCredentialGroup
}

func (i *Installer) lookupCredentialGroup() (*user.Group, error) {
	lookup := i.deps.LookupGroup
	if lookup == nil {
		lookup = user.LookupGroup
	}
	return lookup(DefaultCredentialGroup)
}

func (i *Installer) ensureCredentialGroup(ctx context.Context) error {
	if _, err := i.lookupCredentialGroup(); err == nil {
		return nil
	} else {
		var missing user.UnknownGroupError
		if !errors.As(err, &missing) {
			return fmt.Errorf("look up platform credential group: %w", err)
		}
	}
	if err := i.deps.Run.Run(ctx, Command{Path: "groupadd", Args: []string{"--system", DefaultCredentialGroup}, Sudo: true}); err != nil {
		if fallback := i.deps.Run.Run(ctx, Command{Path: "addgroup", Args: []string{"-S", DefaultCredentialGroup}, Sudo: true}); fallback != nil {
			return fmt.Errorf("create platform credential group: %w", err)
		}
	}
	if _, err := i.lookupCredentialGroup(); err != nil {
		return fmt.Errorf("resolve created platform credential group: %w", err)
	}
	return nil
}

func (i *Installer) credentialGroupPolicy() (securefile.Policy, error) {
	group, err := i.lookupCredentialGroup()
	if err != nil {
		return securefile.Policy{}, err
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return securefile.Policy{}, fmt.Errorf("invalid platform credential group: %w", err)
	}
	return securefile.TrustedGroup(gid)
}

// chownOwnerOnly never opens a temporary staff-group access window on macOS.
func (i *Installer) chownOwnerOnly(ctx context.Context, name string) error {
	roots, err := i.permissionRoots()
	if err != nil {
		return err
	}
	for _, root := range roots {
		if name != "" && name != "root" {
			if err := i.deps.Run.Run(ctx, Command{Path: "chown", Args: []string{"-R", name, root}, Sudo: true}); err != nil {
				return err
			}
		}
		if err := i.deps.Run.Run(ctx, Command{Path: "chmod", Args: []string{"-R", "go-w", root}, Sudo: name != "" && name != "root"}); err != nil {
			return err
		}
		if err := i.deps.Run.Run(ctx, Command{Path: "find", Args: []string{root, "-type", "d", "-exec", "chmod", "g-s", "{}", "+"}, Sudo: name != "" && name != "root"}); err != nil {
			return err
		}
	}
	return i.hardenSensitiveFiles(ctx, securefile.Policy{})
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
			if g == DefaultCredentialGroup {
				return ""
			}
		}
	}
	if err := i.deps.Run.Run(ctx, Command{Path: "usermod", Args: []string{"-aG", DefaultCredentialGroup, user}, Sudo: true}); err != nil {
		i.io.Warn("warning: could not add %s to the %s group: %v", user, DefaultCredentialGroup, err)
		return ""
	}
	return fmt.Sprintf("%s was added to the %s group; new terminals get the access, this one needs `newgrp %s` (or a re-login) first.", user, DefaultCredentialGroup, DefaultCredentialGroup)
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
