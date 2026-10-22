// Package storagerules holds the rules for what a storage definition may
// contain that both ends enforce: the server when a storage is created,
// changed or probed, and the agent when it mounts one. They live in one place
// so the server can never accept what the agent will refuse at mount time.
//
// It is a leaf package with no dependencies beyond the standard library, so
// both the server and the agent may import it.
package storagerules

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var allowedOptions = map[string]bool{}

func init() {
	for _, n := range strings.Fields(`ro rw noatime atime relatime strictatime nodiratime hard soft intr nointr
		lock nolock ac noac cto nocto fsc nofsc sharecache nosharecache resvport noresvport fg bg vers nfsvers
		minorversion proto port rsize wsize timeo retrans actimeo acregmin acregmax acdirmin acdirmax lookupcache
		sec nconnect max_connect local_lock clientaddr mountproto mountport mountvers namlen retry`) {
		allowedOptions[n] = true
	}
}

var optionValueRe = regexp.MustCompile(`^[A-Za-z0-9._:/-]+$`)

// ValidateMountOptions checks a comma separated NFS mount option string against a whitelist and
// returns it normalized (items trimmed, exact duplicates dropped). Each item is "name" or
// "name=value"; values may only contain letters, digits and ._:/-. Unknown names, malformed items,
// repeated names with different values, and ro together with rw are errors that name the
// offending item.
func ValidateMountOptions(opts string) (string, error) {
	if strings.TrimSpace(opts) == "" {
		return "", nil
	}
	var out []string
	seen := map[string]string{} // name -> full item
	for _, raw := range strings.Split(opts, ",") {
		item := strings.TrimSpace(raw)
		if item == "" {
			return "", fmt.Errorf("mount option list %q contains an empty item", opts)
		}
		name, value, hasValue := strings.Cut(item, "=")
		if !allowedOptions[name] {
			return "", fmt.Errorf("mount option %q is not allowed", item)
		}
		if hasValue && !optionValueRe.MatchString(value) {
			return "", fmt.Errorf("mount option %q has an invalid value", item)
		}
		if prev, ok := seen[name]; ok {
			if prev == item {
				continue
			}
			return "", fmt.Errorf("mount option %q conflicts with %q", item, prev)
		}
		seen[name] = item
		out = append(out, item)
	}
	if _, ro := seen["ro"]; ro {
		if _, rw := seen["rw"]; rw {
			return "", fmt.Errorf("mount options ro and rw cannot be combined")
		}
	}
	return strings.Join(out, ","), nil
}

// exportPattern is "host:/path": a host name or address (an IPv6 one in
// brackets), a colon, and an absolute path free of whitespace and shell
// metacharacters.
var exportPattern = regexp.MustCompile(`^[A-Za-z0-9._\[\]:-]+:/[^\s;$` + "`" + `]*$`)

// ValidateExport checks an NFS export, which must look like host:/path. It
// must not start with '-' (the mount command would read it as an option) nor
// contain whitespace, control characters or shell metacharacters.
func ValidateExport(export string) error {
	switch {
	case export == "":
		return errors.New("export is empty")
	case strings.HasPrefix(export, "-"):
		return fmt.Errorf("export %q must not start with '-'", export)
	case !strings.Contains(export, ":"):
		return fmt.Errorf("export %q must look like host:/path", export)
	case strings.ContainsFunc(export, func(r rune) bool { return r <= ' ' || r == 0x7f }):
		return fmt.Errorf("export %q must not contain whitespace or control characters", export)
	case !exportPattern.MatchString(export):
		return fmt.Errorf("export %q must look like host:/path", export)
	}
	return nil
}
