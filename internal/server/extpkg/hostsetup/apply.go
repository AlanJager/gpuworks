package hostsetup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/user"
	"path/filepath"
	"slices"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Cmd is one command host setup runs as root.
type Cmd struct {
	Path string
	Args []string
	// Stdin is the command's input; nil reads nothing.
	Stdin []byte
}

// Runner runs a command as root (escalating when the caller is not).
type Runner interface {
	Run(ctx context.Context, c Cmd) error
}

// Host is the server host as host setup changes it. Its rule files and
// applied records are read directly (both directories are world readable)
// and written only through Run.
type Host struct {
	// RulesDir holds the rule files; empty is the package's RulesDir.
	RulesDir string
	// Records are the applied records.
	Records Records
	Run     Runner
	// GroupExists and InGroup read the user database; nil reads it with
	// os/user.
	GroupExists func(group string) (bool, error)
	InGroup     func(user, group string) (bool, error)
}

// Want are the steps to apply, by package id.
type Want map[string][]extapi.HostSetupStep

// Report is what Apply changed.
type Report struct {
	// Written are the packages whose rule file or record was written;
	// Pruned those whose files were removed. Both sorted.
	Written, Pruned []string
	// Reloaded is set when udev read its rules again and re-ran them.
	Reloaded bool
	// GroupCreated and UserAdded say what changed in the user database;
	// a service account added to Group needs a service restart to have it.
	GroupCreated, UserAdded bool
}

// Changed reports whether Apply changed anything.
func (r Report) Changed() bool {
	return len(r.Written) > 0 || len(r.Pruned) > 0 || r.Reloaded || r.GroupCreated || r.UserAdded
}

func (h Host) rulesDir() string {
	if h.RulesDir == "" {
		return RulesDir
	}
	return h.RulesDir
}

// Apply makes the host match want: the device group exists and holds
// serviceUser (none when it is empty), every wanted package has its rule
// file, udev has read and re-run the rules, and each package has its
// applied record, written last, so a record never claims a rule udev has
// not read. With prune, the rule files and records of packages not in want
// are removed. Running it again with the same want changes nothing.
func (h Host) Apply(ctx context.Context, want Want, serviceUser string, prune bool) (Report, error) {
	var rep Report
	if h.Run == nil {
		return rep, errors.New("hostsetup: no runner")
	}
	ids := slices.Sorted(maps.Keys(want))
	rules := map[string][]byte{}
	digests := map[string]extapi.Digest{}
	for _, id := range ids {
		var err error
		if rules[id], err = Rules(id, want[id]); err != nil {
			return rep, err
		}
		if digests[id], err = Digest(want[id]); err != nil {
			return rep, err
		}
	}
	if len(ids) > 0 {
		if err := h.ensureGroup(ctx, serviceUser, &rep); err != nil {
			return rep, err
		}
	}
	written := map[string]bool{}
	for _, id := range ids {
		name, _ := RuleFile(id)
		path := filepath.Join(h.rulesDir(), name)
		if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, rules[id]) {
			continue
		}
		if err := h.write(ctx, path, rules[id], false); err != nil {
			return rep, err
		}
		written[id] = true
	}
	var stale []string
	if prune {
		var err error
		if stale, err = h.Installed(); err != nil {
			return rep, err
		}
		stale = slices.DeleteFunc(stale, func(id string) bool { _, ok := want[id]; return ok })
		for _, id := range stale {
			name, _ := RuleFile(id)
			if err := h.Run.Run(ctx, Cmd{Path: "rm", Args: []string{"-f", "--", filepath.Join(h.rulesDir(), name)}}); err != nil {
				return rep, err
			}
		}
	}
	var records []string
	for _, id := range ids {
		if got, err := h.Records.Read(id); err != nil || got != digests[id] {
			records = append(records, id)
		}
	}
	if len(written) > 0 || len(stale) > 0 || len(records) > 0 {
		if err := h.reload(ctx); err != nil {
			return rep, err
		}
		rep.Reloaded = true
	}
	if len(records) > 0 {
		if err := h.Run.Run(ctx, Cmd{Path: "mkdir", Args: []string{"-p", "-m", "0755", "--", h.Records.Dir}}); err != nil {
			return rep, err
		}
	}
	for _, id := range records {
		path, _ := h.Records.Path(id)
		if err := h.write(ctx, path, Content(digests[id]), true); err != nil {
			return rep, err
		}
		written[id] = true
	}
	for _, id := range stale {
		path, _ := h.Records.Path(id)
		if err := h.Run.Run(ctx, Cmd{Path: "rm", Args: []string{"-f", "--", path}}); err != nil {
			return rep, err
		}
	}
	rep.Written, rep.Pruned = slices.Sorted(maps.Keys(written)), stale
	return rep, nil
}

// write writes content to path as root with mode 0644. fresh removes the
// file first, so a file someone else created there cannot keep its owner.
func (h Host) write(ctx context.Context, path string, content []byte, fresh bool) error {
	if fresh {
		if err := h.Run.Run(ctx, Cmd{Path: "rm", Args: []string{"-f", "--", path}}); err != nil {
			return err
		}
	}
	if err := h.Run.Run(ctx, Cmd{Path: "tee", Args: []string{"--", path}, Stdin: content}); err != nil {
		return err
	}
	return h.Run.Run(ctx, Cmd{Path: "chmod", Args: []string{"0644", "--", path}})
}

// reload makes udev read its rules again and re-run them for the USB and
// hidraw devices already present.
func (h Host) reload(ctx context.Context) error {
	if err := h.Run.Run(ctx, Cmd{Path: "udevadm", Args: []string{"control", "--reload-rules"}}); err != nil {
		return err
	}
	return h.Run.Run(ctx, Cmd{Path: "udevadm", Args: []string{"trigger", "--action=change", "--subsystem-match=usb", "--subsystem-match=hidraw"}})
}

// ensureGroup creates Group when it is missing and adds serviceUser to it.
func (h Host) ensureGroup(ctx context.Context, serviceUser string, rep *Report) error {
	exists, in := h.GroupExists, h.InGroup
	if exists == nil {
		exists = groupExists
	}
	if in == nil {
		in = userInGroup
	}
	ok, err := exists(Group)
	if err != nil {
		return err
	}
	if !ok {
		if err := h.Run.Run(ctx, Cmd{Path: "groupadd", Args: []string{"--system", Group}}); err != nil {
			return err
		}
		rep.GroupCreated = true
	}
	if serviceUser == "" {
		return nil
	}
	if ok, err = in(serviceUser, Group); err != nil {
		return err
	}
	if !ok {
		if err := h.Run.Run(ctx, Cmd{Path: "usermod", Args: []string{"--append", "--groups", Group, serviceUser}}); err != nil {
			return err
		}
		rep.UserAdded = true
	}
	return nil
}

// Installed lists the packages that have a rule file or an applied record
// on this host, sorted.
func (h Host) Installed() ([]string, error) {
	seen := map[string]bool{}
	for _, dir := range []struct {
		path string
		id   func(string) string
	}{{h.rulesDir(), ruleFileID}, {h.Records.Dir, recordFileID}} {
		entries, err := os.ReadDir(dir.path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if id := dir.id(e.Name()); id != "" {
				seen[id] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(seen)), nil
}

func groupExists(group string) (bool, error) {
	_, err := user.LookupGroup(group)
	var unknown user.UnknownGroupError
	if errors.As(err, &unknown) {
		return false, nil
	}
	return err == nil, err
}

func userInGroup(name, group string) (bool, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return false, fmt.Errorf("hostsetup: service account %s: %w", name, err)
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return false, err
	}
	if u.Gid == g.Gid {
		return true, nil
	}
	ids, err := u.GroupIds()
	if err != nil {
		return false, err
	}
	return slices.Contains(ids, g.Gid), nil
}
