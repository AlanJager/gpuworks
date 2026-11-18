// Package pstate holds the rows of pushed state: the tables plugins push
// through the host API (docs/reference/plugin-protocol.md), as the core
// persists them. It is a leaf of row shapes only. Package store migrates and
// reads and writes them; package pushstate validates, compiles and enforces
// them, and is the only writer.
//
// Every column is typed (ADR 0071 decision 8): a timeline is rows of steps,
// a notice is a code column plus its parameters as typed rows of
// NoticeParam. Display-only data is not persisted: the core never shows it.
//
// Times are Unix milliseconds of extclock; 0 means "none" where a column says
// so.
package pstate

// Owner is one owner's record of one kind (pstate_owner): the push the core
// holds for it. Generation increases strictly per owner and kind.
type Owner struct {
	Owner      string `gorm:"primaryKey;size:64"`
	Kind       string `gorm:"primaryKey;size:32"`
	Schema     int    `gorm:"not null"`
	Generation int64  `gorm:"not null"`
	Digest     string `gorm:"size:80;not null"`
	PushedAtMS int64  `gorm:"not null"`
}

// TableName names the table.
func (Owner) TableName() string { return "pstate_owner" }

// AvailabilityTable is an owner's availability table header
// (pstate_availability): the down grace it pushed, in seconds, 0 for the
// core's default.
type AvailabilityTable struct {
	Owner      string `gorm:"primaryKey;size:64"`
	DownGraceS int    `gorm:"not null"`
}

// TableName names the table.
func (AvailabilityTable) TableName() string { return "pstate_availability" }

// AvailabilityRow is one feature key's row (pstate_availability_row).
type AvailabilityRow struct {
	Owner      string `gorm:"primaryKey;size:64"`
	Feature    string `gorm:"primaryKey;size:100"`
	DownAccess string `gorm:"size:16;not null"`
}

// TableName names the table.
func (AvailabilityRow) TableName() string { return "pstate_availability_row" }

// AvailabilityStep is one step of a feature key's timeline
// (pstate_availability_step), Step counting from 0. AtMS is 0 for a first
// step without a time.
type AvailabilityStep struct {
	Owner      string `gorm:"primaryKey;size:64"`
	Feature    string `gorm:"primaryKey;size:100"`
	Step       int    `gorm:"primaryKey;autoIncrement:false"`
	AtMS       int64  `gorm:"not null"`
	Access     string `gorm:"size:16;not null"`
	Link       string `gorm:"size:2048;not null"`
	NoticeCode string `gorm:"size:128;not null"`
}

// TableName names the table.
func (AvailabilityStep) TableName() string { return "pstate_availability_step" }

// AvailabilityLimit is one limit of a step (pstate_availability_limit):
// Max when Unlimited is false.
type AvailabilityLimit struct {
	Owner     string `gorm:"primaryKey;size:64"`
	Feature   string `gorm:"primaryKey;size:100"`
	Step      int    `gorm:"primaryKey;autoIncrement:false"`
	Dim       string `gorm:"primaryKey;size:64"`
	Max       int64  `gorm:"not null"`
	Unlimited bool   `gorm:"not null"`
}

// TableName names the table.
func (AvailabilityLimit) TableName() string { return "pstate_availability_limit" }

// FleetCapacity is an owner's fleet capacity table (pstate_fleet_capacity):
// the selection rule and the codes of its three notices.
type FleetCapacity struct {
	Owner          string `gorm:"primaryKey;size:64"`
	Select         string `gorm:"size:32;not null"`
	RefuseNodeCode string `gorm:"size:128;not null"`
	RefuseCardCode string `gorm:"size:128;not null"`
	HoldCode       string `gorm:"size:128;not null"`
}

// TableName names the table.
func (FleetCapacity) TableName() string { return "pstate_fleet_capacity" }

// CapacityStep is one step of the caps (pstate_fleet_capacity_step); a nil
// cap is unlimited.
type CapacityStep struct {
	Owner             string `gorm:"primaryKey;size:64"`
	Step              int    `gorm:"primaryKey;autoIncrement:false"`
	AtMS              int64  `gorm:"not null"`
	MaxNodesWithCards *int64
	MaxCards          *int64
}

// TableName names the table.
func (CapacityStep) TableName() string { return "pstate_fleet_capacity_step" }

// CapacityKeep is one node an owner's fleet capacity table keeps first
// (pstate_fleet_capacity_keep).
type CapacityKeep struct {
	Owner  string `gorm:"primaryKey;size:64"`
	NodeID int64  `gorm:"primaryKey;autoIncrement:false"`
}

// TableName names the table.
func (CapacityKeep) TableName() string { return "pstate_fleet_capacity_keep" }

// NodeHold is one node an owner holds from scheduling (pstate_node_hold).
type NodeHold struct {
	Owner  string `gorm:"primaryKey;size:64"`
	NodeID int64  `gorm:"primaryKey;autoIncrement:false"`
	Code   string `gorm:"size:128;not null"`
}

// TableName names the table.
func (NodeHold) TableName() string { return "pstate_node_hold" }

// Banner is one banner of an owner (pstate_banner).
type Banner struct {
	Owner    string `gorm:"primaryKey;size:64"`
	BannerID string `gorm:"primaryKey;size:64"`
	Audience string `gorm:"size:16;not null"`
}

// TableName names the table.
func (Banner) TableName() string { return "pstate_banner" }

// BannerStep is one step of a banner (pstate_banner_step).
type BannerStep struct {
	Owner      string `gorm:"primaryKey;size:64"`
	BannerID   string `gorm:"primaryKey;size:64"`
	Step       int    `gorm:"primaryKey;autoIncrement:false"`
	AtMS       int64  `gorm:"not null"`
	Hidden     bool   `gorm:"not null"`
	Severity   string `gorm:"size:16;not null"`
	Link       string `gorm:"size:2048;not null"`
	NoticeCode string `gorm:"size:128;not null"`
}

// TableName names the table.
func (BannerStep) TableName() string { return "pstate_banner_step" }

// Attestation is one opaque statement (pstate_attestation); Blob is the
// statement's bytes as pushed, never interpreted.
type Attestation struct {
	Owner   string `gorm:"primaryKey;size:64"`
	Name    string `gorm:"primaryKey;size:64"`
	Feature string `gorm:"size:100;not null"`
	Format  string `gorm:"size:64;not null"`
	Blob    []byte `gorm:"type:blob;not null"`
}

// TableName names the table.
func (Attestation) TableName() string { return "pstate_attestation" }

// Notice slots: where a notice sits within its entry (NoticeParam.Slot).
// A timeline step's notice uses StepSlot(i).
const (
	SlotRefuseNode = "refuse_node"
	SlotRefuseCard = "refuse_card"
	SlotHold       = "hold"
	SlotNode       = "node"
)

// NoticeParam is one parameter of a stored notice (pstate_notice_param).
// Kind and Entry name the row the notice belongs to (the feature key, the
// node id in decimal, the banner id, or "" for fleet_capacity), Slot the
// notice within it. Exactly one of Str, Int and Bool is set.
type NoticeParam struct {
	Owner string `gorm:"primaryKey;size:64"`
	Kind  string `gorm:"primaryKey;size:32"`
	Entry string `gorm:"primaryKey;size:100"`
	Slot  string `gorm:"primaryKey;size:32"`
	Key   string `gorm:"primaryKey;size:64"`
	Str   *string
	Int   *int64
	Bool  *bool
}

// TableName names the table.
func (NoticeParam) TableName() string { return "pstate_notice_param" }

// Down records since when an installed owner has been down
// (pstate_down), so a restart never renews its grace.
type Down struct {
	Owner   string `gorm:"primaryKey;size:64"`
	SinceMS int64  `gorm:"not null"`
}

// TableName names the table.
func (Down) TableName() string { return "pstate_down" }

// Override actions.
const (
	OverrideDelete = "delete"
	OverrideLoosen = "loosen"
)

// Override is an administrator's release of one restriction entry
// (pstate_override), bound to the owner's generation of the kind when it
// was made. Value is the loosened cap for OverrideLoosen.
type Override struct {
	ID         int64  `gorm:"primaryKey;autoIncrement"`
	Owner      string `gorm:"size:64;not null;index:idx_pstate_override_owner"`
	Kind       string `gorm:"size:32;not null"`
	EntryKey   string `gorm:"size:100;not null"`
	Generation int64  `gorm:"not null"`
	Action     string `gorm:"size:16;not null"`
	Value      *int64
	ByUser     uint   `gorm:"not null"`
	AtMS       int64  `gorm:"not null"`
	Reason     string `gorm:"size:1024;not null"`
}

// TableName names the table.
func (Override) TableName() string { return "pstate_override" }

// Clock is extclock's persisted high-water mark (pstate_clock, one row).
type Clock struct {
	ID    int   `gorm:"primaryKey;autoIncrement:false"`
	HWMMS int64 `gorm:"not null"`
}

// TableName names the table.
func (Clock) TableName() string { return "pstate_clock" }

// ClockID is the id of the one Clock row.
const ClockID = 1

// Marker is a version marker of persisted state (pstate_marker), compared
// with extapi.CheckMarkers so an older core refuses to start on state a
// newer core wrote.
type Marker struct {
	Name      string `gorm:"primaryKey;size:128"`
	Version   int    `gorm:"not null"`
	WrittenBy string `gorm:"size:64;not null"`
}

// TableName names the table.
func (Marker) TableName() string { return "pstate_marker" }

// Models are every table of pushed state, for the migration that creates
// them.
func Models() []any {
	return []any{
		&Owner{}, &AvailabilityTable{}, &AvailabilityRow{}, &AvailabilityStep{}, &AvailabilityLimit{},
		&FleetCapacity{}, &CapacityStep{}, &CapacityKeep{}, &NodeHold{}, &Banner{}, &BannerStep{}, &Attestation{},
		&NoticeParam{}, &Down{}, &Override{}, &Clock{}, &Marker{},
	}
}
