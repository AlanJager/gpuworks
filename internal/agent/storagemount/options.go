package storagemount

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/storagerules"
)

// ValidateOptions checks a comma separated NFS mount option string against the
// rules the server also applies when a storage is saved, and returns it
// normalized (see storagerules.ValidateMountOptions).
func ValidateOptions(opts string) (string, error) {
	return storagerules.ValidateMountOptions(opts)
}

// effectiveOptions applies the defaults for the mount command: rw unless the user chose ro or rw,
// and noatime unless the user chose another access time mode.
func effectiveOptions(user string) (string, error) {
	v, err := ValidateOptions(user)
	if err != nil {
		return "", err
	}
	if v == "" {
		return "rw,noatime", nil
	}
	items := strings.Split(v, ",")
	has := map[string]bool{}
	for _, it := range items {
		n, _, _ := strings.Cut(it, "=")
		has[n] = true
	}
	if !has["rw"] && !has["ro"] {
		items = append([]string{"rw"}, items...)
	}
	if !has["noatime"] && !has["atime"] && !has["relatime"] && !has["strictatime"] {
		items = append(items, "noatime")
	}
	return strings.Join(items, ","), nil
}
