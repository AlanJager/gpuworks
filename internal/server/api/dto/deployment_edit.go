package dto

// DeploymentDraft is GET /deployments/{id}/draft: the deployment read back
// as the create request that would make it, which the deploy form fills
// itself from to edit the deployment or to clone it.
type DeploymentDraft struct {
	Deployment DeployRequest `json:"deployment"`
	// Revision identifies the configuration the draft was read from. An edit
	// sends it back as base_revision; the edit is refused when the
	// deployment's configuration has changed since.
	Revision string `json:"revision"`
	// Locked are the request fields an edit cannot change, for this
	// deployment's shape: what it is (model, template, engine, name) and, for
	// a multi-node or PD deployment, the nodes it names rank by rank. Cloning
	// changes them.
	Locked []string `json:"locked"`
}

// DeploymentEditRequest is PUT /deployments/{id}: the draft with the edit
// applied, and how the edit reaches the running instances.
type DeploymentEditRequest struct {
	Deployment   DeployRequest `json:"deployment"`
	BaseRevision string        `json:"base_revision"`
	// Apply is rolling, all or none; empty takes the plan's default_apply.
	// An edit that changes nothing a launch reads applies at once and takes
	// none.
	Apply string `json:"apply,omitempty"`
	// Start makes a stopped deployment active with the edit (can_start).
	Start bool `json:"start,omitempty"`
	// Wake withdraws the deployment's sleep order with an apply of all, so
	// the replacement instances start awake (can_wake).
	Wake bool `json:"wake,omitempty"`
}

// Layers of a deployment edit: a live change applies to the deployment at
// once; a restart change is read by a launch, so a running instance keeps
// the old value until it is replaced.
const (
	DeploymentChangeLive    = "live"
	DeploymentChangeRestart = "restart"
)

// DeploymentEditNotePartition: the edit moves the deployment between whole
// cards and a dGPU slice, or to another partition backend. The old
// generation is torn down before the new one starts, so the replicas cannot
// be replaced one at a time and the service is interrupted.
const DeploymentEditNotePartition = "partition_change"

// DeploymentChange is one field an edit changes. From and To render the
// values for display - a string as itself, any other value as its JSON, an
// absent one as "" - and are both "" for a masked field (an environment
// variable's value).
type DeploymentChange struct {
	Field  string `json:"field"`
	Layer  string `json:"layer"`
	From   string `json:"from"`
	To     string `json:"to"`
	Masked bool   `json:"masked,omitempty"`
}

// DeploymentEditPlan is what an edit does, answered by a dry run
// (PUT /deployments/{id}?dry_run=1) and with the edit itself.
type DeploymentEditPlan struct {
	Changes []DeploymentChange `json:"changes"`
	// Restart is whether any change takes a new launch.
	Restart bool `json:"restart"`
	// ApplyOptions are the ways the edit may reach the instances, in the
	// deployment's current state; DefaultApply is the one an edit that names
	// none takes.
	ApplyOptions []string `json:"apply_options"`
	DefaultApply string   `json:"default_apply"`
	// CanStart is whether the deployment is stopped and the edit may start it.
	CanStart bool `json:"can_start"`
	// CanWake is whether the deployment is under a sleep order an apply of
	// all may withdraw.
	CanWake bool `json:"can_wake"`
	// ApplyNote names why the apply options are narrower than the
	// deployment's state alone allows, empty when they are not:
	// DeploymentEditNotePartition.
	ApplyNote string `json:"apply_note,omitempty"`
	// Revision is the configuration the plan was judged against.
	Revision string `json:"revision"`
}

// DeploymentEditResult answers an edit that was written (or found nothing to
// write): its plan and the deployment afterwards.
type DeploymentEditResult struct {
	Plan       DeploymentEditPlan `json:"plan"`
	Deployment Deployment         `json:"deployment"`
}
