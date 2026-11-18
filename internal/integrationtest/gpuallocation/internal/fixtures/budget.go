package fixtures

// These restate allocation contracts for expected values. Tests must not call
// the estimator or admission helpers to compute what those helpers should say.

// NVIDIASliceClaim is what an NVIDIA dGPU slice claims on its card: its quota
// plus the 512 MiB the instance holds outside it (estimate.PartitionClaimBytes).
func NVIDIASliceClaim(limit int64) int64 { return limit + GiB/2 }

// ForeignHeadroom is kept beside a known foreign holder on the 64 GiB cards
// these tests report: 5% of the card, at least 4 GiB (ADR 0068).
const ForeignHeadroom = 4 * GiB

// ForeignHolderBytes is the known memory of the foreign holder these tests
// write into a card's foreign_processes.
const ForeignHolderBytes = 16 * GiB
