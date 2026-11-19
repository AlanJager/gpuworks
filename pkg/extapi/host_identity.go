package extapi

// Object names of the host API's host-identity read.
const (
	objectHostIdentityRequest = "host_identity_request"
	objectHostIdentityAnswer  = "host_identity_answer"
)

// PathHostHostIdentity answers POST with a HostIdentityAnswer for a
// HostIdentityRequest: the salted identity of the machine the management
// server runs on. It needs the read:host_identity permission and the
// CapHostHostIdentity capability. It is a POST so the salt never appears
// in a URL. Frozen with protocol major 1.
const PathHostHostIdentity = "/host/v1/host-identity"

// MaxHostIdentityRequestBytes bounds an encoded host-identity request; the
// server refuses a larger body as too large before decoding it. The
// longest salt, every byte escaped, fits.
const MaxHostIdentityRequestBytes = 4 << 10

// HostIdentityRequest is the body of POST /host/v1/host-identity.
type HostIdentityRequest struct {
	// Salt keys the digests and is bound to the package as for the node
	// reads (MaxNodeIdentitySalt): the same salt gives this package the
	// same digests in both reads, and another package unrelated ones.
	Salt string `json:"salt"`
}

// DecodeHostIdentityRequest decodes and validates a host-identity request.
func DecodeHostIdentityRequest(data []byte) (*HostIdentityRequest, error) {
	if len(data) > MaxHostIdentityRequestBytes {
		return nil, malformed(objectHostIdentityRequest, &fieldError{rule: RuleRange})
	}
	var r HostIdentityRequest
	if err := decodeStrict(objectHostIdentityRequest, data, &r); err != nil {
		return nil, err
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// Validate checks the request's rules.
func (r *HostIdentityRequest) Validate() error {
	if f := text("salt", r.Salt, MaxNodeIdentitySalt); f != nil {
		return wireInvalid(objectHostIdentityRequest, f)
	}
	return nil
}

// HostIdentityAnswer is the salted identity of the management server's
// machine, each part absent when the server could not read it (a
// firmware placeholder, a container, a root file system without a UUID).
// SystemUUID is digested as a node's host is, so under one salt it equals
// the host of each node on the server's machine.
type HostIdentityAnswer struct {
	SystemUUID  *IdentityPart `json:"system_uuid,omitempty"`
	BoardSerial *IdentityPart `json:"board_serial,omitempty"`
	MachineID   *IdentityPart `json:"machine_id,omitempty"`
	RootFSUUID  *IdentityPart `json:"root_fs_uuid,omitempty"`
}

// DecodeHostIdentityAnswer decodes and validates a host-identity answer.
func DecodeHostIdentityAnswer(data []byte) (*HostIdentityAnswer, error) {
	var a HostIdentityAnswer
	if err := decodeStrict(objectHostIdentityAnswer, data, &a); err != nil {
		return nil, err
	}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return &a, nil
}

// Validate checks the answer's rules.
func (a *HostIdentityAnswer) Validate() error {
	for _, p := range []struct {
		field string
		part  *IdentityPart
	}{{"system_uuid", a.SystemUUID}, {"board_serial", a.BoardSerial}, {"machine_id", a.MachineID}, {"root_fs_uuid", a.RootFSUUID}} {
		if p.part == nil {
			continue
		}
		if f := p.part.validate(p.field); f != nil {
			return wireInvalid(objectHostIdentityAnswer, f)
		}
	}
	return nil
}
