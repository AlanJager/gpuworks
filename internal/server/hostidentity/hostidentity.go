// Package hostidentity is the management server's host identity: the
// facts that name the machine the server runs on (the firmware system
// UUID, the mainboard serial, the operating system's machine id and the
// UUID of the root file system), read directly where the service account
// may and from a file root writes before the service starts where it may
// not. Plugins read it salted (POST /host/v1/host-identity), compiled
// modules through app.Host.HostIdentity, and support bundles carry it
// salted; the raw values never leave the server.
package hostidentity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"gpuworks.ai/gpuworks/internal/hwident"
	"gpuworks.ai/gpuworks/internal/proto"
)

// FileVersion is the version of the host-identity file this build writes
// and reads.
const FileVersion = 1

// RunDir and RunFile are where `gpuworks host-identity --write` puts the
// facts only root can read: on /run, a memory file system, so a data
// directory copied to another machine never carries this machine's
// identity. RunDir is owned by root with mode 0755; RunFile by root and
// the service group with mode 0440.
const (
	RunDir  = "/run/gpuworks"
	RunFile = RunDir + "/host-identity.json"
)

// maxFileBytes bounds the file Load reads: four values of at most 128
// bytes each and their names fit well within it.
const maxFileBytes = 4 << 10

// maxValue bounds a board serial, a machine id and a root file system UUID.
const maxValue = 128

// Paths read relative to the file-system root.
const (
	pathSystemUUID  = "sys/class/dmi/id/product_uuid"
	pathBoardSerial = "sys/class/dmi/id/board_serial"
	pathMachineID   = "etc/machine-id"
	pathMountInfo   = "proc/self/mountinfo"
	pathByUUID      = "dev/disk/by-uuid"
	pathSysBlock    = "sys/class/block"
)

// Facts names the machine. An empty field is unknown: unreadable, empty,
// or a firmware placeholder that many machines share. Every field holds a
// normalized value (see Normalize).
type Facts struct {
	// V is FileVersion.
	V int `json:"v"`
	// SystemUUID is the firmware system UUID (SMBIOS product_uuid,
	// IOPlatformUUID on macOS), normalized as an agent reports a node's
	// (proto.NormalizeHostSystemUUID). Root-only on Linux.
	SystemUUID string `json:"system_uuid,omitempty"`
	// BoardSerial is the mainboard serial (the platform serial number on
	// macOS). Root-only on Linux.
	BoardSerial string `json:"board_serial,omitempty"`
	// MachineID is /etc/machine-id, lower-cased.
	MachineID string `json:"machine_id,omitempty"`
	// RootFSUUID is the UUID of the block device mounted at "/", lower-
	// cased; unknown for an overlay, tmpfs or network root.
	RootFSUUID string `json:"root_fs_uuid,omitempty"`
}

// Read reads the facts directly from fsys, rooted at "/" of a Linux host.
// A file it may not read leaves its field unknown.
func Read(fsys fs.FS) Facts {
	return Facts{
		V:           FileVersion,
		SystemUUID:  readValue(fsys, pathSystemUUID, hwident.KindHost),
		BoardSerial: readValue(fsys, pathBoardSerial, hwident.KindBoardSerial),
		MachineID:   readValue(fsys, pathMachineID, hwident.KindMachineID),
		RootFSUUID:  Normalize(hwident.KindRootFS, rootFSUUID(fsys)),
	}
}

// Load is Read with the gaps root fills: the file at name (relative to
// fsys), written by `gpuworks host-identity --write`, supplies the system
// UUID and the board serial when the direct read could not get them, the
// two the kernel lets only root read. A value read directly always wins,
// and the machine id and root file system UUID come only from direct
// reads. A missing, oversized or malformed file adds nothing.
func Load(fsys fs.FS, name string) Facts {
	f := Read(fsys)
	if f.SystemUUID != "" && f.BoardSerial != "" {
		return f
	}
	data, err := readBounded(fsys, name, maxFileBytes)
	if err != nil {
		return f
	}
	file, err := Parse(data)
	if err != nil {
		return f
	}
	if f.SystemUUID == "" {
		f.SystemUUID = file.SystemUUID
	}
	if f.BoardSerial == "" {
		f.BoardSerial = file.BoardSerial
	}
	return f
}

// Marshal encodes f as the host-identity file, one JSON line.
func (f Facts) Marshal() []byte {
	out, _ := json.Marshal(f)
	return append(out, '\n')
}

// ErrInvalid is a host-identity file Parse refuses.
var ErrInvalid = errors.New("host identity file is invalid")

// Parse strictly decodes a host-identity file: one JSON object of version
// FileVersion with only the known fields, each a normalized value.
func Parse(data []byte) (Facts, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f Facts
	if err := dec.Decode(&f); err != nil {
		return Facts{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Facts{}, fmt.Errorf("%w: data after the object", ErrInvalid)
	}
	if f.V != FileVersion {
		return Facts{}, fmt.Errorf("%w: version %d, this build reads %d", ErrInvalid, f.V, FileVersion)
	}
	for kind, v := range map[string]string{hwident.KindHost: f.SystemUUID, hwident.KindBoardSerial: f.BoardSerial,
		hwident.KindMachineID: f.MachineID, hwident.KindRootFS: f.RootFSUUID} {
		if v != "" && Normalize(kind, v) != v {
			return Facts{}, fmt.Errorf("%w: %s is not a normalized value", ErrInvalid, kind)
		}
	}
	return f, nil
}

// Salted is the facts as salted identity parts (hwident.Part): the system
// UUID as hwident.KindHost, the same kind as a node's host, so under one
// salt the server's machine and a node on that machine get one digest.
type Salted struct {
	SystemUUID  hwident.IdentityPart
	BoardSerial hwident.IdentityPart
	MachineID   hwident.IdentityPart
	RootFSUUID  hwident.IdentityPart
}

// Salted is f digested under salt. The caller bounds the salt.
func (f Facts) Salted(salt []byte) Salted {
	return Salted{
		SystemUUID:  hwident.Part(salt, hwident.KindHost, f.SystemUUID),
		BoardSerial: hwident.Part(salt, hwident.KindBoardSerial, f.BoardSerial),
		MachineID:   hwident.Part(salt, hwident.KindMachineID, f.MachineID),
		RootFSUUID:  hwident.Part(salt, hwident.KindRootFS, f.RootFSUUID),
	}
}

// placeholders are values firmware writes when the vendor left a field
// unset. Many machines share them, so they identify none.
var placeholders = map[string]bool{
	"none": true, "unknown": true, "n/a": true, "na": true, "0": true,
	"to be filled by o.e.m.": true, "to be filled by o.e.m": true, "not specified": true,
	"default string": true, "not applicable": true, "not available": true,
	"system serial number": true, "base board serial number": true, "0123456789": true,
	"serial": true, "123456789": true,
}

// Normalize is raw as the facts hold it for kind, or "" when it names no
// machine: empty, a firmware placeholder, all zeros or all F, too long, or
// not of the kind's form. The system UUID follows
// proto.NormalizeHostSystemUUID; the machine id is 32 hex digits; a root
// file system UUID is letters, digits and dashes (an ext4 or XFS UUID, a
// FAT volume id), lower-cased; a board serial is printable ASCII, case
// kept.
func Normalize(kind, raw string) string {
	if kind == hwident.KindHost {
		return proto.NormalizeHostSystemUUID(raw)
	}
	v := strings.Trim(raw, " \t\r\n\x00")
	if v == "" || len(v) > maxValue || placeholders[strings.ToLower(v)] || uniform(v) {
		return ""
	}
	for _, r := range v {
		if r < 0x20 || r > 0x7e {
			return ""
		}
	}
	switch kind {
	case hwident.KindMachineID:
		v = strings.ToLower(v)
		if len(v) != 32 || strings.Trim(v, "0123456789abcdef") != "" {
			return ""
		}
	case hwident.KindRootFS:
		v = strings.ToLower(v)
		if strings.Trim(v, "0123456789abcdefghijklmnopqrstuvwxyz-") != "" {
			return ""
		}
	}
	return v
}

// uniform reports whether v, ignoring dashes, is all '0' or all 'f'.
func uniform(v string) bool {
	s := strings.ToLower(strings.ReplaceAll(v, "-", ""))
	return strings.Trim(s, "0") == "" || strings.Trim(s, "f") == ""
}

func readValue(fsys fs.FS, name, kind string) string {
	data, err := readBounded(fsys, name, maxValue*2)
	if err != nil {
		return ""
	}
	return Normalize(kind, string(data))
}

// readBounded reads at most limit bytes of name; a longer file is an error,
// never a truncated value.
func readBounded(fsys fs.FS, name string, limit int64) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	return data, nil
}

// rootFSUUID finds the UUID of the device mounted at "/" by matching the
// mountinfo entry against the /dev/disk/by-uuid links, first by device
// name and then by major:minor number. It is "" when the root is not a
// block device with a UUID (overlay, tmpfs, network root).
func rootFSUUID(fsys fs.FS) string {
	majMin, source, ok := rootMount(fsys)
	if !ok {
		return ""
	}
	sourceName := ""
	if strings.HasPrefix(source, "/dev/") {
		rel := strings.TrimPrefix(source, "/")
		sourceName = path.Base(rel)
		if target, err := fs.ReadLink(fsys, rel); err == nil {
			sourceName = path.Base(target)
		}
	}
	entries, err := fs.ReadDir(fsys, pathByUUID)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		target, err := fs.ReadLink(fsys, path.Join(pathByUUID, e.Name()))
		if err != nil {
			continue
		}
		dev := path.Base(target)
		if sourceName != "" && dev == sourceName {
			return e.Name()
		}
		data, err := readBounded(fsys, path.Join(pathSysBlock, dev, "dev"), 64)
		if err == nil && strings.TrimSpace(string(data)) == majMin {
			return e.Name()
		}
	}
	return ""
}

// rootMount is the major:minor and source of the last mount at "/".
func rootMount(fsys fs.FS) (majMin, source string, ok bool) {
	data, err := readBounded(fsys, pathMountInfo, 4<<20)
	if err != nil {
		return "", "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		pre, post, found := strings.Cut(line, " - ")
		if !found {
			continue
		}
		f := strings.Fields(pre)
		g := strings.Fields(post)
		if len(f) < 5 || len(g) < 2 || f[4] != "/" {
			continue
		}
		majMin, source, ok = f[2], g[1], true
	}
	return majMin, source, ok
}
