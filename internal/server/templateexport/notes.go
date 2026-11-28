package templateexport

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// ErrorUnavailable reports a deployment that cannot be saved as a template.
// Params carry reason (one of the Reason constants) and, for some reasons,
// the values that decided it.
const ErrorUnavailable = messages.ErrorTemplateExportUnavailable

// Reasons a deployment cannot be exported. They are stable wire values.
const (
	ReasonNotRunning       = "not_running"
	ReasonDistributed      = "distributed"
	ReasonPD               = "pd"
	ReasonNoGPUFacts       = "no_gpu_facts"
	ReasonTemplateAssets   = "template_assets"
	ReasonComponents       = "components"
	ReasonWeightsNotPublic = "weights_not_public"
	ReasonRevisionUnpinned = "revision_unpinned"
	ReasonRevisionsDiffer  = "revisions_differ"
	ReasonTemplateMissing  = "template_missing"
	ReasonModelMissing     = "model_missing"
	ReasonLaunchChanged    = "launch_changed"
	ReasonLaunchMissing    = "launch_snapshot_missing"
	ReasonMixedGPUs        = "heterogeneous_gpus"
	// ReasonCommandImageEntry: the deployment runs the image's own
	// executable (image-entry mode, or a manual mode without a command),
	// which no spec field states.
	ReasonCommandImageEntry = "command_image_entry"
	// ReasonCommandFullArgv: the deployment replaced the engine command with
	// a complete argv; a spec states args or a shell script, not an argv.
	ReasonCommandFullArgv = "command_full_argv"
	// ReasonCommandModelMount: the deployment's manual script runs with the
	// model mounted at model_mount_path, which the spec does not carry.
	ReasonCommandModelMount = "command_model_mount"
	// ReasonCommandMount: the deployment's manual script uses the container
	// path of an extra mount (mount_path), which a spec does not carry: a
	// deployment of the spec would not have it.
	ReasonCommandMount = "command_mount"
	// ReasonCommandNotImportable: the spec the deployment's command is
	// written into is one the catalog's import refuses (a manual command
	// over its size limit, a variable no launch states, a placement that
	// cannot be pinned to the tested cards).
	ReasonCommandNotImportable = "command_not_importable"
)

// Unavailable is the error for a deployment that cannot be saved as a
// template for reason.
func Unavailable(reason string, params map[string]any) error {
	all := map[string]any{"reason": reason}
	for key, value := range params {
		all[key] = value
	}
	return messages.NewProductError(ErrorUnavailable, "the deployment cannot be saved as a template: "+reason, all)
}

// Note is one fact about an export the YAML comments and the evidence state.
// Code is stable; Params are the values a client interpolates.
type Note struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// Note codes.
const (
	// NoteVerifiedOn states the cards the spec ran on (gpu, count).
	NoteVerifiedOn = "verified_on"
	// NoteLocalWeights: the template references registered local files and
	// does not bundle or make those files downloadable on another cluster.
	NoteLocalWeights = "local_weights"
	// NoteOperationPassed and NoteOperationFailed report an operation the
	// export sent a request for (operation, http_status, message).
	NoteOperationPassed = "operation_passed"
	NoteOperationFailed = "operation_failed"
	// NoteOperationNotTested is an operation the export does not send a
	// request for, because one is slow or costly (video, speech).
	NoteOperationNotTested = "operation_not_tested"
	// NoteKVBytesMissing: the catalog has no kv_bytes_per_token for the
	// model and engine, so the spec states none (engine).
	NoteKVBytesMissing = "kv_bytes_missing"
	// NoteModelConfigMissing: the model's config.json was not available, so
	// the spec has no model_config for estimation.
	NoteModelConfigMissing = "model_config_missing"
	// NotePlacementOmitted: the spec states no placement; the cards and the
	// memory the deployment had are recorded here (gpu_count, gpu_memory_mib).
	NotePlacementOmitted = "placement_omitted"
	// NoteVRAMLimit: the deployment ran under a dGPU memory limit per card
	// (vram_limit_mib). It is the deployment's choice, not a requirement.
	NoteVRAMLimit = "vram_limit"
	// NoteEnvExcluded: user env keys left out (keys).
	NoteEnvExcluded = "env_excluded"
	// NoteParametersDropped: configuration keys no engine flag carries
	// (keys).
	NoteParametersDropped = "parameters_dropped"
	// NoteContainerOverridesOmitted: mounts and docker args are not exported.
	NoteContainerOverridesOmitted = "container_overrides_omitted"
	// NoteManualCommand: the spec runs the deployment's manual script as its
	// manual_command; its args do not reach the engine.
	NoteManualCommand = "manual_command"
	// NoteCardsPinned: the placement accepts only the card count the manual
	// script ran on (count), because the script states its parallelism.
	NoteCardsPinned = "cards_pinned"
	// NoteCommandRedacted: the deployment's command (manual script or
	// entrypoint) set a secret or a withheld env value, so the spec holds it
	// redacted and cannot be saved as it is.
	NoteCommandRedacted = "command_redacted"
	// NoteCommandEnvWithheld: the command writes out the values of
	// deployment env keys (keys) the export leaves out; they are redacted.
	NoteCommandEnvWithheld = "command_env_withheld"
	// NoteCommandSetsPlatformEnv: the command sets keys (keys) the platform
	// sets for each node itself, such as a collective network interface;
	// the values it holds are this node's.
	NoteCommandSetsPlatformEnv = "command_sets_platform_env"
	// NoteManualCommandPortUnstated: the manual script neither states the
	// port flag nor references the API port variable (flag, variable; see
	// deploy.ManualCommandStatesPort).
	NoteManualCommandPortUnstated = "manual_command_port_unstated"
)

func launchNotes(in Input, launch deploy.ExportedArgs, excluded []string, model inferencecatalog.InferenceModel) []Note {
	card := in.GPUs[0]
	notes := []Note{{Code: NoteVerifiedOn, Params: map[string]any{"gpu": card.Name, "count": in.GPUCount}}}
	if len(model.Specs) > 0 {
		for _, source := range model.Specs[0].Weights.Sources {
			if source.Provider == "local" {
				notes = append(notes, Note{Code: NoteLocalWeights, Params: map[string]any{"repo_id": source.RepoID}})
			}
		}
	}
	if len(model.Specs) > 0 && model.Specs[0].Placement == nil {
		memory := card.MemoryMiB
		if in.VRAMLimitBytes > 0 {
			memory = in.VRAMLimitBytes >> 20
		}
		notes = append(notes, Note{Code: NotePlacementOmitted, Params: map[string]any{"gpu_count": in.GPUCount, "gpu_memory_mib": memory}})
	}
	if in.VRAMLimitBytes > 0 {
		notes = append(notes, Note{Code: NoteVRAMLimit, Params: map[string]any{"vram_limit_mib": in.VRAMLimitBytes >> 20}})
	}
	if len(excluded) > 0 {
		notes = append(notes, Note{Code: NoteEnvExcluded, Params: map[string]any{"keys": excluded}})
	}
	if len(launch.Dropped) > 0 {
		notes = append(notes, Note{Code: NoteParametersDropped, Params: map[string]any{"keys": launch.Dropped}})
	}
	if in.HasContainerOverrides {
		notes = append(notes, Note{Code: NoteContainerOverridesOmitted})
	}
	if script := in.Command.ManualCommand; script != "" {
		notes = append(notes, Note{Code: NoteManualCommand})
		if !deploy.ManualCommandStatesPort(script) {
			notes = append(notes, Note{Code: NoteManualCommandPortUnstated, Params: deploy.ManualCommandPortFix()})
		}
	}
	return notes
}

func checkNotes(checks []Check) []Note {
	notes := make([]Note, 0, len(checks))
	for _, check := range checks {
		params := map[string]any{"operation": check.Operation}
		switch check.Status {
		case proto.CheckPass:
			params["http_status"] = check.HTTPStatus
			notes = append(notes, Note{Code: NoteOperationPassed, Params: params})
		case proto.CheckFail:
			params["http_status"] = check.HTTPStatus
			params["message"] = check.Message()
			if check.Evidence != "" {
				params["message"] = check.Evidence
			}
			notes = append(notes, Note{Code: NoteOperationFailed, Params: params})
		default:
			notes = append(notes, Note{Code: NoteOperationNotTested, Params: params})
		}
	}
	return notes
}
