package store

// DeploymentState is the user-facing desired state of a deployment. It is the
// single enumeration of the set; the persisted column stays a plain string, so
// the wire and DB representations are unchanged.
type DeploymentState string

const (
	// DeployActive means the controller keeps the configured replica count
	// running.
	DeployActive DeploymentState = "active"
	// DeployStopped means the deployment is retained but its instances are
	// torn down; it can be started again without losing configuration.
	DeployStopped DeploymentState = "stopped"
	// DeployDeleting means the deployment is on its way out: instances are
	// being removed and the row is dropped once they are gone.
	DeployDeleting DeploymentState = "deleting"
)

// DeploymentStates returns every deployment state value. Callers validating or
// ranging over states must use it instead of rebuilding a list.
func DeploymentStates() []DeploymentState {
	return []DeploymentState{DeployActive, DeployStopped, DeployDeleting}
}

// String makes DeploymentState printable and usable where a plain string is
// required (DTO mapping, event payloads, GORM column values).
func (s DeploymentState) String() string { return string(s) }
