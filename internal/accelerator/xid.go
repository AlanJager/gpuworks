package accelerator

// NVIDIA Xid events: what each one says about the card.
//
// An Xid is a line the driver writes to the kernel log once, when the event
// happens. It is not a state: no later reading goes back to normal on its
// own (docs/adr/0060-card-health-from-hardware-facts.md). So an Xid changes a
// card's health only where it says the card itself is broken and no state
// field already says so; it then holds until a runtime check has passed on
// the card after it. Everything else is recorded and changes nothing:
//
//   - The application's own errors (13, 31, 43, 45, 68, 69, 94 — a contained
//     memory error ends the process, not the card) leave the card healthy.
//   - Xids whose state the driver also reports are judged from that state
//     (48 by the volatile ECC counter, 63/64 by the row remapper, 154 by
//     gpu_recovery_action), so they clear when the state does.
//   - NVLink Xids (74, 144–150) are judged from the links themselves.
//
// The classes follow NVIDIA's Xid catalog ("immediate action" per Xid); an
// Xid not listed here is recorded only, rather than guessed at.

// XidFinding is the finding an Xid holds the card at until a runtime check
// passes on it after the event, or "" when the Xid changes nothing.
func XidFinding(xid int) string {
	switch xid {
	case 79: // the GPU has fallen off the bus
		return HealthRebootRequired
	case 61, 62, // internal micro-controller breakpoint / halt
		95,       // uncontained memory error
		119, 120, // GSP RPC timeout / GSP error
		140: // unrecovered ECC error
		return HealthResetRequired
	}
	return ""
}

// XidNVLink reports whether an Xid is about NVLink, which the link state and
// error counters judge.
func XidNVLink(xid int) bool {
	return xid == 74 || (xid >= 144 && xid <= 150)
}
