package extpkg

import (
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/extpkg/archive"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Reasons of ErrorPluginFileMismatch.
const (
	FileUnlisted = "unlisted"
	FileMissing  = "missing"
	FileSize     = "size"
	FileSHA256   = "sha256"
)

func fileMismatch(member, reason string) error {
	return messages.NewProductError(messages.ErrorPluginFileMismatch,
		fmt.Sprintf("package member %q does not match the signed file list (%s)", member, reason),
		map[string]any{"member": member, "reason": reason})
}

// checkFiles is step 9: the members are exactly the listed files, each of
// its listed size and SHA-256, and a package with a server part carries
// exactly one executable directly under bin/<os>_<arch>/, which it returns.
func checkFiles(m *extapi.Manifest, members []archive.Member, p Platform) (string, error) {
	listed := make(map[string]extapi.FileEntry, len(m.Files))
	for _, f := range m.Files {
		listed[f.Path] = f
	}
	have := make(map[string]bool, len(members))
	for _, mem := range members {
		f, ok := listed[mem.Path]
		if !ok {
			return "", fileMismatch(mem.Path, FileUnlisted)
		}
		if f.Size != mem.Size {
			return "", fileMismatch(mem.Path, FileSize)
		}
		if f.SHA256 != mem.SHA256 {
			return "", fileMismatch(mem.Path, FileSHA256)
		}
		have[mem.Path] = true
	}
	for _, f := range m.Files {
		if !have[f.Path] {
			return "", fileMismatch(f.Path, FileMissing)
		}
	}
	if m.Runtime == nil {
		return "", nil
	}
	return ServerBinary(m, p)
}

// ServerBinary returns the server part's executable for platform p: the one
// listed executable directly under bin/<os>_<arch>/ (helpers live in
// subdirectories). Zero or several fail with ErrorPluginBinaryMissing.
func ServerBinary(m *extapi.Manifest, p Platform) (string, error) {
	dir := "bin/" + p.OS + "_" + p.Arch + "/"
	var found []string
	for _, f := range m.Files {
		if rest, ok := strings.CutPrefix(f.Path, dir); ok && f.Exec && !strings.Contains(rest, "/") {
			found = append(found, f.Path)
		}
	}
	if len(found) != 1 {
		return "", messages.NewProductError(messages.ErrorPluginBinaryMissing,
			fmt.Sprintf("package needs exactly one server executable under %s, has %d", dir, len(found)),
			map[string]any{"os": p.OS, "arch": p.Arch, "count": len(found)})
	}
	return found[0], nil
}
