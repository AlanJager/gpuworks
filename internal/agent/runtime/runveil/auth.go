package runveil

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Identity is the host account the PRoot process runs as.
type Identity struct {
	UID      int
	GID      int
	Groups   []int  // supplementary groups, exposed as G<gid> entries
	Username string // host name, reported to the guest as container_ruser
}

// Account is the guest user a workload runs as.
type Account struct {
	User  string
	UID   string
	GID   string
	Gecos string
	Home  string
	Shell string
}

// Root reports whether PRoot has to fake uid 0 (-0) for this account.
func (a Account) Root() bool { return a.UID == "0" }

// AuthFiles are the generated passwd/group pair bound over the guest's own.
type AuthFiles struct {
	Passwd string
	Group  string
}

// Names of the generated account files inside the container directory, next
// to container.json and outside ROOT, so they never shadow image content.
const (
	passwdFile = "gpuworks-passwd"
	groupFile  = "gpuworks-group"
)

var (
	userNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_-]*$`)
	userIDRE   = regexp.MustCompile(`^(\d+)(:(\d+))?$`)
)

// PrepareAccount resolves the guest account for the image's User and writes
// the passwd/group pair that makes it resolvable inside the guest.
//
// It follows udocker's _setup_container_user/_create_user without --hostauth:
// the account is looked up in the guest's /etc/passwd; root keeps uid 0 (so
// PRoot runs with -0) while any other user is mapped onto the host identity,
// and the generated files are copies of the guest files plus that account and
// one G<gid> group per host supplementary group.
//
// Deliberate differences: udocker writes the pair to fresh temp files on every
// run and only deletes them when its Python process exits cleanly, which a
// killed workload never does; here they are rewritten in place in the
// container directory. "name:group" is accepted by taking the name, whereas
// udocker rejects it as invalid syntax. And the account's primary group is
// added when the guest group file lacks that gid; udocker consults the host's
// group database instead, which says nothing about what the guest can resolve.
func PrepareAccount(containerDir, root, user string, id Identity) (Account, AuthFiles, error) {
	if user == "" {
		user = "root"
	}
	if name, _, ok := strings.Cut(user, ":"); ok && userNameRE.MatchString(name) {
		user = name
	}
	var wantName, wantUID, wantGID string
	switch {
	case userNameRE.MatchString(user):
		wantName = user
	case userIDRE.MatchString(user):
		m := userIDRE.FindStringSubmatch(user)
		wantUID, wantGID = m[1], m[3]
	default:
		return Account{}, AuthFiles{}, fmt.Errorf("invalid image user %q", user)
	}

	guestPasswd, _ := os.ReadFile(filepath.Join(root, "etc", "passwd"))
	guestGroup, _ := os.ReadFile(filepath.Join(root, "etc", "group"))
	account, found := lookupPasswd(guestPasswd, wantName, wantUID)
	hostUID, hostGID := strconv.Itoa(id.UID), strconv.Itoa(id.GID)
	if found {
		if account.User != "root" {
			account.UID, account.GID = hostUID, hostGID
		}
	} else {
		// A numeric user absent from the guest is named by _fill_user below;
		// udocker would write "uid:gid" itself as the name and corrupt passwd.
		account = Account{User: wantName, UID: wantUID, GID: wantGID}
	}
	// udocker's _fill_user defaults.
	if account.UID == "" {
		account.UID = hostUID
	}
	if account.GID == "" {
		account.GID = hostGID
	}
	if account.User == "" {
		account.User = "udoc" + prefix(account.UID, 4)
	}
	if account.Home == "" {
		account.Home = "/"
	}
	if account.Shell == "" {
		account.Shell = "/bin/sh"
	}
	if account.Gecos == "" {
		account.Gecos = "*UDOCKER*"
	}

	passwd := appendLine(guestPasswd, fmt.Sprintf("%s:x:%s:%s:%s:%s:%s", account.User, account.UID, account.GID, account.Gecos, account.Home, account.Shell))
	group := guestGroup
	if !groupHasGID(group, account.GID) {
		group = appendLine(group, fmt.Sprintf("%s:x:%s:", account.User, account.GID))
	}
	for _, gid := range id.Groups {
		group = appendLine(group, fmt.Sprintf("G%d:x:%d:%s,", gid, gid, account.User))
	}
	files := AuthFiles{Passwd: filepath.Join(containerDir, passwdFile), Group: filepath.Join(containerDir, groupFile)}
	if err := writeFileAtomic(files.Passwd, passwd); err != nil {
		return Account{}, AuthFiles{}, fmt.Errorf("write guest passwd: %w", err)
	}
	if err := writeFileAtomic(files.Group, group); err != nil {
		return Account{}, AuthFiles{}, fmt.Errorf("write guest group: %w", err)
	}
	return account, files, nil
}

func lookupPasswd(data []byte, name, uid string) (Account, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Split(strings.TrimSpace(scanner.Text()), ":")
		if len(fields) != 7 {
			continue
		}
		if (name != "" && fields[0] == name) || (uid != "" && fields[2] == uid) {
			return Account{User: fields[0], UID: fields[2], GID: fields[3], Gecos: fields[4], Home: fields[5], Shell: fields[6]}, true
		}
	}
	return Account{}, false
}

func groupHasGID(data []byte, gid string) bool {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Split(strings.TrimSpace(scanner.Text()), ":")
		if len(fields) >= 3 && fields[2] == gid {
			return true
		}
	}
	return false
}

// appendLine adds line unless the file already holds it verbatim, as
// NixAuthentication.add_user/add_group do.
func appendLine(data []byte, line string) []byte {
	for _, existing := range strings.Split(string(data), "\n") {
		if existing == line {
			return data
		}
	}
	out := append([]byte(nil), data...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return append(out, line+"\n"...)
}

func prefix(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
