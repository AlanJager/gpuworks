package diagnostics

import (
	"encoding/json"
	"io"

	"gpuworks.ai/gpuworks/internal/hwident"
	"gpuworks.ai/gpuworks/internal/server/hostidentity"
)

// supportIdentityKey salts the server machine's identity in a collection.
// It is fixed, so one machine has the same digests in every collection and
// a supporter can tell whether two collections come from one machine; it
// is unlike every plugin's key (those start with "pkg"), so a collection's
// digests match no plugin's.
var supportIdentityKey = []byte("core\x00support_bundle")

// ServerHostIdentity is server/host-identity.json: the facts that name the
// server's machine (hostidentity.Facts), each salted with
// supportIdentityKey, absent when the server could not read it. No raw
// value goes into the collection.
type ServerHostIdentity struct {
	SystemUUID  *HostIdentityPart `json:"system_uuid,omitempty"`
	BoardSerial *HostIdentityPart `json:"board_serial,omitempty"`
	MachineID   *HostIdentityPart `json:"machine_id,omitempty"`
	RootFSUUID  *HostIdentityPart `json:"root_fs_uuid,omitempty"`
}

// HostIdentityPart is one salted fact (hwident.IdentityPart).
type HostIdentityPart struct {
	H    string `json:"h"`
	Tail string `json:"tail,omitempty"`
}

func writeServerHostIdentity(w io.Writer, facts hostidentity.Facts) (int64, bool, error) {
	s := facts.Salted(supportIdentityKey)
	doc := ServerHostIdentity{SystemUUID: bundlePart(s.SystemUUID), BoardSerial: bundlePart(s.BoardSerial),
		MachineID: bundlePart(s.MachineID), RootFSUUID: bundlePart(s.RootFSUUID)}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return 0, false, err
	}
	n, err := w.Write(append(raw, '\n'))
	return int64(n), false, err
}

func bundlePart(p hwident.IdentityPart) *HostIdentityPart {
	if !p.Known() {
		return nil
	}
	return &HostIdentityPart{H: p.H, Tail: p.Tail}
}
