package launchd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"time"
)

// NewHost is this Mac. Self is the person behind the command: under sudo, the
// account sudo was run from, whose launch agent an uninstall removes.
func NewHost(out io.Writer) (*Host, error) {
	h := &Host{
		Launchctl: func(ctx context.Context, args ...string) (string, error) {
			out, err := exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
			return string(out), err
		},
		Admin:  os.Geteuid() == 0,
		Lookup: LookupAccount,
		Chown:  os.Lchown,
		Out:    out,
		Wait:   15 * time.Second,
		Tick:   500 * time.Millisecond,
		// The plist's ThrottleInterval is 3 s.
		Stable: 4 * time.Second,
	}
	name := ""
	if h.Admin {
		name = os.Getenv("SUDO_USER")
	}
	if name == "" {
		current, err := user.Current()
		if err != nil {
			return nil, err
		}
		name = current.Username
	}
	self, err := LookupAccount(name)
	if err != nil {
		return nil, err
	}
	h.Self = self
	return h, nil
}

// LookupAccount finds a macOS user by name.
func LookupAccount(name string) (Account, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return Account{}, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return Account{}, fmt.Errorf("uid of %s: %w", name, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return Account{}, fmt.Errorf("gid of %s: %w", name, err)
	}
	return Account{Name: u.Username, UID: uid, GID: gid, Home: u.HomeDir}, nil
}
