package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	serverdeploy "gpuworks.ai/gpuworks/internal/server/deploy"
)

const launchFactsUnknown = "unknown"

func (a *apiHandlers) getInstanceLaunchFacts(w http.ResponseWriter, r *http.Request) {
	instance, err := a.d.Store.InstanceByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	runtimeObservation, hasRuntimeObservation := launchFactsRuntimeObservation(instance.RuntimeObservation)
	response := dto.InstanceLaunchFacts{
		Intent:                       dto.LaunchIntentLayer{Status: launchFactsUnknown},
		Resolved:                     dto.LaunchResolvedLayer{Status: launchFactsUnknown},
		RuntimeObservationRelation:   runtimeObservationRelation(instance.State, instance.ContainerID, runtimeObservation, hasRuntimeObservation),
		RuntimeObservation:           runtimeObservation,
		RuntimeObservationReceivedAt: instance.RuntimeObservationReceivedAt,
	}
	if deployment, err := a.d.Store.GetDeployment(instance.DeploymentID); err == nil {
		env := map[string]string{}
		if strings.TrimSpace(deployment.Env) != "" {
			_ = json.Unmarshal([]byte(deployment.Env), &env)
		}
		response.Intent = dto.LaunchIntentLayer{Status: "available", Value: &dto.LaunchIntent{
			DeploymentID: deployment.ID, Name: serverdeploy.RedactLaunchText(deployment.Name), Engine: serverdeploy.RedactLaunchText(deployment.Engine),
			Image: serverdeploy.RedactLaunchText(deployment.Image), ServedName: serverdeploy.RedactLaunchText(deployment.ServedName), DesiredState: deployment.State.String(),
			Replicas: deployment.Replicas, Env: nonNilStringMap(serverdeploy.RedactLaunchEnv(env)), Sleepable: deployment.Sleepable, DesiredSleep: deployment.DesiredSleep,
			VRAMLimitBytes: deployment.VRAMLimitBytes, SMLimitPct: deployment.SMLimitPct, PartitionBackend: deployment.PartitionBackend,
			NNodes: deployment.NNodes, NodeRank: deployment.NodeRank,
		}}
	}
	if strings.TrimSpace(instance.DesiredStateJSON) != "" {
		if resolved, ok := resolvedLaunchFromDesired(instance.UUID, instance.DesiredStateJSON); ok {
			resolved = serverdeploy.RedactResolvedLaunch(resolved)
			response.Resolved = dto.LaunchResolvedLayer{Status: "available", Value: &resolved}
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func resolvedLaunchFromDesired(instanceUUID, raw string) (proto.ResolvedLaunch, bool) {
	var envelope struct {
		Launch json.RawMessage `json:"launch"`
	}
	var desired proto.InstanceDesiredState
	if json.Unmarshal([]byte(raw), &envelope) != nil || json.Unmarshal([]byte(raw), &desired) != nil {
		return proto.ResolvedLaunch{}, false
	}
	launch := strings.TrimSpace(string(envelope.Launch))
	if launch == "" || launch == "null" || !strings.HasPrefix(launch, "{") {
		return proto.ResolvedLaunch{}, false
	}
	if strings.TrimSpace(instanceUUID) == "" || desired.UUID != instanceUUID || strings.TrimSpace(desired.Image) == "" {
		return proto.ResolvedLaunch{}, false
	}
	return desired.ResolvedLaunch, true
}

func launchFactsRuntimeObservation(raw string) (proto.InstanceRuntimeObservation, bool) {
	unknown := unknownRuntimeObservation()
	if strings.TrimSpace(raw) == "" {
		return unknown, false
	}
	var observed proto.InstanceRuntimeObservation
	if json.Unmarshal([]byte(raw), &observed) != nil {
		return unknown, false
	}
	observed = serverdeploy.RedactRuntimeObservation(observed)
	if observed.Container.Status == proto.ObservationObserved {
		observed.Container.Reason = ""
		observed.Container.Entrypoint = nonNilStrings(observed.Container.Entrypoint)
		observed.Container.Cmd = nonNilStrings(observed.Container.Cmd)
		observed.Container.Args = nonNilStrings(observed.Container.Args)
		observed.Container.Env = nonNilStringMap(observed.Container.Env)
		observed.Container.Mounts = nonNilMounts(observed.Container.Mounts)
		observed.Container.Devices = nonNilStrings(observed.Container.Devices)
	} else {
		reason := observed.Container.Reason
		observed.Container = unknown.Container
		observed.Container.Reason = stableObservationReason(reason)
	}
	if observed.Image.Status == proto.ObservationObserved {
		observed.Image.Reason = ""
		observed.Image.Entrypoint = nonNilStrings(observed.Image.Entrypoint)
		observed.Image.Cmd = nonNilStrings(observed.Image.Cmd)
		observed.Image.Env = nonNilStringMap(observed.Image.Env)
		observed.Image.Volumes = nonNilStrings(observed.Image.Volumes)
	} else {
		reason := observed.Image.Reason
		observed.Image = unknown.Image
		observed.Image.Reason = stableObservationReason(reason)
	}
	if observed.Process.Status == proto.ObservationObserved {
		observed.Process.Reason = ""
		observed.Process.Argv = nonNilStrings(observed.Process.Argv)
		observed.Process.Env = nonNilStringMap(observed.Process.Env)
	} else {
		reason := observed.Process.Reason
		observed.Process = unknown.Process
		observed.Process.Reason = stableObservationReason(reason)
	}
	return observed, true
}

func stableObservationReason(reason string) string {
	switch reason {
	case proto.ObservationReasonUnsupported, proto.ObservationReasonNotRunning, proto.ObservationReasonPermission,
		proto.ObservationReasonTimeout, proto.ObservationReasonInspectFailed, proto.ObservationReasonNonLocalPID,
		proto.ObservationReasonNotCollected:
		return reason
	default:
		return proto.ObservationReasonNotCollected
	}
}

func runtimeObservationRelation(state, lifecycleID string, observation proto.InstanceRuntimeObservation, present bool) dto.RuntimeObservationRelation {
	if !present {
		return dto.RuntimeObservationRelation{Status: "unknown", Reason: proto.ObservationReasonNotCollected}
	}
	switch state {
	case proto.InstFailed, proto.InstStopped, proto.InstPendingRemoval:
		return dto.RuntimeObservationRelation{Status: "historical", Reason: "instance_terminal"}
	}
	observedID := strings.TrimSpace(observation.Container.ContainerID)
	lifecycleID = strings.TrimSpace(lifecycleID)
	if observation.Container.Status != proto.ObservationObserved || observedID == "" {
		return dto.RuntimeObservationRelation{Status: "unverified", Reason: "observation_identity_unavailable"}
	}
	if lifecycleID == "" {
		return dto.RuntimeObservationRelation{Status: "unverified", Reason: "lifecycle_identity_unavailable"}
	}
	if lifecycleID == observedID || matchingHexContainerIDs(lifecycleID, observedID) {
		return dto.RuntimeObservationRelation{Status: "current"}
	}
	if isHexContainerID(lifecycleID) && isHexContainerID(observedID) {
		return dto.RuntimeObservationRelation{Status: "historical", Reason: "container_replaced"}
	}
	return dto.RuntimeObservationRelation{Status: "unverified", Reason: "lifecycle_identity_unavailable"}
}

func matchingHexContainerIDs(left, right string) bool {
	if !isHexContainerID(left) || !isHexContainerID(right) {
		return false
	}
	shorter, longer := left, right
	if len(shorter) > len(longer) {
		shorter, longer = longer, shorter
	}
	return len(shorter) >= 12 && strings.HasPrefix(strings.ToLower(longer), strings.ToLower(shorter))
}

func isHexContainerID(value string) bool {
	if len(value) < 12 || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func unknownRuntimeObservation() proto.InstanceRuntimeObservation {
	return proto.InstanceRuntimeObservation{
		Container: proto.RuntimeContainerObservation{Status: proto.ObservationUnknown, Reason: proto.ObservationReasonNotCollected, Entrypoint: []string{}, Cmd: []string{}, Args: []string{}, Env: map[string]string{}, Mounts: []proto.RuntimeMountObservation{}, Devices: []string{}},
		Image:     proto.RuntimeImageObservation{Status: proto.ObservationUnknown, Reason: proto.ObservationReasonNotCollected, Entrypoint: []string{}, Cmd: []string{}, Env: map[string]string{}, Volumes: []string{}},
		Process:   proto.RuntimeProcessObservation{Status: proto.ObservationUnknown, Reason: proto.ObservationReasonNotCollected},
	}
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func nonNilStringMap(values map[string]string) map[string]string {
	if values == nil {
		return map[string]string{}
	}
	return values
}

func nonNilMounts(values []proto.RuntimeMountObservation) []proto.RuntimeMountObservation {
	if values == nil {
		return []proto.RuntimeMountObservation{}
	}
	return values
}
