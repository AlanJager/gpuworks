package messages

// Why applying a Slurm cluster entry from the platform failed
// (POST /slurm/clusters/{uuid}/apply and .../remove): the platform runs the
// setup on the cluster's manager node through the cloud account's command
// channel, so each failure is something the operator can act on.
const (
	// ErrorSlurmApplyNotApplicable: the entry is not one the platform can
	// apply. Param reason is why: no_source (the entry was not made from a
	// cloud account's cluster), no_provider (no cloud account is configured),
	// not_slurm_cluster (the cloud cluster does not schedule with Slurm),
	// cluster_unknown (the last sync listed no node of the cluster and the
	// cloud's listing does not have it: check the account or sync it),
	// cluster_listing_pending (the cloud's cluster listing is still being
	// read: ask again in a moment), cluster_listing_failed (the cloud's
	// cluster listing failed or is missing a region, so the cluster cannot be
	// placed),
	// provider_unsupported (the account's driver cannot run commands on the
	// manager node) or revoked (the entry's token is revoked).
	ErrorSlurmApplyNotApplicable = "SLURM_APPLY_NOT_APPLICABLE"
	// ErrorSlurmApplyManagerUnknown: the cloud did not name the cluster's
	// manager node, so there is nowhere to run the setup.
	ErrorSlurmApplyManagerUnknown = "SLURM_APPLY_MANAGER_UNKNOWN"
	// ErrorSlurmApplyPermissionMissing: the cloud account's key may not make a
	// call the setup needs. Param permission is the action (ehpc:GetCluster,
	// ecs:RunCommand...).
	ErrorSlurmApplyPermissionMissing = "SLURM_APPLY_PERMISSION_MISSING"
	// ErrorSlurmApplyFailed: a step did not finish. Param step is which
	// (SlurmApplyStep*), reason a stable word when the script said why
	// (the setup script's precheck reasons, or server_unreachable when the
	// manager node could not fetch the script) and output the last lines the
	// manager printed. With reason server_unreachable, url is the server
	// address the Slurm manager node could not reach and kind why (dns,
	// connect, timeout, tls, http, no_curl or other).
	ErrorSlurmApplyFailed = "SLURM_APPLY_FAILED"
)

// Slurm apply steps, the step param of ErrorSlurmApplyFailed.
const (
	// SlurmApplyStepCluster: the cloud would not describe the cluster.
	SlurmApplyStepCluster = "cluster"
	// SlurmApplyStepAssistant: the cloud's command channel to the manager
	// was not usable (offline, refused).
	SlurmApplyStepAssistant = "assistant"
	// SlurmApplyStepRun: the script did not run to its end.
	SlurmApplyStepRun = "run"
	// SlurmApplyStepGres: gres.conf could not be read or written.
	SlurmApplyStepGres = "gres"
	// SlurmApplyStepSetup: the setup script failed (reason says why).
	SlurmApplyStepSetup = "setup"
	// SlurmApplyStepResume: a drained node could not be read or resumed.
	SlurmApplyStepResume = "resume"
)

// Why applying is not applicable, the reason param of
// ErrorSlurmApplyNotApplicable.
const (
	SlurmApplyReasonNoSource              = "no_source"
	SlurmApplyReasonNoProvider            = "no_provider"
	SlurmApplyReasonNotSlurmCluster       = "not_slurm_cluster"
	SlurmApplyReasonClusterUnknown        = "cluster_unknown"
	SlurmApplyReasonClusterListingPending = "cluster_listing_pending"
	SlurmApplyReasonClusterListingFailed  = "cluster_listing_failed"
	SlurmApplyReasonProviderUnsupported   = "provider_unsupported"
	SlurmApplyReasonRevoked               = "revoked"
)
