package proto

import "strings"

// Hello.HostSystemUUID is the host's firmware system UUID: SMBIOS/DMI
// product_uuid on Linux, IOPlatformUUID on macOS. It is empty when the agent
// could not read it (a non-root agent on Linux, where the file is
// root-only), when the firmware left a placeholder, and from agents that
// predate it. Several logical nodes on one host report the same value; it
// describes the machine, not the node.

// MaxHostSystemUUIDLen bounds Hello.HostSystemUUID: an SMBIOS UUID is 36
// characters, and anything longer than this is not one.
const MaxHostSystemUUIDLen = 64

// hostUUIDPlaceholders are values firmware writes when the vendor left the
// system UUID unset. Many machines share them, so they identify none. (The
// worded ones with spaces or slashes, "Not Specified", "N/A", fail the
// character check anyway.)
var hostUUIDPlaceholders = map[string]bool{
	"03000200-0400-0500-0006-000700080009": true, // a common unset-board default
	"none":                                 true,
	"unknown":                              true,
}

// NormalizeHostSystemUUID is the host system UUID as the wire carries it:
// trimmed and lower-cased, or "" for a value that identifies no machine
// (empty, a firmware placeholder, all zeros or all F, longer than
// MaxHostSystemUUIDLen, or holding anything but letters, digits and
// dashes). The agent applies it before sending and the server again on
// receipt, since the server never trusts what an agent sends.
func NormalizeHostSystemUUID(raw string) string {
	v := strings.ToLower(strings.Trim(raw, " \t\r\n\x00"))
	if v == "" || len(v) > MaxHostSystemUUIDLen || hostUUIDPlaceholders[v] {
		return ""
	}
	digits := 0
	uniform := true
	var first rune
	for _, r := range v {
		switch {
		case r == '-':
			continue
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z':
		default:
			return ""
		}
		if digits == 0 {
			first = r
		} else if r != first {
			uniform = false
		}
		digits++
	}
	if digits == 0 || (uniform && (first == '0' || first == 'f')) {
		return ""
	}
	return v
}
