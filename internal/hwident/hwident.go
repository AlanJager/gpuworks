// Package hwident salts hardware identities: a raw serial or UUID becomes a
// digest that differs per caller's salt and a short unsalted tail a person
// can check against the machine. The node reads, the management server's
// host identity and the support bundle share it, so one raw value gives the
// same digest under one salt wherever it is read. It imports nothing from
// this repository, so the agent may use it as well as the server.
package hwident

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// Kinds a raw value is digested as. The kind is part of the digest, so the
// same string read as two kinds gives two unrelated digests.
const (
	// KindHost is a machine's firmware system UUID: a node's
	// HostSystemUUID and the management server's own system UUID are the
	// same kind, so a caller can tell which node runs on the server's
	// machine.
	KindHost = "host"
	// KindCard is an accelerator card's hardware serial.
	KindCard = "card"
	// KindBoardSerial is the mainboard's serial (on macOS the platform
	// serial number).
	KindBoardSerial = "board_serial"
	// KindMachineID is the operating system's machine id (/etc/machine-id).
	KindMachineID = "machine_id"
	// KindRootFS is the UUID of the file system mounted at "/".
	KindRootFS = "root_fs_uuid"
)

// IdentityPart is one salted hardware identity: H, the first 16 hex
// characters (8 bytes) of HMAC-SHA256 keyed with the caller's salt over
// the kind, a zero byte and the raw value; and Tail, the raw value's last
// four characters, so a person can check it against the machine. Tail is
// empty for a raw value shorter than eight characters, so it never is the
// whole value. The zero IdentityPart means the value is unknown.
type IdentityPart struct {
	H    string
	Tail string
}

// Known reports whether the part names a value.
func (p IdentityPart) Known() bool { return p.H != "" }

// Part is raw digested as kind under salt; an empty raw value is the zero
// (unknown) part. The caller bounds and checks the salt: an empty salt
// would give every caller the same digests.
func Part(salt []byte, kind, raw string) IdentityPart {
	if raw == "" {
		return IdentityPart{}
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(kind))
	mac.Write([]byte{0})
	mac.Write([]byte(raw))
	p := IdentityPart{H: hex.EncodeToString(mac.Sum(nil)[:8])}
	if r := []rune(raw); len(r) >= 8 {
		p.Tail = string(r[len(r)-4:])
	}
	return p
}
