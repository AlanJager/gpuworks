package hostidentity

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"gpuworks.ai/gpuworks/internal/hwident"
)

const (
	sysUUID     = "4C4C4544-0042-3510-8051-B4C04F4E3432"
	machineID   = "0123456789ABCDEF0123456789abcdef"
	rootFS      = "2f9c1e0a-6b7d-4e8f-9a1b-3c5d7e9f0a2b"
	runFileName = "run/gpuworks/host-identity.json"
)

// host is a Linux root file system with every fact readable and the root
// file system on /dev/sda2.
func host() fstest.MapFS {
	return fstest.MapFS{
		pathSystemUUID:  {Data: []byte(sysUUID + "\n")},
		pathBoardSerial: {Data: []byte("  PF2ABC9X  \n")},
		pathMachineID:   {Data: []byte(machineID + "\n")},
		pathMountInfo: {Data: []byte("22 1 8:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw\n" +
			"30 22 0:26 / /run rw - tmpfs tmpfs rw\n")},
		pathByUUID + "/" + rootFS:  {Data: []byte("../../sda2"), Mode: fs.ModeSymlink},
		pathByUUID + "/1111-2222":  {Data: []byte("../../sda1"), Mode: fs.ModeSymlink},
		pathSysBlock + "/sda2/dev": {Data: []byte("8:2\n")},
		pathSysBlock + "/sda1/dev": {Data: []byte("8:1\n")},
	}
}

// Every fact is read and normalized: UUIDs and the machine id lower-cased,
// the serial trimmed with its case kept, the root file system found by its
// device.
func TestReadNormalizesEveryFact(t *testing.T) {
	got := Read(host())
	want := Facts{V: 1, SystemUUID: strings.ToLower(sysUUID), BoardSerial: "PF2ABC9X",
		MachineID: strings.ToLower(machineID), RootFSUUID: rootFS}
	if got != want {
		t.Fatalf("Read = %+v\nwant %+v", got, want)
	}
}

// Placeholders, all-zero or all-F values, values not of their kind's form
// and unreadable files are unknown, never guessed.
func TestReadLeavesPlaceholdersAndMissingFilesUnknown(t *testing.T) {
	fsys := host()
	fsys[pathSystemUUID] = &fstest.MapFile{Data: []byte("03000200-0400-0500-0006-000700080009\n")}
	fsys[pathBoardSerial] = &fstest.MapFile{Data: []byte("To Be Filled By O.E.M.\n")}
	fsys[pathMachineID] = &fstest.MapFile{Data: []byte("00000000000000000000000000000000\n")}
	delete(fsys, pathMountInfo)
	if got := Read(fsys); got != (Facts{V: 1}) {
		t.Fatalf("placeholders read as %+v", got)
	}
	for _, c := range []struct{ kind, raw string }{
		{hwident.KindBoardSerial, "FFFF-FFFF"},
		{hwident.KindBoardSerial, "PF2\x01ABC"},
		{hwident.KindBoardSerial, strings.Repeat("x", maxValue+1)},
		{hwident.KindBoardSerial, "序列号12345678"},
		{hwident.KindMachineID, "abc"},
		{hwident.KindMachineID, "0123456789abcdef0123456789abcdeg"},
		{hwident.KindRootFS, "a/b"},
	} {
		if got := Normalize(c.kind, c.raw); got != "" {
			t.Errorf("Normalize(%s, %q) = %q", c.kind, c.raw, got)
		}
	}
	if got := Read(fstest.MapFS{}); got != (Facts{V: 1}) {
		t.Fatalf("empty host = %+v", got)
	}
}

// The root file system is matched by major:minor when the mount source is
// not a device path; an overlay root has no UUID.
func TestRootFSUUIDByNumberAndOverlay(t *testing.T) {
	fsys := host()
	fsys[pathMountInfo] = &fstest.MapFile{Data: []byte("22 1 8:2 / / rw - ext4 /dev/root rw\n")}
	if got := Read(fsys).RootFSUUID; got != rootFS {
		t.Fatalf("by number = %q", got)
	}
	fsys[pathMountInfo] = &fstest.MapFile{Data: []byte("22 1 0:45 / / rw - overlay overlay rw\n")}
	if got := Read(fsys).RootFSUUID; got != "" {
		t.Fatalf("overlay root = %q", got)
	}
}

// An unprivileged server cannot read the DMI files: the root-written file
// fills exactly those two gaps.
func TestLoadFillsOnlyTheRootOnlyGaps(t *testing.T) {
	fsys := host()
	delete(fsys, pathSystemUUID)
	delete(fsys, pathBoardSerial)
	file := Facts{V: 1, SystemUUID: "6a3b1c2d-4e5f-6071-8293-a4b5c6d7e8f9", BoardSerial: "FROMROOT1",
		MachineID: "ffffffffffffffffffffffffffffffee", RootFSUUID: "dead-beef"}
	fsys[runFileName] = &fstest.MapFile{Data: file.Marshal()}
	got := Load(fsys, runFileName)
	want := Facts{V: 1, SystemUUID: file.SystemUUID, BoardSerial: "FROMROOT1",
		MachineID: strings.ToLower(machineID), RootFSUUID: rootFS}
	if got != want {
		t.Fatalf("Load = %+v\nwant %+v", got, want)
	}
}

// A value read directly wins over the file, and the machine id and root
// file system come only from direct reads, even when they are unknown.
func TestLoadNeverOverridesDirectReads(t *testing.T) {
	fsys := host()
	delete(fsys, pathBoardSerial)
	delete(fsys, pathMachineID)
	delete(fsys, pathMountInfo)
	file := Facts{V: 1, SystemUUID: "6a3b1c2d-4e5f-6071-8293-a4b5c6d7e8f9", BoardSerial: "FROMROOT1",
		MachineID: "ffffffffffffffffffffffffffffffee", RootFSUUID: "dead-beef"}
	fsys[runFileName] = &fstest.MapFile{Data: file.Marshal()}
	got := Load(fsys, runFileName)
	want := Facts{V: 1, SystemUUID: strings.ToLower(sysUUID), BoardSerial: "FROMROOT1"}
	if got != want {
		t.Fatalf("Load = %+v\nwant %+v", got, want)
	}
}

// A missing, malformed, oversized or newer file adds nothing.
func TestLoadIgnoresAFileItCannotTrust(t *testing.T) {
	base := host()
	delete(base, pathSystemUUID)
	delete(base, pathBoardSerial)
	want := Read(base)
	for name, data := range map[string]string{
		"missing":     "",
		"not json":    "{",
		"unknown key": `{"v":1,"board_serial":"X1234567","owner":"root"}`,
		"version 2":   `{"v":2,"board_serial":"X1234567"}`,
		"no version":  `{"board_serial":"X1234567"}`,
		"two objects": `{"v":1,"board_serial":"X1234567"}{"v":1}`,
		"not normal":  `{"v":1,"system_uuid":"6A3B1C2D-4E5F-6071-8293-A4B5C6D7E8F9"}`,
		"placeholder": `{"v":1,"board_serial":"Default string"}`,
		"oversized":   `{"v":1,"board_serial":"X1234567"}` + strings.Repeat(" ", maxFileBytes),
	} {
		fsys := fstest.MapFS{}
		for k, v := range base {
			fsys[k] = v
		}
		if data != "" {
			fsys[runFileName] = &fstest.MapFile{Data: []byte(data)}
		}
		if got := Load(fsys, runFileName); got != want {
			t.Errorf("%s: Load = %+v, want %+v", name, got, want)
		}
	}
}

// Parse is strict and round-trips what Marshal writes.
func TestParseRoundTripsAndIsStrict(t *testing.T) {
	f := Read(host())
	back, err := Parse(f.Marshal())
	if err != nil || back != f {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	if string(f.Marshal()) != `{"v":1,"system_uuid":"4c4c4544-0042-3510-8051-b4c04f4e3432","board_serial":"PF2ABC9X","machine_id":"0123456789abcdef0123456789abcdef","root_fs_uuid":"2f9c1e0a-6b7d-4e8f-9a1b-3c5d7e9f0a2b"}`+"\n" {
		t.Fatalf("file = %s", f.Marshal())
	}
	if _, err := Parse([]byte(`{"v":1,"system_uuid":1}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a number for a string = %v", err)
	}
	if empty, err := Parse([]byte(`{"v":1}`)); err != nil || empty != (Facts{V: 1}) {
		t.Fatalf("no facts = %+v, %v", empty, err)
	}
}

// Salted digests each fact as its kind, the system UUID as a node's host,
// so the server's machine and a node on it share a digest under one salt;
// unknown facts stay unknown, and two salts give unrelated digests.
func TestSaltedDigestsEachFactAsItsKind(t *testing.T) {
	f := Read(host())
	salt := []byte("pkg\x00acme_backup\x00s")
	s := f.Salted(salt)
	if s.SystemUUID != hwident.Part(salt, hwident.KindHost, f.SystemUUID) ||
		s.BoardSerial != hwident.Part(salt, hwident.KindBoardSerial, "PF2ABC9X") ||
		s.MachineID != hwident.Part(salt, hwident.KindMachineID, f.MachineID) ||
		s.RootFSUUID != hwident.Part(salt, hwident.KindRootFS, rootFS) {
		t.Fatalf("Salted = %+v", s)
	}
	if s.SystemUUID.Tail != "3432" || s.BoardSerial.Tail != "BC9X" || s.MachineID.Tail != "cdef" {
		t.Fatalf("tails = %+v", s)
	}
	if again := f.Salted(salt); again != s {
		t.Fatal("one salt gave two answers")
	}
	if other := f.Salted([]byte("t")); other.SystemUUID.H == s.SystemUUID.H || other.SystemUUID.Tail != s.SystemUUID.Tail {
		t.Fatalf("another salt = %+v", other)
	}
	if none := (Facts{V: 1}).Salted(salt); none != (Salted{}) {
		t.Fatalf("no facts salted = %+v", none)
	}
}

// macOS gives the platform UUID and serial number through ioreg.
func TestParseIOReg(t *testing.T) {
	out := `+-o J316sAP  <class IOPlatformExpertDevice, id 0x100000202, registered, matched, active, busy 0 (12 ms), retain 39>
    {
      "IOPlatformSerialNumber" = "C02XG1ABJGH5"
      "IOPlatformUUID" = "6A3B1C2D-4E5F-6071-8293-A4B5C6D7E8F9"
      "model" = <"MacBookPro18,1">
    }
`
	want := Facts{V: 1, SystemUUID: "6a3b1c2d-4e5f-6071-8293-a4b5c6d7e8f9", BoardSerial: "C02XG1ABJGH5"}
	if got := ParseIOReg(out); got != want {
		t.Fatalf("ParseIOReg = %+v", got)
	}
	if got := ParseIOReg(""); got != (Facts{V: 1}) {
		t.Fatalf("no output = %+v", got)
	}
}

// Local reads once and never panics, whatever the host.
func TestLocalIsStable(t *testing.T) {
	if a, b := Local(), Local(); a != b || a.V != FileVersion {
		t.Fatalf("Local = %+v then %+v", a, b)
	}
}
