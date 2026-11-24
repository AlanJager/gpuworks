package app

// admissionCloseCause names why a member's business admission was closed.
// The admission journal logs it as "caller", and nothing else may appear
// there: a reader of the logs matches these values. They are listed in
// docs/explanation/control-plane-cluster.md.
type admissionCloseCause string

const (
	// The proof worker's own closes.
	closeProofCanceled    admissionCloseCause = "proof_canceled"      // an admission check found the proof in progress cancelled
	closeProofExpired     admissionCloseCause = "proof_expired"       // the last proof's admission ran out
	closeProofRunCanceled admissionCloseCause = "proof_run_canceled"  // the worker's run ended before a proof could start
	closeProofContextEnd  admissionCloseCause = "proof_context_ended" // the proof's context ended for another reason than its window
	closeProofFailed      admissionCloseCause = "proof_failed"        // a proof ended on a verdict, or without an answer for too long

	// Closes from outside the proof worker.
	closeProofStopped      admissionCloseCause = "proof_stopped"             // the proof worker was stopped
	closeMemberStopping    admissionCloseCause = "member_stopping"           // the member is stopping its closed business
	closeTransportRefused  admissionCloseCause = "transport_refresh_refused" // the transport refresh ended on a verdict
	closeOwnOriginChanged  admissionCloseCause = "own_origin_changed"        // the member's enrollment origin is not the registered one
	closeMemberAbsent      admissionCloseCause = "member_absent"             // the member is gone from the observed member list
	closeMaterialProbe     admissionCloseCause = "material_probe_rejected"   // the probe of the published manifest ended on a verdict
	closeMaterialStartup   admissionCloseCause = "material_startup_rejected" // the first replication after start ended on a verdict
	closeMaterialCheck     admissionCloseCause = "material_check_rejected"   // the periodic check of installed material ended on a verdict
	closeMaterialPass      admissionCloseCause = "material_pass_rejected"    // a replication pass ended on a verdict
	closeMaterialReceived  admissionCloseCause = "material_received"         // the material receiver took a change from another member
	closeMaterialInstalled admissionCloseCause = "material_installed"        // replication changed this member's installed material
	closeOriginRegistered  admissionCloseCause = "origin_registered"         // a member registered a changed enrollment origin
	closeWriterRefused     admissionCloseCause = "writer_refused"            // the database refused a write: the member is no longer admitted
	closeOther             admissionCloseCause = "other"                     // a cause this list does not name
)

// admissionCloseCauses is every cause, with whether the proof worker itself
// closes on it. While a member is already closed, the worker's closes are
// its failed proofs, which the journal reports on their own line.
var admissionCloseCauses = map[admissionCloseCause]bool{
	closeProofCanceled:     true,
	closeProofExpired:      true,
	closeProofRunCanceled:  true,
	closeProofContextEnd:   true,
	closeProofFailed:       true,
	closeProofStopped:      false,
	closeMemberStopping:    false,
	closeTransportRefused:  false,
	closeOwnOriginChanged:  false,
	closeMemberAbsent:      false,
	closeMaterialProbe:     false,
	closeMaterialStartup:   false,
	closeMaterialCheck:     false,
	closeMaterialPass:      false,
	closeMaterialReceived:  false,
	closeMaterialInstalled: false,
	closeOriginRegistered:  false,
	closeWriterRefused:     false,
	closeOther:             false,
}

// byProof reports a close the proof worker made itself.
func (c admissionCloseCause) byProof() bool { return admissionCloseCauses[c] }
