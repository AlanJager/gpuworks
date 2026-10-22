package api

import (
	"errors"
	"math/rand/v2"
	"net/http"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func preserveEngineModelName(req *dto.DeployRequest) {
	req.ServedName = strings.TrimSpace(req.ServedName)
	if req.ServedName == "" {
		if req.QuickParams == nil {
			req.QuickParams = make(map[string]any)
		}
		req.QuickParams["served_model_name"] = ""
	} else {
		delete(req.QuickParams, "served_model_name")
	}
}

const servedNameSuffixAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// withServedNameSuffix gives a name a random four-character suffix, the same
// shape the deploy form proposes, so a copy of a deployment does not ask for
// the served name its original holds.
func withServedNameSuffix(name string) string {
	suffix := make([]byte, 4)
	for i := range suffix {
		suffix[i] = servedNameSuffixAlphabet[rand.IntN(len(servedNameSuffixAlphabet))]
	}
	return name + "-" + string(suffix)
}

// writeDeploymentCreateErr answers a failed deployment create: a served name
// another deployment holds is the caller's to change (409), anything else is
// the server's.
func writeDeploymentCreateErr(w http.ResponseWriter, err error) {
	var inUse *store.ServedNameInUseError
	if errors.As(err, &inUse) {
		writeErr(w, http.StatusConflict, wrapProductError(ErrorServedNameInUse, err.Error(), map[string]any{
			"served_name": inUse.ServedName, "deployment_id": inUse.HolderID, "deployment": inUse.HolderName,
		}, err))
		return
	}
	writeErr(w, http.StatusInternalServerError, err)
}
