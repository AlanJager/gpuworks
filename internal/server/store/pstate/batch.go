package pstate

import "strconv"

// StepSlot is the NoticeParam slot of the notice of a timeline's step i.
func StepSlot(i int) string { return "step." + strconv.Itoa(i) }

// OwnerKind names one owner's table of one kind.
type OwnerKind struct {
	Owner string
	Kind  string
}

// Rows are rows of pushed state of any kinds: what a TableWrite inserts and
// what a Snapshot holds.
type Rows struct {
	Availability       []AvailabilityTable
	AvailabilityRows   []AvailabilityRow
	AvailabilitySteps  []AvailabilityStep
	AvailabilityLimits []AvailabilityLimit
	FleetCapacity      []FleetCapacity
	CapacitySteps      []CapacityStep
	CapacityKeep       []CapacityKeep
	NodeHolds          []NodeHold
	Banners            []Banner
	BannerSteps        []BannerStep
	Attestations       []Attestation
	Params             []NoticeParam
}

// TableWrite replaces part of one owner's table of one kind and records the
// push it came from. It applies only if Record.Generation exceeds the stored
// generation; otherwise the whole write is skipped and reported stale.
//
// An entry is the unit a kind's rows are replaced in: a feature key
// (availability), a node id in decimal (node_holds), a banner id, an
// attestation name. fleet_capacity has the one entry "". The write deletes
// every row (notice parameters included) of DeleteEntries, or of the whole
// kind with DeleteAll, then inserts Insert, whose rows all belong to
// Record's owner and kind.
type TableWrite struct {
	Record        Owner
	DeleteAll     bool
	DeleteEntries []string
	Insert        Rows
}

// Batch is everything one writer tick commits, in one transaction.
type Batch struct {
	// Uninstall deletes every row of these owners (their overrides and
	// down records included); markers and the clock are not an owner's.
	Uninstall []string
	Tables    []TableWrite
	// Down records owners as down since the given instant; Up deletes their
	// down records.
	Down []Down
	Up   []string
	// Overrides are inserted (their ID left 0); DropOverrides are deleted.
	Overrides     []Override
	DropOverrides []int64
	Clock         *Clock
	Markers       []Marker
}

// Empty reports whether b writes nothing.
func (b *Batch) Empty() bool {
	return len(b.Uninstall) == 0 && len(b.Tables) == 0 && len(b.Down) == 0 && len(b.Up) == 0 &&
		len(b.Overrides) == 0 && len(b.DropOverrides) == 0 && b.Clock == nil && len(b.Markers) == 0
}

// Result is what a committed batch did.
type Result struct {
	// Stale are the table writes skipped because the stored generation was
	// not below theirs.
	Stale []OwnerKind
	// OverrideIDs are the ids given to Batch.Overrides, in order.
	OverrideIDs []int64
}

// Snapshot is every row of pushed state, as read at boot.
type Snapshot struct {
	Owners    []Owner
	Rows      Rows
	Down      []Down
	Overrides []Override
	// Clock is nil before a high-water mark was first written.
	Clock   *Clock
	Markers []Marker
}
